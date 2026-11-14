// @author: kevinkelly
// @license: Apache License 2.0
// @file: store.go
// @time: 2026/10/28

package authz

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// 登录失败与锁定策略。
const (
	maxFails     = 5
	lockDuration = 10 * time.Minute
	failWindow   = 10 * time.Minute
)

// 常见错误。
var (
	ErrInvalidCredentials = errors.New("用户名或口令错误")
	ErrLocked             = errors.New("登录失败次数过多，已被临时锁定")
	ErrNotFound           = errors.New("用户不存在")
	ErrExists             = errors.New("用户已存在")
	ErrLastAdmin          = errors.New("不能删除、禁用或降级最后一个管理员")
	ErrInvalidName        = errors.New("用户名需 2–64 位，只能包含小写字母、数字及 . _ @ -，且以字母或数字开头")
	ErrTOTPBound          = errors.New("该用户已绑定 TOTP，请先重置")
	ErrTOTPNotPending     = errors.New("尚未开始绑定 TOTP")
	ErrBadOTP             = errors.New("动态验证码错误或已被使用")
)

// LockedError 表示因登录失败过多被锁定，Until 为解锁时间。errors.Is(err, ErrLocked) 为真。
type LockedError struct{ Until time.Time }

func (e *LockedError) Error() string {
	return fmt.Sprintf("登录失败次数过多，请在 %s 之后重试", e.Until.Local().Format("15:04:05"))
}
func (e *LockedError) Is(target error) bool { return target == ErrLocked }

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._@-]{1,63}$`)

func normName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// rec 是内存中的用户记录（TOTP 密钥为明文，仅落盘时加密）。
type rec struct {
	Name, Display string
	PassHash      string
	Roles         map[string][]Role
	DBs           []string
	Disabled      bool
	Created       time.Time
	Dept, Email   string
	Phone, Note   string
	LastLogin     *time.Time
	totp          string // 已启用的密钥
	pending       string // 绑定中的密钥
	lastStep      int64  // 最近一次被接受的 TOTP 步长（防重放）
}

func (r *rec) clone() *rec {
	c := *r
	c.Roles = cloneRoles(r.Roles)
	c.DBs = append([]string(nil), r.DBs...)
	return &c
}

func cloneRoles(m map[string][]Role) map[string][]Role {
	out := make(map[string][]Role, len(m))
	for k, v := range m {
		out[k] = append([]Role{}, v...)
	}
	return out
}

func (r *rec) user() User {
	return User{Name: r.Name, Display: r.Display, Roles: cloneRoles(r.Roles), DBs: append([]string{}, r.DBs...),
		Disabled: r.Disabled, HasTOTP: r.totp != "", Created: r.Created,
		Dept: r.Dept, Email: r.Email, Phone: r.Phone, Note: r.Note, LastLogin: r.LastLogin}
}

func (r *rec) isAdmin() bool {
	for _, rs := range r.Roles {
		for _, x := range rs {
			if x == RoleAdmin {
				return true
			}
		}
	}
	return false
}

// diskUser 是落盘格式。TOTPEnc 标记 TOTP / TOTPPending 是否为 AES-256-GCM 密文（否则为明文 base32）。
type diskUser struct {
	Name         string            `json:"name"`
	Display      string            `json:"display"`
	PassHash     string            `json:"passHash"`
	Roles        map[string][]Role `json:"roles"`
	DBs          []string          `json:"dbs"`
	Disabled     bool              `json:"disabled"`
	Created      time.Time         `json:"created"`
	TOTP         string            `json:"totp,omitempty"`
	TOTPPending  string            `json:"totpPending,omitempty"`
	TOTPEnc      bool              `json:"totpEnc"`
	TOTPLastStep int64             `json:"totpLastStep,omitempty"`
	Dept         string            `json:"dept,omitempty"`
	Email        string            `json:"email,omitempty"`
	Phone        string            `json:"phone,omitempty"`
	Note         string            `json:"note,omitempty"`
	LastLogin    *time.Time        `json:"lastLogin,omitempty"`
}

type diskFile struct {
	Version int        `json:"version"`
	Users   []diskUser `json:"users"`
}

type failState struct {
	count       int
	first       time.Time
	lockedUntil time.Time
}

// Store 是 JSON 文件用户库（0600，原子替换写入，并发安全）。
type Store struct {
	// Issuer 是 BeginTOTP 生成的 otpauth URI 的发行方名称。
	Issuer string

	mu    sync.Mutex
	path  string
	aead  cipher.AEAD // nil 表示 TOTP 明文存放
	recs  map[string]*rec
	fails map[string]*failState
}

// OpenStore 打开（不存在则创建）用户库。masterKey 非空时经 SHA-256 派生 AES-256-GCM 密钥加密 TOTP 密钥；
// 为空则明文存放（记录里 totpEnc=false）。若文件里已有密文而 masterKey 为空或错误，返回错误。
func OpenStore(path string, masterKey []byte) (*Store, error) {
	s := &Store{Issuer: "TiDB-Ops", path: path, recs: map[string]*rec{}, fails: map[string]*failState{}}
	if len(masterKey) > 0 {
		k := sha256.Sum256(masterKey)
		blk, err := aes.NewCipher(k[:])
		if err != nil {
			return nil, err
		}
		if s.aead, err = cipher.NewGCM(blk); err != nil {
			return nil, err
		}
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := s.save(s.recs); err != nil {
			return nil, err
		}
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f diskFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("用户库 %s 无法解析：%w", path, err)
	}
	for _, d := range f.Users {
		r := &rec{Name: d.Name, Display: d.Display, PassHash: d.PassHash, Roles: cloneRoles(d.Roles),
			DBs: append([]string{}, d.DBs...), Disabled: d.Disabled, Created: d.Created, lastStep: d.TOTPLastStep,
			Dept: d.Dept, Email: d.Email, Phone: d.Phone, Note: d.Note, LastLogin: d.LastLogin}
		if r.Roles == nil {
			r.Roles = map[string][]Role{}
		}
		if r.totp, err = s.openSecret(d.Name, d.TOTP, d.TOTPEnc); err != nil {
			return nil, err
		}
		if r.pending, err = s.openSecret(d.Name, d.TOTPPending, d.TOTPEnc); err != nil {
			return nil, err
		}
		s.recs[d.Name] = r
	}
	_ = os.Chmod(path, 0o600)
	return s, nil
}

func (s *Store) sealSecret(name, plain string) (string, error) {
	if plain == "" || s.aead == nil {
		return plain, nil
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := s.aead.Seal(nonce, nonce, []byte(plain), []byte(name)) // AAD 绑定用户名，防止密文被挪用
	return base64.StdEncoding.EncodeToString(ct), nil
}

func (s *Store) openSecret(name, stored string, enc bool) (string, error) {
	if stored == "" || !enc {
		return stored, nil
	}
	if s.aead == nil {
		return "", fmt.Errorf("用户 %s 的 TOTP 密钥已加密，但未提供 masterKey", name)
	}
	raw, err := base64.StdEncoding.DecodeString(stored)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return "", fmt.Errorf("用户 %s 的 TOTP 密文格式非法", name)
	}
	n := s.aead.NonceSize()
	pt, err := s.aead.Open(nil, raw[:n], raw[n:], []byte(name))
	if err != nil {
		return "", fmt.Errorf("用户 %s 的 TOTP 密钥解密失败（masterKey 不匹配？）", name)
	}
	return string(pt), nil
}

// save 把 recs 原子写入磁盘（临时文件 + fsync + rename）。
func (s *Store) save(recs map[string]*rec) error {
	names := make([]string, 0, len(recs))
	for n := range recs {
		names = append(names, n)
	}
	sort.Strings(names)
	f := diskFile{Version: 1, Users: make([]diskUser, 0, len(names))}
	for _, n := range names {
		r := recs[n]
		d := diskUser{Name: r.Name, Display: r.Display, PassHash: r.PassHash, Roles: r.Roles, DBs: r.DBs,
			Disabled: r.Disabled, Created: r.Created, TOTPEnc: s.aead != nil, TOTPLastStep: r.lastStep,
			Dept: r.Dept, Email: r.Email, Phone: r.Phone, Note: r.Note, LastLogin: r.LastLogin}
		var err error
		if d.TOTP, err = s.sealSecret(r.Name, r.totp); err != nil {
			return err
		}
		if d.TOTPPending, err = s.sealSecret(r.Name, r.pending); err != nil {
			return err
		}
		f.Users = append(f.Users, d)
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".users-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpName) }
	_ = tmp.Chmod(0o600)
	if _, err := tmp.Write(b); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// cloneRecs 深拷贝，供“先改副本、落盘成功再替换”的事务式修改。
func (s *Store) cloneRecs() map[string]*rec {
	m := make(map[string]*rec, len(s.recs))
	for k, v := range s.recs {
		m[k] = v.clone()
	}
	return m
}

func (s *Store) commit(recs map[string]*rec) error {
	if err := s.save(recs); err != nil {
		return err
	}
	s.recs = recs
	return nil
}

// otherAdmins 统计除 except 之外仍启用且拥有 admin 角色（任一环境）的用户数。
func otherAdmins(recs map[string]*rec, except string) int {
	n := 0
	for k, r := range recs {
		if k != except && !r.Disabled && r.isAdmin() {
			n++
		}
	}
	return n
}

func validateRoles(roles map[string][]Role) (map[string][]Role, error) {
	out := make(map[string][]Role, len(roles))
	for env, rs := range roles {
		env = strings.TrimSpace(env)
		if env == "" {
			return nil, errors.New("环境名不能为空")
		}
		seen := map[Role]bool{}
		list := []Role{}
		for _, r := range rs {
			if !r.Valid() {
				return nil, fmt.Errorf("未知角色 %q", r)
			}
			if !seen[r] {
				seen[r] = true
				list = append(list, r)
			}
		}
		out[env] = list
	}
	return out, nil
}

// Add 创建用户（口令需满足 CheckPassword）。用户名会被规整为小写。
func (s *Store) Add(name, display, password string, roles map[string][]Role, dbs []string) error {
	name = normName(name)
	if !nameRe.MatchString(name) {
		return ErrInvalidName
	}
	if err := CheckPassword(name, password); err != nil {
		return err
	}
	rl, err := validateRoles(roles)
	if err != nil {
		return err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.recs[name]; ok {
		return ErrExists
	}
	if strings.TrimSpace(display) == "" {
		display = name
	}
	recs := s.cloneRecs()
	recs[name] = &rec{Name: name, Display: display, PassHash: hash, Roles: rl,
		DBs: append([]string{}, dbs...), Created: time.Now().UTC()}
	return s.commit(recs)
}

// （带资料的创建见 profile.go 的 AddWithProfile。）

// modify 在副本上执行 fn 并落盘。
func (s *Store) modify(name string, fn func(recs map[string]*rec, r *rec) error) error {
	name = normName(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.recs[name]; !ok {
		return ErrNotFound
	}
	recs := s.cloneRecs()
	if err := fn(recs, recs[name]); err != nil {
		return err
	}
	return s.commit(recs)
}

// SetPassword 重设口令（需满足策略）。
func (s *Store) SetPassword(name, password string) error {
	name = normName(name)
	if err := CheckPassword(name, password); err != nil {
		return err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	return s.modify(name, func(_ map[string]*rec, r *rec) error { r.PassHash = hash; return nil })
}

// SetRoles 整体替换用户的角色（env→角色）。最后一个管理员不能被降级。
func (s *Store) SetRoles(name string, roles map[string][]Role) error {
	rl, err := validateRoles(roles)
	if err != nil {
		return err
	}
	return s.modify(name, func(recs map[string]*rec, r *rec) error {
		wasAdmin := !r.Disabled && r.isAdmin()
		r.Roles = rl
		if wasAdmin && !r.isAdmin() && otherAdmins(recs, r.Name) == 0 {
			return ErrLastAdmin
		}
		return nil
	})
}

// SetDBs 设置 developer 被授权的库。
func (s *Store) SetDBs(name string, dbs []string) error {
	return s.modify(name, func(_ map[string]*rec, r *rec) error { r.DBs = append([]string{}, dbs...); return nil })
}

// SetDisabled 启用 / 禁用用户。最后一个管理员不能被禁用。禁用后调用方应 Sessions.RevokeUser。
func (s *Store) SetDisabled(name string, disabled bool) error {
	return s.modify(name, func(recs map[string]*rec, r *rec) error {
		if disabled && !r.Disabled && r.isAdmin() && otherAdmins(recs, r.Name) == 0 {
			return ErrLastAdmin
		}
		r.Disabled = disabled
		return nil
	})
}

// Delete 删除用户。最后一个管理员不能被删除。删除后调用方应 Sessions.RevokeUser。
func (s *Store) Delete(name string) error {
	return s.modify(name, func(recs map[string]*rec, r *rec) error {
		if !r.Disabled && r.isAdmin() && otherAdmins(recs, r.Name) == 0 {
			return ErrLastAdmin
		}
		delete(recs, r.Name)
		return nil
	})
}

// List 返回全部用户（按用户名排序）。
func (s *Store) List() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.recs))
	for n := range s.recs {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]User, 0, len(names))
	for _, n := range names {
		out = append(out, s.recs[n].user())
	}
	return out
}

// Get 返回用户（深拷贝）。
func (s *Store) Get(name string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[normName(name)]
	if !ok {
		return User{}, false
	}
	return r.user(), true
}

// ---- 登录 ----

var (
	dummyOnce sync.Once
	dummyHash string
)

func getDummyHash() string {
	dummyOnce.Do(func() { dummyHash, _ = HashPassword("dummy-password-for-timing") })
	return dummyHash
}

func (s *Store) lockedUntil(now time.Time, keys ...string) (time.Time, bool) {
	var until time.Time
	for _, k := range keys {
		if k == "" {
			continue
		}
		if st, ok := s.fails[k]; ok && now.Before(st.lockedUntil) && st.lockedUntil.After(until) {
			until = st.lockedUntil
		}
	}
	return until, !until.IsZero()
}

func (s *Store) recordFailure(now time.Time, keys ...string) {
	if len(s.fails) > 4096 { // 清理已过期条目，防止内存被刷爆
		for k, st := range s.fails {
			if !now.Before(st.lockedUntil) && now.Sub(st.first) > failWindow {
				delete(s.fails, k)
			}
		}
	}
	for _, k := range keys {
		if k == "" {
			continue
		}
		st := s.fails[k]
		if st == nil {
			st = &failState{}
			s.fails[k] = st
		}
		if now.Before(st.lockedUntil) {
			continue
		}
		if st.count == 0 || now.Sub(st.first) > failWindow {
			st.count, st.first = 0, now
		}
		st.count++
		if st.count >= maxFails {
			st.lockedUntil = now.Add(lockDuration)
			st.count = 0
		}
	}
}

func (s *Store) clearFailure(key string) { delete(s.fails, key) }

func ipKey(ip string) string {
	if ip = strings.TrimSpace(ip); ip == "" {
		return ""
	}
	return "i:" + ip
}

// acceptOTP 校验 code 并做防重放：只接受比 lastStep 更新的步长。成功后落盘 lastStep（尽力而为）。
func (s *Store) acceptOTP(r *rec, secret, code string, now time.Time) bool {
	step, ok := matchTOTP(secret, code, now)
	if !ok || step <= r.lastStep {
		return false
	}
	r.lastStep = step
	_ = s.save(s.recs)
	return true
}

// Authenticate 校验用户名 / 口令。登录只认口令，不再校验动态码。
//
// 同一用户名或同一 IP 连续失败 5 次后锁定 10 分钟（返回 *LockedError）。失败一律返回 ErrInvalidCredentials，
// 不区分“用户不存在 / 口令错 / 已禁用”，且对不存在的用户同样做一次口令哈希，以消除计时侧信道。
// 第三个参数（原 otp）已忽略，仅为兼容调用方保留；TOTP 只用于审批 / 高风险申请的二次确认（VerifyOTP）。
// now 由调用方注入（测试用）；ip 为空则不做 IP 维度限制。
func (s *Store) Authenticate(name, password, _, ip string, now time.Time) (User, error) {
	key := normName(name)
	uk, ik := "u:"+key, ipKey(ip)

	s.mu.Lock()
	if until, locked := s.lockedUntil(now, uk, ik); locked {
		s.mu.Unlock()
		return User{}, &LockedError{Until: until}
	}
	hash := getDummyHash()
	if r, ok := s.recs[key]; ok {
		hash = r.PassHash
	}
	s.mu.Unlock()

	pwOK := VerifyPassword(password, hash) // 耗时操作放在锁外

	s.mu.Lock()
	defer s.mu.Unlock()
	fail := func() (User, error) {
		s.recordFailure(now, uk, ik)
		return User{}, ErrInvalidCredentials
	}
	r, ok := s.recs[key]
	if !ok || !pwOK || r.Disabled || r.PassHash != hash {
		return fail()
	}
	s.clearFailure(uk)
	return r.user(), nil
}

// ---- TOTP ----

// BeginTOTP 为用户生成待确认的 TOTP 密钥，返回密钥与 otpauth URI。需 ConfirmTOTP 后才生效。
func (s *Store) BeginTOTP(name string) (secret, uri string, err error) {
	secret = NewTOTPSecret()
	err = s.modify(name, func(_ map[string]*rec, r *rec) error {
		if r.totp != "" {
			return ErrTOTPBound
		}
		r.pending = secret
		uri = ProvisioningURI(s.Issuer, r.Name, secret)
		return nil
	})
	if err != nil {
		return "", "", err
	}
	return secret, uri, nil
}

// ConfirmTOTP 用认证器给出的第一个验证码确认绑定。
func (s *Store) ConfirmTOTP(name, code string, now time.Time) error {
	name = normName(name)
	ok := "o:" + name
	s.mu.Lock()
	if until, locked := s.lockedUntil(now, ok); locked {
		s.mu.Unlock()
		return &LockedError{Until: until}
	}
	s.mu.Unlock()
	var bad bool
	err := s.modify(name, func(_ map[string]*rec, r *rec) error {
		if r.pending == "" {
			return ErrTOTPNotPending
		}
		step, matched := matchTOTP(r.pending, code, now)
		if !matched {
			bad = true
			return ErrBadOTP
		}
		r.totp, r.pending, r.lastStep = r.pending, "", step
		return nil
	})
	s.mu.Lock()
	if bad {
		s.recordFailure(now, ok)
	} else if err == nil {
		s.clearFailure(ok)
	}
	s.mu.Unlock()
	return err
}

// VerifyOTP 供审批 / 高风险申请的二次确认。同一时间步长只能使用一次（防重放）；
// 连续失败 5 次后该用户的二次确认被锁定 10 分钟。用户不存在或未绑定返回 false。
func (s *Store) VerifyOTP(name, code string, now time.Time) bool {
	name = normName(name)
	k := "o:" + name
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, locked := s.lockedUntil(now, k); locked {
		return false
	}
	r, ok := s.recs[name]
	if !ok || r.totp == "" || r.Disabled {
		return false
	}
	if !s.acceptOTP(r, r.totp, code, now) {
		s.recordFailure(now, k)
		return false
	}
	s.clearFailure(k)
	return true
}

// ResetTOTP 清除用户的 TOTP 绑定（含未确认的密钥），之后可重新 BeginTOTP。
func (s *Store) ResetTOTP(name string) error {
	return s.modify(name, func(_ map[string]*rec, r *rec) error {
		r.totp, r.pending, r.lastStep = "", "", 0
		return nil
	})
}

// SetPasswordInsecure 绕过口令策略直接设置口令。仅供本机演示 / 开发用的命令行（user passwd --insecure-password）使用，
// 服务的 HTTP 接口永远不会调用它。
func (s *Store) SetPasswordInsecure(name, password string) error {
	if password == "" {
		return errors.New("口令不能为空")
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	return s.modify(normName(name), func(_ map[string]*rec, r *rec) error { r.PassHash = hash; return nil })
}
