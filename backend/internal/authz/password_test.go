// @author: kevinkelly
// @license: Apache License 2.0
// @file: password_test.go
// @time: 2026/10/28

package authz

import (
	"encoding/base32"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPBKDF2Vectors(t *testing.T) {
	cases := []struct {
		pw, salt string
		iter, n  int
		want     string
	}{
		{"password", "salt", 1, 32, "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{"password", "salt", 2, 32, "ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{"password", "salt", 4096, 32, "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
		// 多块输出
		{"passwordPASSWORDpassword", "saltSALTsaltSALTsaltSALTsaltSALTsalt", 4096, 40,
			"348c89dbcbd32b2f32d814b8116e84cf2b17347ebc1800181c4e2a1fb8dd53e1c635518c7dac47e9"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(pbkdf2SHA256([]byte(c.pw), []byte(c.salt), c.iter, c.n))
		if got != c.want {
			t.Errorf("PBKDF2(%q,%q,%d,%d)\n got %s\nwant %s", c.pw, c.salt, c.iter, c.n, got, c.want)
		}
	}
}

func TestPasswordHashFormat(t *testing.T) {
	old := passwordIterations
	passwordIterations = DefaultIterations
	defer func() { passwordIterations = old }()
	if DefaultIterations < 210000 {
		t.Fatal("迭代次数应不少于 210000")
	}
	h, err := HashPassword("Correct-Horse-9")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(h, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		t.Fatalf("格式不对：%s", h)
	}
	if it, _ := strconv.Atoi(parts[1]); it < 210000 {
		t.Fatalf("迭代次数=%d", it)
	}
	if !VerifyPassword("Correct-Horse-9", h) {
		t.Fatal("正确口令应通过")
	}
	if VerifyPassword("Correct-Horse-8", h) {
		t.Fatal("错误口令不应通过")
	}
}

func TestPasswordSaltedAndVerifyRobust(t *testing.T) {
	h1, _ := HashPassword("Same-Passw0rd")
	h2, _ := HashPassword("Same-Passw0rd")
	if h1 == h2 {
		t.Fatal("每次哈希应使用不同盐")
	}
	for _, bad := range []string{"", "x", "pbkdf2-sha256$1$a$b", "md5$1000$AAAA$AAAA",
		"pbkdf2-sha256$0$AAAA$AAAA", "pbkdf2-sha256$99999999999$AAAA$AAAA", "pbkdf2-sha256$abc$AAAA$AAAA",
		"pbkdf2-sha256$1000$!!!$AAAA", "pbkdf2-sha256$1000$AAAA$"} {
		if VerifyPassword("Same-Passw0rd", bad) {
			t.Errorf("非法哈希串不应通过：%q", bad)
		}
	}
}

func TestCheckPassword(t *testing.T) {
	ok := []string{"Abcdefg-123", "abcDEF1234", "abcdef123!", "ABCDEF123!", "Tidbops-Demo-1", "密码Abc12345"}
	for _, p := range ok {
		if err := CheckPassword("someone", p); err != nil {
			t.Errorf("%q 应通过：%v", p, err)
		}
	}
	bad := map[string]string{
		"Ab1!":                    "太短",
		"Abcdefg12":               "9 位",
		"abcdefghijk":             "1 类",
		"abcdefgh123":             "2 类",
		"ABCDEFGH123":             "2 类",
		"Alice-Secret-1":          "含用户名",
		"my-ALICE-pass1":          "含用户名（大小写）",
		strings.Repeat("aA1", 50): "超长",
	}
	for p, why := range bad {
		if err := CheckPassword("alice", p); err == nil {
			t.Errorf("%q 应被拒绝（%s）", p, why)
		}
	}
}

// RFC 6238 附录 B：SHA1，secret = "12345678901234567890"，8 位码；我们取 6 位，即后 6 位。
func TestTOTPRFC6238Vectors(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	cases := []struct {
		unix int64
		code string // 8 位标准值的后 6 位
	}{
		{59, "287082"},          // 94287082
		{1111111109, "081804"},  // 07081804
		{1111111111, "050471"},  // 14050471
		{1234567890, "005924"},  // 89005924
		{2000000000, "279037"},  // 69279037
		{20000000000, "353130"}, // 65353130
	}
	for _, c := range cases {
		if got := TOTPCode(secret, time.Unix(c.unix, 0)); got != c.code {
			t.Errorf("t=%d got %s want %s", c.unix, got, c.code)
		}
	}
	// 带空格 / 小写 / 填充的密钥也能解析
	spaced := strings.ToLower(secret[:8] + " " + secret[8:] + "====")
	if TOTPCode(spaced, time.Unix(59, 0)) != "287082" {
		t.Error("密钥格式宽容性不对")
	}
	if TOTPCode("not base32 !!", time.Unix(59, 0)) != "" || TOTPCode("", time.Unix(59, 0)) != "" {
		t.Error("非法密钥应返回空串")
	}
}

func TestMatchTOTPWindow(t *testing.T) {
	secret := NewTOTPSecret()
	if len(secret) != 32 {
		t.Fatalf("密钥长度=%d", len(secret))
	}
	if s2 := NewTOTPSecret(); s2 == secret {
		t.Fatal("密钥应随机")
	}
	now := time.Unix(1_700_000_010, 0)
	step := now.Unix() / 30
	for d := int64(-1); d <= 1; d++ {
		code := TOTPCode(secret, time.Unix((step+d)*30, 0))
		if s, ok := matchTOTP(secret, code, now); !ok || s != step+d {
			t.Errorf("偏移 %d 应在窗口内：%d %v", d, s, ok)
		}
	}
	for _, d := range []int64{-2, 2} {
		code := TOTPCode(secret, time.Unix((step+d)*30, 0))
		if _, ok := matchTOTP(secret, code, now); ok {
			t.Errorf("偏移 %d 应在窗口外", d)
		}
	}
	for _, c := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := matchTOTP(secret, c, now); ok {
			t.Errorf("%q 不应通过", c)
		}
	}
}

func TestProvisioningURI(t *testing.T) {
	uri := ProvisioningURI("TiDB Ops", "alice@corp", "ABCDEF234567")
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "otpauth" || u.Host != "totp" {
		t.Fatalf("%s", uri)
	}
	if !strings.Contains(uri, "TiDB%20Ops:alice%40corp") {
		t.Fatalf("label 转义不对：%s", uri)
	}
	q := u.Query()
	if q.Get("secret") != "ABCDEF234567" || q.Get("issuer") != "TiDB Ops" || q.Get("digits") != "6" ||
		q.Get("period") != "30" || q.Get("algorithm") != "SHA1" {
		t.Fatalf("参数不对：%v", q)
	}
}
