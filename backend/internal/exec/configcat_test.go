// @author: kevinkelly
// @license: Apache License 2.0
// @file: configcat_test.go
// @time: 2026/10/28

package exec

import (
	"strings"
	"testing"
)

func item(t *testing.T, role, key string) ConfigItem {
	t.Helper()
	it, ok := Lookup(role, key)
	if !ok {
		t.Fatalf("目录里没有 %s/%s", role, key)
	}
	return it
}

func TestCatalogParityWithCatalogJS(t *testing.T) {
	// 与 D:\tidb\catalog.js 的 CONFIG_CATALOG 条目数一致。
	want := map[string]int{"tikv": 9, "tidb": 8, "pd": 5, "tiflash": 3, "ticdc": 3}
	cat := Catalog()
	for role, n := range want {
		if len(cat[role]) != n {
			t.Errorf("%s 配置项 %d 个，期望 %d", role, len(cat[role]), n)
		}
		for _, it := range cat[role] {
			if it.Role != role || it.Label == "" || it.Type == "" || it.Scope == "" {
				t.Errorf("%s/%s 字段不全: %+v", role, it.Key, it)
			}
		}
	}
	it := item(t, "pd", "replication.max-replicas")
	if !it.Danger || !it.Online || it.Scope != "cluster" || it.Min != 1 || it.Max != 7 {
		t.Errorf("max-replicas = %+v", it)
	}
	it = item(t, "tidb", "tidb_mem_quota_query")
	if !it.Sys || it.Type != TypeSize || it.Scope != "cluster" {
		t.Errorf("tidb_mem_quota_query = %+v", it)
	}
	if it := item(t, "tikv", "server.grpc-concurrency"); it.Online {
		t.Error("grpc-concurrency 需重启生效")
	}
	if it := item(t, "tikv", "storage.block-cache.capacity"); !it.Online || it.Scope != "instance" || it.Sys {
		t.Errorf("block cache = %+v", it)
	}
}

func TestLookup(t *testing.T) {
	if _, ok := Lookup("cdc", "gc-ttl"); !ok {
		t.Error("cdc 应是 ticdc 的别名")
	}
	if _, ok := Lookup("ticdc", "gc-ttl"); !ok {
		t.Error("ticdc/gc-ttl")
	}
	for _, c := range [][2]string{{"tikv", "nope"}, {"nope", "log.level"}, {"pd", "storage.block-cache.capacity"}, {"tikv", ""}, {"", ""}, {"tikv", "LOG.LEVEL"}} {
		if _, ok := Lookup(c[0], c[1]); ok {
			t.Errorf("Lookup(%q,%q) 不应命中", c[0], c[1])
		}
	}
	// 返回副本：改动不影响目录。
	it := item(t, "tikv", "log.level")
	it.Opts[0] = "evil"
	if again := item(t, "tikv", "log.level"); again.Opts[0] != "debug" {
		t.Error("Lookup 返回的 Opts 必须是副本")
	}
	// log.level 在各角色里是不同的条目。
	if item(t, "tiflash", "logger.level").Opts[2] != "warning" {
		t.Error("tiflash 日志级别含 warning")
	}
}

func TestPDShortKey(t *testing.T) {
	for in, want := range map[string]string{
		"schedule.max-store-down-time": "max-store-down-time",
		"replication.max-replicas":     "max-replicas",
		"plain":                        "plain",
		"a.b.c":                        "c",
	} {
		if got := PDShortKey(in); got != want {
			t.Errorf("PDShortKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalize(t *testing.T) {
	size := item(t, "tikv", "storage.block-cache.capacity")
	dur := item(t, "pd", "schedule.max-store-down-time")
	num := item(t, "tikv", "raftstore.store-pool-size")
	boolean := item(t, "tidb", "tidb_enable_prepared_plan_cache")
	enum := item(t, "tidb", "tidb_replica_read")
	cases := []struct {
		it       ConfigItem
		in, want string
	}{
		{size, "16GB", "16GB"}, {size, "16gb", "16GB"}, {size, "512MB", "512MB"}, {size, "512mb", "512MB"}, {size, "1TB", "1TB"},
		{size, "16GiB", "16GB"}, {size, "1024KB", "1024KB"}, {size, "0GB", "0GB"}, {size, "007GB", "7GB"}, {size, "100B", "100B"},
		{dur, "30m", "30m"}, {dur, "2h", "2h"}, {dur, "90s", "90s"}, {dur, "90m", "90m"}, {dur, "1h30m", "90m"}, {dur, "60s", "1m"},
		{dur, "120m", "2h"}, {dur, "1h1m1s", "3661s"}, {dur, "0s", "0s"}, {dur, "3600s", "1h"},
		{num, "1", "1"}, {num, "16", "16"}, {num, "007", "7"},
		{boolean, "on", "ON"}, {boolean, "OFF", "OFF"}, {boolean, "true", "ON"}, {boolean, "False", "OFF"}, {boolean, "1", "ON"}, {boolean, "0", "OFF"}, {boolean, "yes", "ON"}, {boolean, "no", "OFF"},
		{enum, "leader", "leader"}, {enum, "LEADER", "leader"}, {enum, "Closest-Replicas", "closest-replicas"},
	}
	for _, tc := range cases {
		got, err := Normalize(tc.it, tc.in)
		if err != nil || got != tc.want {
			t.Errorf("Normalize(%s, %q) = %q, %v; want %q", tc.it.Key, tc.in, got, err, tc.want)
			continue
		}
		// 幂等
		if again, err := Normalize(tc.it, got); err != nil || again != got {
			t.Errorf("Normalize 不幂等: %q → %q → %q (%v)", tc.in, got, again, err)
		}
	}
}

func TestNormalizeRejects(t *testing.T) {
	size := item(t, "tikv", "storage.block-cache.capacity")
	dur := item(t, "pd", "schedule.max-store-down-time")
	num := item(t, "tikv", "raftstore.store-pool-size")
	boolean := item(t, "tidb", "tidb_enable_prepared_plan_cache")
	enum := item(t, "tikv", "log.level")
	bad := map[string]struct {
		it   ConfigItem
		vals []string
	}{
		"size":     {size, []string{"", "16", "16G", "16 GB", " 16GB", "16GB ", "-16GB", "+16GB", "16.5GB", "1e3GB", "0x10GB", "16GB;", "16GB'", `16GB"`, `16GB\`, "16GB\n", "16GB`id`", "$(id)GB", "16PB", "GB", "99999999GB", "99999TB", strings.Repeat("9", 70) + "GB", "١٦GB", "16GB\x00"}},
		"duration": {dur, []string{"", "30", "30M", "30min", "-30m", "30m ", "1.5h", "1m1h", "1h1h", "1h1m1s1s", "m", "999999999h", "1h;", "30m'", "30m\n", "30m`id`", "٣٠m", "1d"}},
		"int":      {num, []string{"", "0", "17", "-1", "+3", "3.0", "3e0", "0x3", " 3", "3 ", "3;", "3'", "3\n", "99999999999", "abc", "٣", "3 OR 1"}},
		"bool":     {boolean, []string{"", "2", "maybe", "ON;", "ON'", "on off", "enable", "t", "y"}},
		"enum":     {enum, []string{"", "trace", "info'", "info;", "inf", "info ", `"info"`, "info\n", "debug,info", "*"}},
	}
	for name, b := range bad {
		for _, v := range b.vals {
			if got, err := Normalize(b.it, v); err == nil {
				t.Errorf("%s: Normalize(%q) 应拒绝，却得到 %q", name, v, got)
			}
		}
	}
	// 范围
	if _, err := Normalize(item(t, "pd", "replication.max-replicas"), "8"); err == nil {
		t.Error("max-replicas 上限 7")
	}
	if _, err := Normalize(item(t, "pd", "replication.max-replicas"), "0"); err == nil {
		t.Error("max-replicas 下限 1")
	}
	if v, err := Normalize(item(t, "tidb", "performance.max-procs"), "0"); err != nil || v != "0" {
		t.Errorf("max-procs 允许 0: %q %v", v, err)
	}
	if _, err := Normalize(item(t, "ticdc", "gc-ttl"), "599"); err == nil {
		t.Error("gc-ttl 下限 600")
	}
	// 未知类型
	if _, err := Normalize(ConfigItem{Key: "x", Type: "weird"}, "1"); err == nil {
		t.Error("未知类型应拒绝")
	}
	// 自定义范围
	rng := ConfigItem{Key: "x", Type: TypeSize, Min: 1 << 20, Max: 1 << 30}
	if _, err := Normalize(rng, "1KB"); err == nil {
		t.Error("低于 Min")
	}
	if _, err := Normalize(rng, "2GB"); err == nil {
		t.Error("高于 Max")
	}
	if _, err := Normalize(rng, "512MB"); err != nil {
		t.Error(err)
	}
	drng := ConfigItem{Key: "d", Type: TypeDuration, Min: 60, Max: 3600}
	if _, err := Normalize(drng, "30s"); err == nil {
		t.Error("低于 Min")
	}
	if _, err := Normalize(drng, "2h"); err == nil {
		t.Error("高于 Max")
	}
}

// 任何通过 Normalize 的输出都只含 [A-Za-z0-9._-]，可以安全放进 SQL 字符串字面量 / pd-ctl 参数。
func TestNormalizeOutputIsInjectionSafe(t *testing.T) {
	inputs := []string{"16GB", "16gb", "30m", "90s", "on", "OFF", "3", "info", "leader", "closest-replicas", "1h30m", "0", "007",
		"1' OR '1", "x\\", "a;b", "1\n", "`x`", "$(x)", "ON\"", "5 5", "1TB"}
	for _, role := range []string{"tikv", "tidb", "pd", "tiflash", "ticdc"} {
		for _, it := range Catalog()[role] {
			for _, in := range inputs {
				out, err := Normalize(it, in)
				if err != nil {
					continue
				}
				for _, r := range out {
					if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
						t.Fatalf("%s/%s: Normalize(%q) = %q 含不安全字符 %q", role, it.Key, in, out, r)
					}
				}
			}
		}
	}
}

func TestSizeBytesAndDurationSeconds(t *testing.T) {
	for in, want := range map[string]int64{"16GB": 16 << 30, "512MB": 512 << 20, "1TB": 1 << 40, "1024KB": 1 << 20, "7B": 7, "0GB": 0} {
		if got, err := SizeBytes(in); err != nil || got != want {
			t.Errorf("SizeBytes(%q) = %d, %v", in, got, err)
		}
	}
	if _, err := SizeBytes("16"); err == nil {
		t.Error("无单位应拒绝")
	}
	for in, want := range map[string]int64{"30m": 1800, "2h": 7200, "90s": 90, "1h30m": 5400} {
		if got, err := DurationSeconds(in); err != nil || got != want {
			t.Errorf("DurationSeconds(%q) = %d, %v", in, got, err)
		}
	}
	if _, err := DurationSeconds("x"); err == nil {
		t.Error("非法时长应拒绝")
	}
}
