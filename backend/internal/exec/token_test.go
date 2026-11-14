// @author: kevinkelly
// @license: Apache License 2.0
// @file: token_test.go
// @time: 2026/10/28

package exec

import (
	"context"
	"strings"
	"testing"
	"time"
)

func tokCmd() Cmd {
	return Cmd{Kind: KindClusterRestart, Cluster: testCluster, Nodes: []string{"10.0.2.11:20160"}}
}

func TestTokenRoundTrip(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	exp := now.Add(5 * time.Minute)
	tok := Sign(testKey, tokCmd(), "CHG-100", exp)
	if !strings.HasPrefix(tok, "v1.1700000300.") {
		t.Fatalf("令牌格式: %s", tok)
	}
	if parts := strings.Split(tok, "."); len(parts) != 3 {
		t.Fatalf("令牌应为 v1.<exp>.<mac>: %s", tok)
	}
	if err := Verify(testKey, tokCmd(), "CHG-100", tok, now); err != nil {
		t.Fatalf("合法令牌被拒绝: %v", err)
	}
	// 临界：过期前 1 秒有效，到期时刻起无效。
	if err := Verify(testKey, tokCmd(), "CHG-100", tok, exp.Add(-time.Second)); err != nil {
		t.Errorf("到期前应有效: %v", err)
	}
	if err := Verify(testKey, tokCmd(), "CHG-100", tok, exp); err == nil {
		t.Error("到期时刻应无效")
	}
	if err := Verify(testKey, tokCmd(), "CHG-100", tok, exp.Add(time.Hour)); err == nil {
		t.Error("过期令牌应拒绝")
	}
	// 确定性：同样输入得到同样令牌。
	if Sign(testKey, tokCmd(), "CHG-100", exp) != tok {
		t.Error("Sign 应是确定性的")
	}
}

func TestTokenBindsCommand(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	exp := now.Add(time.Hour)
	base := Cmd{Kind: KindSQLSetConfig, Cluster: testCluster, Role: "tikv", Key: "log.level", Value: "info", Nodes: []string{"a:1"}, Content: "c", Name: "n", Store: 1, Rate: 1, Instance: "i"}
	tok := Sign(testKey, base, "CHG-1", exp)
	if err := Verify(testKey, base, "CHG-1", tok, now); err != nil {
		t.Fatal(err)
	}
	muts := map[string]func(*Cmd){
		"Kind":     func(c *Cmd) { c.Kind = KindSQLSetGlobal },
		"Cluster":  func(c *Cmd) { c.Cluster = "other" },
		"Nodes":    func(c *Cmd) { c.Nodes = []string{"a:2"} },
		"Nodes+":   func(c *Cmd) { c.Nodes = []string{"a:1", "b:1"} },
		"NodesDel": func(c *Cmd) { c.Nodes = nil },
		"Role":     func(c *Cmd) { c.Role = "tidb" },
		"Store":    func(c *Cmd) { c.Store = 2 },
		"Name":     func(c *Cmd) { c.Name = "m" },
		"Key":      func(c *Cmd) { c.Key = "gc.level" },
		"Value":    func(c *Cmd) { c.Value = "debug" },
		"Instance": func(c *Cmd) { c.Instance = "j" },
		"Rate":     func(c *Cmd) { c.Rate = 2 },
		"Content":  func(c *Cmd) { c.Content = "c2" },
	}
	for name, mut := range muts {
		c := base
		c.Nodes = append([]string(nil), base.Nodes...)
		mut(&c)
		if err := Verify(testKey, c, "CHG-1", tok, now); err == nil {
			t.Errorf("篡改 %s 后令牌仍有效", name)
		}
	}
	// 同一令牌不能挪给别的变更单。
	if err := Verify(testKey, base, "CHG-2", tok, now); err == nil {
		t.Error("篡改 changeID 后令牌仍有效")
	}
	if err := Verify(testKey, base, "chg-1", tok, now); err == nil {
		t.Error("changeID 区分大小写")
	}
	// 换密钥。
	if err := Verify([]byte("another-key-another-key-another!"), base, "CHG-1", tok, now); err == nil {
		t.Error("不同密钥签发的令牌应无效")
	}
}

func TestTokenTamperExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tok := Sign(testKey, tokCmd(), "CHG-1", now.Add(time.Minute))
	parts := strings.Split(tok, ".")
	// 把过期时间改长（MAC 不匹配）。
	forged := "v1.1900000000." + parts[2]
	if err := Verify(testKey, tokCmd(), "CHG-1", forged, now); err == nil {
		t.Error("篡改过期时间后令牌仍有效")
	}
	// 篡改 MAC 末位。
	mac := []byte(parts[2])
	if mac[len(mac)-1] == 'A' {
		mac[len(mac)-1] = 'B'
	} else {
		mac[len(mac)-1] = 'A'
	}
	if err := Verify(testKey, tokCmd(), "CHG-1", "v1."+parts[1]+"."+string(mac), now); err == nil {
		t.Error("篡改 MAC 后令牌仍有效")
	}
}

func TestTokenMalformed(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	good := Sign(testKey, tokCmd(), "CHG-1", now.Add(time.Minute))
	mac := strings.Split(good, ".")[2]
	for _, tok := range []string{
		"", "v1", "v1.", "v1..", "v1.1700000060", "v1.1700000060.", "v2.1700000060." + mac, "V1.1700000060." + mac,
		"v1.abc." + mac, "v1.+1700000060." + mac, "v1.01700000060." + mac, "v1.-5." + mac, "v1.0." + mac, "v1.1700000060.!!!", "v1.1700000060." + mac + ".x", ".1700000060." + mac,
		"v1.1700000060." + mac + mac, strings.Repeat("a", 300), "v1.99999999999999999999." + mac, " " + good, good + " ", good + "\n",
		"v1.1700000060.%%%%", "v1 1700000060 " + mac,
	} {
		if err := Verify(testKey, tokCmd(), "CHG-1", tok, now); err == nil {
			t.Errorf("畸形令牌 %q 应拒绝", truncate(tok, 40))
		}
	}
	// 空令牌被明确拒绝。
	if err := Verify(testKey, tokCmd(), "CHG-1", "", now); err == nil || !strings.Contains(err.Error(), "令牌") {
		t.Errorf("空令牌: %v", err)
	}
}

func TestTokenKeyAndChangeIDRules(t *testing.T) {
	exp := time.Unix(1_700_000_300, 0)
	now := time.Unix(1_700_000_000, 0)
	if Sign([]byte("short"), tokCmd(), "CHG-1", exp) != "" || Sign(nil, tokCmd(), "CHG-1", exp) != "" {
		t.Error("过短 / 空密钥不得签发")
	}
	for _, id := range []string{"", "a b", "a\nb", "a;b", "-x", strings.Repeat("a", 129), "中文", "a\x00"} {
		if Sign(testKey, tokCmd(), id, exp) != "" {
			t.Errorf("changeID %q 不得签发", id)
		}
		good := Sign(testKey, tokCmd(), "CHG-1", exp)
		if err := Verify(testKey, tokCmd(), id, good, now); err == nil {
			t.Errorf("changeID %q 不得通过验证", id)
		}
	}
	good := Sign(testKey, tokCmd(), "CHG-1", exp)
	if err := Verify(nil, tokCmd(), "CHG-1", good, now); err == nil {
		t.Error("空密钥不得验证通过")
	}
	if err := Verify([]byte("short"), tokCmd(), "CHG-1", good, now); err == nil {
		t.Error("过短密钥不得验证通过")
	}
}

// 写命令必须带令牌；读命令无需令牌（经 SimRunner 走同一段 authorize）。
func TestWriteNeedsTokenReadDoesNot(t *testing.T) {
	sim := &SimRunner{Pol: testPolicy(""), Topo: StaticTopo(testTopo()), Key: testKey}
	ctx := context.Background()
	read := Request{Cmd: Cmd{Kind: KindClusterDisplay, Cluster: testCluster}}
	if _, err := sim.Run(ctx, read, nil); err != nil {
		t.Errorf("读命令无需令牌: %v", err)
	}
	write := Request{Cmd: tokCmd(), ChangeID: "CHG-1"}
	res, err := sim.Run(ctx, write, nil)
	if err == nil || res.Exit != -1 {
		t.Errorf("无令牌的写命令必须被拒绝: %+v %v", res, err)
	}
	write.Token = "v1.9999999999.AAAA"
	if _, err := sim.Run(ctx, write, nil); err == nil {
		t.Error("伪造令牌必须被拒绝")
	}
	if len(sim.Calls()) != 1 {
		t.Errorf("被拒绝的请求不得被执行: %d", len(sim.Calls()))
	}
}
