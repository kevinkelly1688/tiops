// @author: kevinkelly
// @license: Apache License 2.0
// @file: store.go
// @time: 2026/10/28

package notify

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const encPrefix = "enc1:"

// channel 是内存中的渠道（URL / Secret 为明文，只在进程内存与发送时使用）。
type channel struct {
	ID        string
	Type      string
	Name      string
	Enabled   bool
	URL       string
	Secret    string
	Events    []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (c *channel) subscribes(ev string) bool {
	if len(c.Events) == 0 {
		return true
	}
	for _, e := range c.Events {
		if e == ev {
			return true
		}
	}
	return false
}

// fileChannel / fileDoc 是落盘格式。url / secret 在有主密钥时为 "enc1:<base64(nonce||密文)>"。
type fileChannel struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Name      string    `json:"name"`
	Enabled   bool      `json:"enabled"`
	URL       string    `json:"url"`
	Secret    string    `json:"secret,omitempty"`
	Events    []string  `json:"events"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type fileDoc struct {
	Version  int           `json:"version"`
	Plain    bool          `json:"plain"` // true 表示没有主密钥，url / secret 为明文
	Channels []fileChannel `json:"channels"`
}

// deriveKey 由 MasterKey 经 SHA-256 派生 AES-256 密钥；MasterKey 为空返回 nil。
func deriveKey(master []byte) []byte {
	if len(master) == 0 {
		return nil
	}
	h := sha256.Sum256(master)
	return h[:]
}

// seal 用 AES-256-GCM 加密；aad 绑定渠道 ID 与字段名，防止密文被挪用到别的渠道 / 字段。
func seal(key []byte, plain, aad string) (string, error) {
	if plain == "" {
		return "", nil
	}
	if key == nil {
		return plain, nil
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	g, err := cipher.NewGCM(blk)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := g.Seal(nonce, nonce, []byte(plain), []byte(aad))
	return encPrefix + base64.StdEncoding.EncodeToString(ct), nil
}

func open(key []byte, stored, aad string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !strings.HasPrefix(stored, encPrefix) {
		return stored, nil // 明文（plain=true 的文件）
	}
	if key == nil {
		return "", errors.New("渠道文件已加密，但未提供主密钥")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encPrefix))
	if err != nil {
		return "", errors.New("渠道文件中的密文格式不正确")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	g, err := cipher.NewGCM(blk)
	if err != nil {
		return "", err
	}
	if len(raw) < g.NonceSize() {
		return "", errors.New("渠道文件中的密文过短")
	}
	pt, err := g.Open(nil, raw[:g.NonceSize()], raw[g.NonceSize():], []byte(aad))
	if err != nil {
		return "", errors.New("渠道文件解密失败（主密钥是否变更？）")
	}
	return string(pt), nil
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "ch-" + hex.EncodeToString(b)
}

// loadChannels 读取渠道文件；文件不存在返回空集合。
func loadChannels(path string, key []byte) ([]*channel, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc fileDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("渠道文件不是合法 JSON: %w", err)
	}
	var out []*channel
	for _, fc := range doc.Channels {
		u, err := open(key, fc.URL, fc.ID+"|url")
		if err != nil {
			return nil, fmt.Errorf("渠道 %s: %w", fc.ID, err)
		}
		s, err := open(key, fc.Secret, fc.ID+"|secret")
		if err != nil {
			return nil, fmt.Errorf("渠道 %s: %w", fc.ID, err)
		}
		out = append(out, &channel{ID: fc.ID, Type: fc.Type, Name: fc.Name, Enabled: fc.Enabled, URL: u, Secret: s, Events: fc.Events, CreatedAt: fc.CreatedAt, UpdatedAt: fc.UpdatedAt})
	}
	return out, nil
}

// encodeChannels 生成落盘内容。
func encodeChannels(chs []*channel, key []byte) ([]byte, error) {
	doc := fileDoc{Version: 1, Plain: key == nil, Channels: []fileChannel{}}
	for _, c := range chs {
		u, err := seal(key, c.URL, c.ID+"|url")
		if err != nil {
			return nil, err
		}
		s, err := seal(key, c.Secret, c.ID+"|secret")
		if err != nil {
			return nil, err
		}
		ev := c.Events
		if ev == nil {
			ev = []string{}
		}
		doc.Channels = append(doc.Channels, fileChannel{ID: c.ID, Type: c.Type, Name: c.Name, Enabled: c.Enabled, URL: u, Secret: s, Events: ev, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
	}
	return json.MarshalIndent(doc, "", "  ")
}

// writeFileAtomic 以 0600 原子写文件（先写临时文件再改名）。
func writeFileAtomic(path string, data []byte) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func loadLog(path string) []Delivery {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var d []Delivery
	if json.Unmarshal(b, &d) != nil {
		return nil
	}
	if len(d) > maxLog {
		d = d[:maxLog]
	}
	return d
}
