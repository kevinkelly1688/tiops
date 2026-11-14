// @author: kevinkelly
// @license: Apache License 2.0
// @file: manager.go
// @time: 2026/10/28

package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 任务种类与状态。
const (
	KindPrecheck = "precheck"
	KindDeploy   = "deploy"

	StatusRunning = "running"
	StatusOK      = "ok"
	StatusFailed  = "failed"
	StatusUnknown = "unknown" // 超过 staleAfter 仍没有退出码（平台 / 主机重启过、任务被外部杀掉）
)

const (
	precheckValid  = 24 * time.Hour   // 预检结果多久内有效
	requestValid   = 1 * time.Hour    // 申请多久内要有人批准
	approvalValid  = 24 * time.Hour   // 批准后多久内可以开始（含失败后从断点继续）
	staleAfter     = 6 * time.Hour    // 没有退出码也不再认为在运行
	lockAfter      = 3                // 连续输错 root 密码的次数
	lockFor        = 10 * time.Minute // 锁定时长
	runTimeout     = 45 * time.Second // 经 su 启动 / 取消的总超时
	maxLogChunk    = 256 << 10
	tidbRootSecret = "tidb-root-password"
)

var reID = regexp.MustCompile(`^[a-z0-9-]{8,48}$`)

// 错误。
var (
	ErrLocked       = errors.New("root 密码连续输错次数过多，该账号被暂时锁定，请稍后再试")
	ErrBusy         = errors.New("已有部署任务在运行，请等它结束")
	ErrNotReady     = errors.New("条件不满足")
	ErrSelfApprove  = errors.New("申请人不能批准自己的申请，需要另一名管理员")
	ErrSecretGone   = errors.New("TiDB root 密码已经显示过一次（或没有生成）；原文件在中控机上，权限 600")
	ErrNoPassword   = errors.New("请输入中控机 root 密码")
	ErrNameMismatch = errors.New("输入的集群名与申请不一致")
)

// Job 是一次预检或部署任务。状态由磁盘上的 exit 文件推导，所以平台重启不会丢。
type Job struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Cluster   string     `json:"cluster"`
	ConfVer   int        `json:"confVer"`
	ConfSHA   string     `json:"confSha"`
	By        string     `json:"by"`
	Request   string     `json:"request,omitempty"`
	Started   time.Time  `json:"started"`
	Unit      string     `json:"unit"`
	Cancelled bool       `json:"cancelled,omitempty"`
	LaunchErr string     `json:"launchErr,omitempty"`
	Status    string     `json:"status"`
	Exit      *int       `json:"exit,omitempty"`
	Ended     *time.Time `json:"ended,omitempty"`
	Secret    bool       `json:"secret,omitempty"` // 有尚未显示的 TiDB root 密码
}

// Request 是一次部署申请（需要另一名管理员批准）。
type Request struct {
	ID         string     `json:"id"`
	Cluster    string     `json:"cluster"`
	ConfVer    int        `json:"confVer"`
	ConfSHA    string     `json:"confSha"`
	Precheck   string     `json:"precheck"`
	By         string     `json:"by"`
	At         time.Time  `json:"at"`
	ApprovedBy string     `json:"approvedBy,omitempty"`
	ApprovedAt *time.Time `json:"approvedAt,omitempty"`
	LastJob    string     `json:"lastJob,omitempty"`
	Done       bool       `json:"done,omitempty"` // 已有一次部署成功
	State      string     `json:"state"`          // pending | approved | expired | done（读取时推导）
}

// Config 是 Manager 的配置。
type Config struct {
	Dir     string     // <数据目录>/deploy
	WebDir  string     // 前端目录（里面有 deploy/dist/tidb-deploy-*.tar.gz）
	Runner  RootRunner // 取得 root 的执行器；nil = 不可用
	RunUser string     // 运行平台的系统用户（任务文件的属主，用于让它读到日志）；默认 tidbops
	WorkDir string     // tidb-deploy.sh 的工作目录（TD_WORKROOT 默认值）；用来找新集群的 root 密码文件
	Now     func() time.Time
	// NoPathCheck 仅供测试：跳过「数据目录路径只含安全字符」的检查（开发机的临时目录路径不符合 Linux 规则）。
	NoPathCheck bool
}

// Manager 管理配置、申请与任务。
type Manager struct {
	cfg   Config
	Store *Store

	mu    sync.Mutex
	fails map[string][]time.Time
}

// NewManager 创建 Manager 并准备目录。
func NewManager(cfg Config) (*Manager, error) {
	if cfg.Dir == "" {
		return nil, errors.New("deploy: 需要数据目录")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RunUser == "" {
		cfg.RunUser = "tidbops"
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = "/var/lib/tidb-deploy"
	}
	for _, d := range []string{cfg.Dir, filepath.Join(cfg.Dir, "jobs")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	return &Manager{cfg: cfg, Store: NewStore(cfg.Dir), fails: map[string][]time.Time{}}, nil
}

// Available 返回能否使用（没有 root 执行器时给出原因）。
func (m *Manager) Available() (bool, string) {
	if m.cfg.Runner == nil {
		return false, "没有可用的 root 执行器"
	}
	if _, _, err := FindToolPackage(m.cfg.WebDir); err != nil {
		return false, err.Error()
	}
	return true, ""
}

func (m *Manager) now() time.Time { return m.cfg.Now() }

func (m *Manager) jobDir(id string) string { return filepath.Join(m.cfg.Dir, "jobs", id) }

func newID(now time.Time) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "d-" + now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// ---------------------------------------------------------------- 申请 ----

func (m *Manager) reqFile() string { return filepath.Join(m.cfg.Dir, "requests.json") }

func (m *Manager) loadReqs() []Request {
	b, err := os.ReadFile(m.reqFile())
	if err != nil {
		return nil
	}
	var rs []Request
	_ = json.Unmarshal(b, &rs)
	return rs
}

func (m *Manager) saveReqs(rs []Request) error {
	b, _ := json.MarshalIndent(rs, "", "  ")
	return writeFileAtomic(m.reqFile(), b, 0o600)
}

func (m *Manager) stateOf(r Request) string {
	switch {
	case r.Done:
		return "done"
	case r.ApprovedAt != nil && m.now().Sub(*r.ApprovedAt) > approvalValid:
		return "expired"
	case r.ApprovedAt == nil && m.now().Sub(r.At) > requestValid:
		return "expired"
	case r.ApprovedAt != nil:
		return "approved"
	}
	return "pending"
}

// Requests 返回全部申请（新 → 旧，最多 50 条），带推导出的状态。
func (m *Manager) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs := append([]Request{}, m.loadReqs()...)
	for i := range rs {
		if rs[i].LastJob != "" { // 最近一次部署任务成功 = 申请完成
			if j, err := m.Job(rs[i].LastJob); err == nil && j.Kind == KindDeploy && j.Status == StatusOK {
				rs[i].Done = true
			}
		}
		rs[i].State = m.stateOf(rs[i])
	}
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].At.After(rs[j].At) })
	if len(rs) > 50 {
		rs = rs[:50]
	}
	return rs
}

// GetRequest 取一条申请。
func (m *Manager) GetRequest(id string) (Request, bool) {
	for _, r := range m.Requests() {
		if r.ID == id {
			return r, true
		}
	}
	return Request{}, false
}

// RequestDeploy 提交部署申请：必须有一次同版本、成功且未过期的预检。
func (m *Manager) RequestDeploy(user, precheckID string) (Request, error) {
	pj, err := m.Job(precheckID)
	if err != nil {
		return Request{}, err
	}
	if pj.Kind != KindPrecheck || pj.Status != StatusOK {
		return Request{}, fmt.Errorf("%w：这次预检没有通过", ErrNotReady)
	}
	if pj.Ended != nil && m.now().Sub(*pj.Ended) > precheckValid {
		return Request{}, fmt.Errorf("%w：预检已超过 %d 小时，请重新预检", ErrNotReady, int(precheckValid.Hours()))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r := Request{ID: newID(m.now()), Cluster: pj.Cluster, ConfVer: pj.ConfVer, ConfSHA: pj.ConfSHA, Precheck: pj.ID, By: user, At: m.now()}
	r.ID = "r" + r.ID[1:]
	rs := append(m.loadReqs(), r)
	if err := m.saveReqs(rs); err != nil {
		return Request{}, err
	}
	r.State = "pending"
	return r, nil
}

// Approve 由另一名管理员批准申请。
func (m *Manager) Approve(user, id string) (Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs := m.loadReqs()
	for i := range rs {
		if rs[i].ID != id {
			continue
		}
		switch st := m.stateOf(rs[i]); st {
		case "expired":
			return Request{}, fmt.Errorf("%w：申请已过期（%d 小时内要有人批准），请重新提交", ErrNotReady, int(requestValid.Hours()))
		case "approved", "done":
			return Request{}, fmt.Errorf("%w：申请已经被批准过了", ErrNotReady)
		}
		if rs[i].By == user {
			return Request{}, ErrSelfApprove
		}
		now := m.now()
		rs[i].ApprovedBy, rs[i].ApprovedAt = user, &now
		if err := m.saveReqs(rs); err != nil {
			return Request{}, err
		}
		rs[i].State = "approved"
		return rs[i], nil
	}
	return Request{}, ErrNotFound
}

// ---------------------------------------------------------------- 任务 ----

func (m *Manager) writeJob(j Job) error {
	b, _ := json.MarshalIndent(j, "", "  ")
	return writeFileAtomic(filepath.Join(m.jobDir(j.ID), "job.json"), b, 0o600)
}

// Job 读取任务并推导当前状态。
func (m *Manager) Job(id string) (Job, error) {
	if !reID.MatchString(id) {
		return Job{}, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(m.jobDir(id), "job.json"))
	if err != nil {
		return Job{}, ErrNotFound
	}
	var j Job
	if err := json.Unmarshal(b, &j); err != nil {
		return Job{}, err
	}
	m.derive(&j)
	return j, nil
}

func (m *Manager) derive(j *Job) {
	dir := m.jobDir(j.ID)
	j.Status, j.Exit, j.Ended = StatusRunning, nil, nil
	if j.LaunchErr != "" {
		j.Status = StatusFailed
		e := j.Started
		j.Ended = &e
		return
	}
	if b, err := os.ReadFile(filepath.Join(dir, "exit")); err == nil && strings.TrimSpace(string(b)) != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			j.Exit = &n
			if st, e := os.Stat(filepath.Join(dir, "exit")); e == nil {
				t := st.ModTime()
				j.Ended = &t
			}
			j.Status = StatusFailed
			if n == 0 {
				j.Status = StatusOK
			}
			if j.Kind == KindDeploy && n == 0 {
				if _, err := os.Stat(filepath.Join(dir, tidbRootSecret)); err == nil {
					j.Secret = true
				}
			}
			return
		}
	}
	if m.now().Sub(j.Started) > staleAfter {
		j.Status = StatusUnknown
	}
}

// Jobs 返回最近的任务（新 → 旧，最多 n 条）；cluster 非空时只看该集群。
func (m *Manager) Jobs(cluster string, n int) []Job {
	ents, _ := os.ReadDir(filepath.Join(m.cfg.Dir, "jobs"))
	out := []Job{}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if j, err := m.Job(e.Name()); err == nil && (cluster == "" || j.Cluster == cluster) {
			out = append(out, j)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// Running 返回正在运行的任务（没有则 false）。
func (m *Manager) Running() (Job, bool) {
	for _, j := range m.Jobs("", 20) {
		if j.Status == StatusRunning {
			return j, true
		}
	}
	return Job{}, false
}

// Log 读取输出：从字节偏移 offset 起最多 maxLogChunk 字节，返回内容与新的偏移。
func (m *Manager) Log(id string, offset int64) (string, int64, error) {
	if !reID.MatchString(id) {
		return "", 0, ErrNotFound
	}
	f, err := os.Open(filepath.Join(m.jobDir(id), "out.log"))
	if err != nil {
		return "", 0, ErrNotFound
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if offset < 0 || offset > st.Size() {
		offset = 0
	}
	n := st.Size() - offset
	if n > maxLogChunk {
		n = maxLogChunk
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, offset); err != nil && n > 0 {
		return "", 0, err
	}
	// 不要把一个多字节字符截成两半：只返回到最后一个换行为止（除非已经读到文件末尾）
	if offset+n < st.Size() {
		if i := strings.LastIndexByte(string(buf), '\n'); i >= 0 {
			buf = buf[:i+1]
		}
	}
	return string(buf), offset + int64(len(buf)), nil
}

func (m *Manager) appendLog(id, line string) {
	f, err := os.OpenFile(filepath.Join(m.jobDir(id), "out.log"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line + "\n")
}

// ---------------------------------------------------------------- 口令防暴力 ----

func (m *Manager) lockedLocked(user string) bool {
	cut := m.now().Add(-lockFor)
	var keep []time.Time
	for _, t := range m.fails[user] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	m.fails[user] = keep
	return len(keep) >= lockAfter
}

func (m *Manager) noteFail(user string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fails[user] = append(m.fails[user], m.now())
	return lockAfter - len(m.fails[user])
}

func (m *Manager) clearFails(user string) {
	m.mu.Lock()
	delete(m.fails, user)
	m.mu.Unlock()
}

// BadPasswordError 在 root 密码错误时返回，带剩余次数。
type BadPasswordError struct{ Left int }

func (e *BadPasswordError) Error() string {
	if e.Left <= 0 {
		return ErrBadPassword.Error() + "；已锁定 " + strconv.Itoa(int(lockFor.Minutes())) + " 分钟"
	}
	return fmt.Sprintf("%s（还剩 %d 次）", ErrBadPassword.Error(), e.Left)
}
func (e *BadPasswordError) Unwrap() error { return ErrBadPassword }

// ---------------------------------------------------------------- 启动 ----

// StartPrecheck 对已保存的配置版本跑只读预检（tidb-deploy.sh check）。
func (m *Manager) StartPrecheck(ctx context.Context, user, cluster string, ver int, password []byte) (Job, error) {
	v, err := m.Store.Get(cluster, ver)
	if err != nil {
		zero(password)
		return Job{}, err
	}
	return m.launch(ctx, user, KindPrecheck, v, "", password)
}

// StartDeploy 开始（或从断点继续）部署：申请必须已被批准、集群名确认一致。
func (m *Manager) StartDeploy(ctx context.Context, user, reqID, confirm string, password []byte) (Job, error) {
	r, ok := m.GetRequest(reqID)
	if !ok {
		zero(password)
		return Job{}, ErrNotFound
	}
	switch r.State {
	case "pending":
		zero(password)
		return Job{}, fmt.Errorf("%w：还没有另一名管理员批准", ErrNotReady)
	case "expired":
		zero(password)
		return Job{}, fmt.Errorf("%w：申请已过期，请重新预检并提交", ErrNotReady)
	case "done":
		zero(password)
		return Job{}, fmt.Errorf("%w：这个申请已经部署成功", ErrNotReady)
	}
	if strings.TrimSpace(confirm) != r.Cluster {
		zero(password)
		return Job{}, ErrNameMismatch
	}
	v, err := m.Store.Get(r.Cluster, r.ConfVer)
	if err != nil || v.SHA != r.ConfSHA {
		zero(password)
		return Job{}, fmt.Errorf("%w：配置版本已不存在或内容与申请时不一致", ErrNotReady)
	}
	return m.launch(ctx, user, KindDeploy, v, reqID, password)
}

func (m *Manager) launch(ctx context.Context, user, kind string, v Version, reqID string, password []byte) (Job, error) {
	defer zero(password)
	if len(password) == 0 {
		return Job{}, ErrNoPassword
	}
	if m.cfg.Runner == nil {
		return Job{}, errors.New("没有可用的 root 执行器")
	}
	m.mu.Lock()
	if m.lockedLocked(user) {
		m.mu.Unlock()
		return Job{}, ErrLocked
	}
	if rj, busy := m.Running(); busy {
		m.mu.Unlock()
		return Job{}, fmt.Errorf("%w（任务 %s，集群 %s）", ErrBusy, rj.ID, rj.Cluster)
	}
	now := m.now()
	j := Job{ID: newID(now), Kind: kind, Cluster: v.Cluster, ConfVer: v.Ver, ConfSHA: v.SHA, By: user, Request: reqID, Started: now}
	j.Unit = "tidbops-deploy-" + j.ID
	dir := m.jobDir(j.ID)
	err := m.prepare(j, dir)
	if err == nil {
		err = m.writeJob(j)
	}
	if err == nil && reqID != "" {
		rs := m.loadReqs()
		for i := range rs {
			if rs[i].ID == reqID {
				rs[i].LastJob = j.ID
			}
		}
		err = m.saveReqs(rs)
	}
	m.mu.Unlock()
	if err != nil {
		_ = os.RemoveAll(dir)
		return Job{}, err
	}

	cctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	out, rerr := m.cfg.Runner.RunAsRoot(cctx, m.startCmd(j, dir), password)
	if rerr != nil {
		if errors.Is(rerr, ErrBadPassword) {
			_ = os.RemoveAll(dir) // 没有任何命令被执行：不留任务记录
			m.mu.Lock()
			rs := m.loadReqs()
			for i := range rs {
				if rs[i].ID == reqID && rs[i].LastJob == j.ID {
					rs[i].LastJob = ""
				}
			}
			_ = m.saveReqs(rs)
			m.mu.Unlock()
			return Job{}, &BadPasswordError{Left: m.noteFail(user)}
		}
		j.LaunchErr = rerr.Error()
		_ = m.writeJob(j)
		m.appendLog(j.ID, "[平台] 启动失败："+rerr.Error())
		m.derive(&j)
		return j, rerr
	}
	m.clearFails(user)
	if out != "" {
		m.appendLog(j.ID, "[平台] "+out)
	}
	m.derive(&j)
	return j, nil
}

// prepare 准备任务目录：配置、工具包、run.sh、预建的输出与退出文件（属主是平台用户，root 往里写；平台才读得到）。
func (m *Manager) prepare(j Job, dir string) error {
	if !m.cfg.NoPathCheck && !safePath(m.cfg.Dir) {
		return fmt.Errorf("数据目录 %q 含不支持的字符（只允许字母数字和 _ . / -），无法交给 root 执行", m.cfg.Dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	pkg, _, err := FindToolPackage(m.cfg.WebDir)
	if err != nil {
		return err
	}
	if _, err := ExtractTool(pkg, filepath.Join(dir, "tool")); err != nil {
		return err
	}
	conf, err := os.ReadFile(m.Store.ConfPath(j.Cluster, j.ConfVer))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy.conf"), conf, 0o600); err != nil {
		return err
	}
	for _, f := range []string{"out.log", "exit"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, "run.sh"), []byte(m.runScript(j, dir)), 0o700)
}

func (m *Manager) runScript(j Job, dir string) string {
	args := "check --config \"$JOB/deploy.conf\""
	if j.Kind == KindDeploy {
		args = "deploy --config \"$JOB/deploy.conf\" --yes"
	}
	secret := ""
	if j.Kind == KindDeploy {
		secret = fmt.Sprintf(`if [ $rc -eq 0 ] && [ -f '%[1]s/%[2]s/root-password.txt' ]; then
  cp '%[1]s/%[2]s/root-password.txt' "$JOB/%[3]s" && chown '%[4]s' "$JOB/%[3]s" 2>/dev/null; chmod 600 "$JOB/%[3]s"
fi
`, m.cfg.WorkDir, j.Cluster, tidbRootSecret, m.cfg.RunUser)
	}
	return fmt.Sprintf(`#!/bin/bash
# 由平台生成：以 root 身份（transient systemd 单元，不受平台服务的沙箱限制）运行 tidb-deploy.sh
JOB='%s'
exec >>"$JOB/out.log" 2>&1
umask 077
echo "[平台] 任务 %s 开始：tidb-deploy.sh %s（root，$(date '+%%F %%T')）"
cd "$JOB/tool" || { echo "[平台] 找不到工具目录"; printf '1' > "$JOB/exit"; exit 1; }
./tidb-deploy.sh %s
rc=$?
%secho "[平台] 任务结束，退出码 $rc（$(date '+%%F %%T')）"
printf '%%s' "$rc" > "$JOB/exit"
exit $rc
`, dir, j.ID, j.Kind, args, secret)
}

// startCmd 是交给 su -c 的命令：用 systemd-run 起一个临时单元（在 systemd 主进程的命名空间里，不继承平台服务的沙箱）。
func (m *Manager) startCmd(j Job, dir string) string {
	return fmt.Sprintf("systemd-run --unit=%s --description='tidbops %s %s' /bin/bash %s/run.sh", j.Unit, j.Kind, j.Cluster, filepath.ToSlash(dir))
}

// Cancel 取消运行中的任务（systemctl stop 该单元；需要 root 密码）。
func (m *Manager) Cancel(ctx context.Context, user, id string, password []byte) (Job, error) {
	defer zero(password)
	j, err := m.Job(id)
	if err != nil {
		return Job{}, err
	}
	if j.Status != StatusRunning {
		return j, fmt.Errorf("%w：任务不在运行", ErrNotReady)
	}
	if len(password) == 0 {
		return Job{}, ErrNoPassword
	}
	m.mu.Lock()
	locked := m.lockedLocked(user)
	m.mu.Unlock()
	if locked {
		return Job{}, ErrLocked
	}
	cctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	_, rerr := m.cfg.Runner.RunAsRoot(cctx, "systemctl stop "+j.Unit, password)
	if rerr != nil {
		if errors.Is(rerr, ErrBadPassword) {
			return Job{}, &BadPasswordError{Left: m.noteFail(user)}
		}
		return Job{}, rerr
	}
	m.clearFails(user)
	j.Cancelled = true
	_ = m.writeJob(j)
	m.appendLog(j.ID, "[平台] 已由 "+user+" 取消（systemctl stop "+j.Unit+"）；已完成的阶段保留，可从断点继续")
	_ = os.WriteFile(filepath.Join(m.jobDir(j.ID), "exit"), []byte("143"), 0o600)
	return m.mustJob(j.ID), nil
}

func (m *Manager) mustJob(id string) Job {
	j, _ := m.Job(id)
	return j
}

// RevealSecret 返回新集群的 TiDB root 随机密码，并立即删除平台这一份（只能取一次；中控机上的 600 文件不动）。
func (m *Manager) RevealSecret(id string) (string, error) {
	j, err := m.Job(id)
	if err != nil {
		return "", err
	}
	p := filepath.Join(m.jobDir(j.ID), tidbRootSecret)
	b, err := os.ReadFile(p)
	if err != nil {
		return "", ErrSecretGone
	}
	if err := os.Remove(p); err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
