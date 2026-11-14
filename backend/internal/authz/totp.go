// @author: kevinkelly
// @license: Apache License 2.0
// @file: totp.go
// @time: 2026/10/28

package authz

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 / 主流认证器 App 要求 HMAC-SHA1
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	totpPeriod = 30
	totpDigits = 6
	totpWindow = 1 // 校验窗口 ±1 个时间步长
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret 生成 20 字节随机密钥，返回无填充 base32 串。
func NewTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic("authz: 无法读取系统随机数：" + err.Error())
	}
	return b32.EncodeToString(b)
}

func decodeSecret(secret string) ([]byte, error) {
	s := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "=", "").Replace(secret))
	return b32.DecodeString(s)
}

// hotp 返回 RFC 4226 的 6 位动态口令。
func hotp(key []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[off])&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
	mod := uint32(1)
	for i := 0; i < totpDigits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", totpDigits, code%mod)
}

func timeStep(t time.Time) int64 { return t.Unix() / totpPeriod }

// TOTPCode 返回 t 所在 30 秒时间步长的 6 位验证码；密钥非法返回空串。
func TOTPCode(secret string, t time.Time) string {
	key, err := decodeSecret(secret)
	if err != nil || len(key) == 0 {
		return ""
	}
	return hotp(key, uint64(timeStep(t)))
}

// matchTOTP 在 now 的 ±1 个步长内寻找与 code 匹配的步长，返回该步长。所有候选都做恒定时间比较。
func matchTOTP(secret, code string, now time.Time) (step int64, ok bool) {
	key, err := decodeSecret(secret)
	if err != nil || len(key) == 0 {
		return 0, false
	}
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return 0, false
	}
	cur := timeStep(now)
	for d := int64(-totpWindow); d <= totpWindow; d++ {
		s := cur + d
		if s < 0 {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(hotp(key, uint64(s))), []byte(code)) == 1 && !ok {
			step, ok = s, true
		}
	}
	return step, ok
}

// ProvisioningURI 返回供认证器 App 扫码的 otpauth:// URI。
func ProvisioningURI(issuer, user, secret string) string {
	esc := func(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
	label := esc(user)
	if issuer != "" {
		label = esc(issuer) + ":" + esc(user)
	}
	u := "otpauth://totp/" + label + "?secret=" + esc(secret) + "&algorithm=SHA1&digits=6&period=30"
	if issuer != "" {
		u += "&issuer=" + esc(issuer)
	}
	return u
}
