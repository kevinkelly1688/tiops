// @author: kevinkelly
// @license: Apache License 2.0
// @file: password.go
// @time: 2026/10/28

package authz

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DefaultIterations 是 PBKDF2 的默认迭代次数（OWASP 对 HMAC-SHA256 的建议下限 210000）。
const DefaultIterations = 210000

const (
	saltLen    = 16
	keyLen     = 32
	maxIter    = 10_000_000 // 校验时接受的迭代上限，防止被篡改的哈希串造成 CPU 耗尽
	minPwdLen  = 10
	maxPwdLen  = 128
	hashPrefix = "pbkdf2-sha256"
)

// passwordIterations 仅供包内测试调低以加速；生产恒为 DefaultIterations。
var passwordIterations = DefaultIterations

// pbkdf2SHA256 实现 RFC 8018 的 PBKDF2，PRF 为 HMAC-SHA256。
func pbkdf2SHA256(password, salt []byte, iter, dkLen int) []byte {
	prf := hmac.New(sha256.New, password)
	const hLen = sha256.Size
	blocks := (dkLen + hLen - 1) / hLen
	dk := make([]byte, 0, blocks*hLen)
	u := make([]byte, 0, hLen)
	t := make([]byte, hLen)
	var ib [4]byte
	for b := 1; b <= blocks; b++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(ib[:], uint32(b))
		prf.Write(ib[:])
		u = prf.Sum(u[:0])
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:dkLen]
}

// HashPassword 返回 `pbkdf2-sha256$iter$salt$hash`（salt、hash 为无填充标准 base64）。
func HashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk := pbkdf2SHA256([]byte(pw), salt, passwordIterations, keyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("%s$%d$%s$%s", hashPrefix, passwordIterations, enc.EncodeToString(salt), enc.EncodeToString(dk)), nil
}

// VerifyPassword 以恒定时间比较口令与哈希串；格式非法返回 false。
func VerifyPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != hashPrefix {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > maxIter {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got := pbkdf2SHA256([]byte(pw), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// CheckPassword 校验口令策略：至少 10 位、至少包含 3 类字符（小写 / 大写 / 数字 / 其他）、不得包含用户名
// （不区分大小写）。另限制最长 128 位。
func CheckPassword(name, pw string) error {
	n := utf8.RuneCountInString(pw)
	if n < minPwdLen {
		return fmt.Errorf("口令至少 %d 位", minPwdLen)
	}
	if n > maxPwdLen {
		return fmt.Errorf("口令最多 %d 位", maxPwdLen)
	}
	var lower, upper, digit, other bool
	for _, r := range pw {
		switch {
		case unicode.IsLower(r):
			lower = true
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsDigit(r):
			digit = true
		default:
			other = true
		}
	}
	classes := 0
	for _, b := range []bool{lower, upper, digit, other} {
		if b {
			classes++
		}
	}
	if classes < 3 {
		return errors.New("口令需至少包含大写字母、小写字母、数字、符号中的 3 类")
	}
	if nm := strings.ToLower(strings.TrimSpace(name)); nm != "" && strings.Contains(strings.ToLower(pw), nm) {
		return errors.New("口令不得包含用户名")
	}
	return nil
}
