// @author: kevinkelly
// @license: Apache License 2.0
// @file: hostcheck_test.go
// @time: 2026/10/28

package collect_test

import (
	"strings"
	"testing"

	"tidbops/internal/collect"
)

const checkSample = `Starting component ` + "`cluster`" + `: /home/tidb/.tiup/components/cluster/v1.16.1/tiup-cluster check prod-tidb-01 scale-out.yml --cluster
Node         Check         Result  Message
----         -----         ------  -------
10.0.2.17    os-version    Pass    OS is Rocky Linux 9.3 9.3
10.0.2.17    cpu-cores     Pass    number of CPU cores / threads: 32
10.0.2.17    swap          Pass    swap is disabled
10.0.2.18    os-version    Pass    OS is Rocky Linux 9.3 9.3
10.0.2.18    swap          Fail    swap is enabled, please disable it for best performance
10.0.2.18    thp           Warn    THP is enabled, please disable it for best performance
10.0.2.18    ntp           Fail    chronyd/ntpd is not running
10.0.2.18    selinux       Fail    SELinux is not disabled
10.0.2.18    limits        Fail    soft limit of 'nofile' for user 'tidb' is not set or too low
10.0.2.18    limits        Fail    hard limit of 'nofile' for user 'tidb' is not set or too low
10.0.2.18    limits        Pass    soft limit of 'stack' for user 'tidb' is ok
10.0.2.18    disk          Warn    mount point /tidb-data does not have 'noatime' option set
10.0.2.18    disk          Warn    mount point /tidb-data does not have 'nodelalloc' option set
10.0.2.18    sysctl        Fail    net.core.somaxconn = 128, should be greater than 32768
10.0.2.18    cpu-governor  Warn    CPU frequency governor is powersave, should use performance
10.0.2.18    service       Fail    service irqbalance is not running
10.0.2.18    timezone      Warn    time zone differs from the control machine
10.0.2.180   swap          Fail    belongs to another host with a similar prefix
`

func TestParseCheckByHost(t *testing.T) {
	good := collect.ParseCheckByHost(checkSample, "10.0.2.17")
	if len(good) != 3 {
		t.Fatalf("10.0.2.17 应有 3 项: %+v", good)
	}
	for _, it := range good {
		if it.R != "ok" || it.Fix != "" || it.G != "主机" {
			t.Errorf("通过项不应有 fix: %+v", it)
		}
	}
	if good[1].N != "cpu-cores" || good[1].D != "number of CPU cores / threads: 32" {
		t.Errorf("通过项应保留 tiup 的说明: %+v", good[1])
	}

	items := collect.ParseCheckByHost(checkSample, "10.0.2.18")
	by := map[string]string{}
	res := map[string]string{}
	det := map[string]string{}
	for _, it := range items {
		by[it.N], res[it.N], det[it.N] = it.Fix, it.R, it.D
	}
	if len(items) != 11 {
		t.Errorf("同一检查项的多行应归并为一项（共 11 项）: %d: %+v", len(items), items)
	}
	// 10.0.2.180 的行不能混进 10.0.2.18
	if strings.Contains(det["swap"], "another host") {
		t.Errorf("主机过滤必须是精确匹配: %q", det["swap"])
	}
	want := map[string]struct{ r, fix string }{
		"swap":         {"bad", "swapoff -a"},
		"thp":          {"warn", "echo never > /sys/kernel/mm/transparent_hugepage/enabled"},
		"ntp":          {"bad", "systemctl enable --now chronyd"},
		"selinux":      {"bad", "setenforce 0"},
		"limits":       {"bad", `echo "tidb soft nofile 1000000" >> /etc/security/limits.conf`},
		"disk":         {"warn", "mount -o remount,noatime,nodelalloc /tidb-data"},
		"sysctl":       {"bad", "sysctl -w net.core.somaxconn=32768"},
		"cpu-governor": {"warn", "cpupower frequency-set --governor performance"},
		"service":      {"bad", "systemctl enable --now irqbalance"},
		"timezone":     {"warn", ""}, // 映射不到：fix 为空
		"os-version":   {"ok", ""},
	}
	for n, w := range want {
		if res[n] != w.r {
			t.Errorf("%s 结果 = %q，want %q", n, res[n], w.r)
		}
		if w.fix == "" && by[n] != "" || w.fix != "" && !strings.Contains(by[n], w.fix) {
			t.Errorf("%s 的 fix = %q，应包含 %q", n, by[n], w.fix)
		}
	}
	if !strings.Contains(by["limits"], `"tidb hard nofile 1000000"`) {
		t.Errorf("limits 的 fix 应覆盖 soft 与 hard: %q", by["limits"])
	}
	if strings.Contains(det["limits"], "stack") {
		t.Errorf("limits 有失败行时说明里只放失败 / 警告信息: %q", det["limits"])
	}
	if checkLabel := items[0].L; checkLabel == "" {
		t.Errorf("已知检查项应带中文名: %+v", items[0])
	}
	// 修复建议里的 ntp 偏移变体
	off := collect.ParseCheckByHost("1.1.1.1  ntp  Fail  time offset 156ms exceeds threshold\n", "1.1.1.1")
	if len(off) != 1 || off[0].Fix != "chronyc makestep" {
		t.Errorf("时间偏移应建议 chronyc makestep: %+v", off)
	}
}

func TestParseCheckByHostEdgeCases(t *testing.T) {
	if items := collect.ParseCheckByHost("", "10.0.0.1"); items != nil {
		t.Errorf("空输出应返回 nil: %+v", items)
	}
	if items := collect.ParseCheckByHost(checkSample, "10.9.9.9"); items != nil {
		t.Errorf("没有该主机的行应返回 nil: %+v", items)
	}
	// 不是结果表的行（表头、横线、提示语、列不足）一律忽略，不 panic
	junk := "Node Check Result Message\n---- ----- ------ -------\nAll checks passed\n10.0.0.1\n10.0.0.1 swap\n10.0.0.1 swap Maybe x\n"
	if items := collect.ParseCheckByHost(junk, "10.0.0.1"); items != nil {
		t.Errorf("无法识别的行应被忽略: %+v", items)
	}
	// 未知检查项：没有中文名与 fix，但仍保留
	it := collect.ParseCheckByHost("10.0.0.1  weird-check  Fail  something broke\n", "10.0.0.1")
	if len(it) != 1 || it[0].N != "weird-check" || it[0].R != "bad" || it[0].Fix != "" || it[0].L != "" {
		t.Errorf("未知检查项: %+v", it)
	}
	// Fail 没有说明
	it = collect.ParseCheckByHost("10.0.0.1  swap  Fail\n", "10.0.0.1")
	if len(it) != 1 || it[0].D == "" {
		t.Errorf("没有说明时也要有可读的描述: %+v", it)
	}
}
