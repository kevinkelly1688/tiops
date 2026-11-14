// @author: kevinkelly
// @license: Apache License 2.0
// @file: token.go
// @time: 2026/10/28

package exec

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 审批令牌（A3 §3.2）：平台在审批通过后为「具体的一条命令」签发短期令牌，exec 入口独立验证。
// 令牌绑定：规范化命令摘要（含 Content）+ changeID + 过期时间，用 HMAC-SHA256。
// 因此即使 Web 进程被攻破，攻击者也不能改动命令、挪用别的变更的令牌，或使用过期的令牌；
// 而没有密钥就无法签发新令牌。
//
// 格式：v1.<expUnix>.<base64url(mac)>

const (
	tokenVersion = "v1"
	// MinKeyLen 是令牌密钥的最小长度（字节）。更短的密钥拒绝签发与验证。
	MinKeyLen = 16
)

func tokenMAC(key []byte, c Cmd, changeID string, expUnix int64) []byte {
	digest := sha256.Sum256(MarshalCanonical(c))
	msg := fmt.Sprintf("tidbops-exec-token|%s|%d:%s|%d|%s", tokenVersion, len(changeID), changeID, expUnix, hex.EncodeToString(digest[:]))
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// Sign 签发令牌。密钥短于 MinKeyLen 或 changeID 不合法时返回空串（空串不会通过 Verify）。
func Sign(key []byte, c Cmd, changeID string, exp time.Time) string {
	if len(key) < MinKeyLen || !changeIDRe.MatchString(changeID) {
		return ""
	}
	e := exp.Unix()
	return tokenVersion + "." + strconv.FormatInt(e, 10) + "." + base64.RawURLEncoding.EncodeToString(tokenMAC(key, c, changeID, e))
}

// Verify 验证令牌：格式、MAC（常量时间比较）、过期时间。命令或 changeID 与签发时不一致一律拒绝。
func Verify(key []byte, c Cmd, changeID, token string, now time.Time) error {
	if len(key) < MinKeyLen {
		return fmt.Errorf("未配置令牌密钥")
	}
	if token == "" {
		return fmt.Errorf("缺少审批令牌")
	}
	if !changeIDRe.MatchString(changeID) {
		return fmt.Errorf("变更单号不合法")
	}
	if len(token) > 256 {
		return fmt.Errorf("令牌格式错误")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != tokenVersion {
		return fmt.Errorf("令牌格式错误")
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || exp <= 0 || strconv.FormatInt(exp, 10) != parts[1] {
		return fmt.Errorf("令牌格式错误")
	}
	mac, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || base64.RawURLEncoding.EncodeToString(mac) != parts[2] { // Go 的解码器会忽略换行，必须要求规范编码
		return fmt.Errorf("令牌格式错误")
	}
	if !hmac.Equal(mac, tokenMAC(key, c, changeID, exp)) {
		return fmt.Errorf("令牌与命令不匹配")
	}
	if now.Unix() >= exp {
		return fmt.Errorf("令牌已过期")
	}
	return nil
}
