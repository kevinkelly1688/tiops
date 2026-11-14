// @author: kevinkelly
// @license: Apache License 2.0
// @file: crypto.go
// @time: 2026/10/28

package notify

import (
	"errors"
	"strings"
)

// SealSecret 用主密钥（AES-256-GCM，密钥由 SHA-256 派生）加密一段机密文本，返回 "enc1:<base64(nonce||密文)>"。
// aad 绑定用途与对象（例如 "cluster|stg|mysqlPassword"），防止密文被挪用到别处。
// 与渠道文件使用同一套实现；与内部的 seal 不同，主密钥为空时返回错误而不是明文（调用方不应在无密钥时落盘机密）。
// plain 为空返回空串。
func SealSecret(master []byte, plain, aad string) (string, error) {
	if plain == "" {
		return "", nil
	}
	k := deriveKey(master)
	if k == nil {
		return "", errors.New("没有主密钥，拒绝以明文保存机密信息")
	}
	return seal(k, plain, aad)
}

// OpenSecret 解密 SealSecret 的输出。stored 为空返回空串；不是 "enc1:" 开头的内容一律拒绝（不接受明文）。
func OpenSecret(master []byte, stored, aad string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !strings.HasPrefix(stored, encPrefix) {
		return "", errors.New("密文格式不正确")
	}
	return open(deriveKey(master), stored, aad)
}
