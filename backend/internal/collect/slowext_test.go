// @author: kevinkelly
// @license: Apache License 2.0
// @file: slowext_test.go
// @time: 2026/10/28

package collect

import (
	"fmt"
	"strings"
	"testing"

	"tidbops/internal/model"
)

const extHdr = "Digest\tPlan_digest\tcnt\tcur_cnt\tcur_sum\tprev_sum\tpseudo_cnt\tlock_cnt\tregion_miss_cnt\tbusy_cnt\tpd_cnt\tmax_resolve_lock\tmax_del_skipped\tmax_retry\n"

// extRow 生成一行 sql/slow-ext：cnt 总样本，cur 当前窗口样本，curSum / prevSum 两个窗口的 Query_time 之和，
// 其后依次是 pseudo / lock / regionMiss / busy / pd 的当前窗口样本数。
func extRow(digest, plan string, cnt, cur int, curSum, prevSum float64, n ...int) string {
	c := make([]int, 5)
	copy(c, n)
	return fmt.Sprintf("%s\t%s\t%d\t%d\t%g\t%g\t%d\t%d\t%d\t%d\t%d\t0.8\t0\t3\n", digest, plan, cnt, cur, curSum, prevSum, c[0], c[1], c[2], c[3], c[4])
}

func groupOf(digest, sql string) model.SlowGroup {
	return model.SlowGroup{Digest: digest, DB: "shop", User: "app", SQL: sql, Tag: "耗时较长", Keys: 1000, PKeys: 900}
}

func findingsByID(fs []model.Finding) map[string]model.Finding {
	m := map[string]model.Finding{}
	for _, f := range fs {
		m[f.ID] = f
	}
	return m
}

func TestSlowExtPlanChange(t *testing.T) {
	// 旧计划 pold 上一小时 6 条平均 1s；当前窗口出现新计划 pnew（4 条，平均 3s）→ 计划突变且明显变慢
	tsv := extHdr +
		extRow("d1", "pold", 6, 0, 0, 6) +
		extRow("d1", "pnew", 4, 4, 12, 0)
	ext := parseSlowExt([]byte(tsv))
	a := ext["d1"]
	oldP, newP, ok := a.planChange()
	if !ok || oldP != "pold" || newP != "pnew" {
		t.Fatalf("计划变化判断不对: old=%q new=%q ok=%v", oldP, newP, ok)
	}
	fs := findingsByID(DiagnoseSlowExt([]model.SlowGroup{groupOf("d1", "SELECT * FROM orders WHERE id = ?")}, ext))
	f, found := fs["slowx-plan-d1"]
	if !found || f.Sev != "warning" {
		t.Fatalf("应有 warning 级别的计划突变诊断项: %+v", fs)
	}
	ev := strings.Join(f.Evidence, " ")
	if !strings.Contains(ev, "pnew") || !strings.Contains(ev, "pold") || !strings.Contains(ev, "3.0 倍") {
		t.Errorf("证据应写明新旧计划与耗时倍数: %v", f.Evidence)
	}
	if !strings.Contains(f.Actions[0].Cmd, "PLAN DIGEST 'pold'") || !strings.Contains(f.Actions[0].Cmd, "'d1'") {
		t.Errorf("建议应带绑定旧计划与查看计划的语句: %q", f.Actions[0].Cmd)
	}

	// 计划变了但没变慢 → 只作提示
	same := extHdr + extRow("d2", "pold", 6, 0, 0, 6) + extRow("d2", "pnew", 4, 4, 4, 0)
	fs = findingsByID(DiagnoseSlowExt([]model.SlowGroup{groupOf("d2", "SELECT 1 FROM t")}, parseSlowExt([]byte(same))))
	if f := fs["slowx-plan-d2"]; f.Sev != "info" {
		t.Errorf("计划变化但耗时没变，应为 info: %+v", f)
	}
}

func TestSlowExtNoChangeCases(t *testing.T) {
	// 上一窗口没有样本：新出现的计划不能算「突变」（也许只是上一小时没有超过慢日志阈值）
	fresh := parseSlowExt([]byte(extHdr + extRow("d1", "p1", 3, 3, 9, 0)))
	if _, _, ok := fresh["d1"].planChange(); ok {
		t.Error("上一窗口没有样本时不应判定计划突变")
	}
	// 两个窗口都只有同一个计划：不是突变
	steady := parseSlowExt([]byte(extHdr + extRow("d1", "p1", 10, 5, 5, 5)))
	if _, _, ok := steady["d1"].planChange(); ok {
		t.Error("计划没变不应判定突变")
	}
	if fs := DiagnoseSlowExt([]model.SlowGroup{groupOf("d1", "SELECT 1 FROM t")}, steady); len(fs) != 0 {
		t.Errorf("平稳的数据不应有诊断项: %+v", fs)
	}
	if fs := DiagnoseSlowExt([]model.SlowGroup{groupOf("d1", "SELECT 1 FROM t")}, nil); fs != nil {
		t.Errorf("没有扩展数据时不应有诊断项: %+v", fs)
	}
}

func TestSlowExtSamePlanSlower(t *testing.T) {
	// 同一计划：上一小时 6 条平均 1s，当前 6 条平均 3s
	ext := parseSlowExt([]byte(extHdr + extRow("d1", "p1", 12, 6, 18, 6, 0, 0, 0, 0, 0)))
	fs := findingsByID(DiagnoseSlowExt([]model.SlowGroup{groupOf("d1", "SELECT 1 FROM t")}, ext))
	if f := fs["slowx-slower-d1"]; f.Sev != "warning" || !strings.Contains(strings.Join(f.Evidence, " "), "3.0 倍") {
		t.Errorf("应有耗时上升的诊断项: %+v", fs)
	}
	// 样本太少 / 绝对耗时太小 → 不报
	few := parseSlowExt([]byte(extHdr + extRow("d1", "p1", 6, 3, 9, 3, 0, 0, 0, 0, 0)))
	if fs := DiagnoseSlowExt([]model.SlowGroup{groupOf("d1", "SELECT 1 FROM t")}, few); len(fs) != 0 {
		t.Errorf("样本不足不应判定: %+v", fs)
	}
	tiny := parseSlowExt([]byte(extHdr + extRow("d1", "p1", 12, 6, 1.8, 0.6, 0, 0, 0, 0, 0)))
	if fs := DiagnoseSlowExt([]model.SlowGroup{groupOf("d1", "SELECT 1 FROM t")}, tiny); len(fs) != 0 {
		t.Errorf("平均耗时不到 0.5s 不应判定: %+v", fs)
	}
}

func TestSlowExtBackoffAndStats(t *testing.T) {
	// 10 条里：6 条 pseudo，4 条 txnLock，4 条 tikvServerBusy，6 条 regionMiss
	ext := parseSlowExt([]byte(extHdr + extRow("d1", "p1", 10, 10, 20, 0, 6, 4, 6, 4, 0)))
	g := groupOf("d1", "UPDATE inventory SET stock = stock - 1 WHERE sku = ?")
	fs := findingsByID(DiagnoseSlowExt([]model.SlowGroup{g}, ext))
	for id, sev := range map[string]string{"slowx-stats-d1": "warning", "slowx-lock-d1": "warning", "slowx-busy-d1": "info", "slowx-miss-d1": "info"} {
		if f, ok := fs[id]; !ok || f.Sev != sev {
			t.Errorf("缺少或级别不对 %s: %+v", id, f)
		}
	}
	if c := fs["slowx-stats-d1"].Actions[0].Cmd; !strings.Contains(c, "ANALYZE TABLE inventory") {
		t.Errorf("统计信息建议应指向具体的表: %q", c)
	}
	if !strings.Contains(fs["slowx-lock-d1"].Actions[0].Cmd, "DATA_LOCK_WAITS") {
		t.Errorf("锁冲突建议应含 DATA_LOCK_WAITS: %+v", fs["slowx-lock-d1"].Actions)
	}
	// 同时有 pdRPC 退避时，regionMiss 不单独报（更可能是 PD 问题）
	pd := parseSlowExt([]byte(extHdr + extRow("d1", "p1", 10, 10, 20, 0, 0, 0, 6, 0, 2)))
	if fs := findingsByID(DiagnoseSlowExt([]model.SlowGroup{g}, pd)); len(fs) != 0 {
		t.Errorf("有 pdRPC 退避时不应报 regionMiss: %+v", fs)
	}
	// 占比没到阈值 → 不报
	low := parseSlowExt([]byte(extHdr + extRow("d1", "p1", 10, 10, 20, 0, 2, 2, 2, 2, 0)))
	if fs := DiagnoseSlowExt([]model.SlowGroup{g}, low); len(fs) != 0 {
		t.Errorf("占比低不应报: %+v", fs)
	}
}

func TestSlowExtMVCC(t *testing.T) {
	g := groupOf("d1", "SELECT COUNT(*) FROM events WHERE ts > ?")
	g.Keys, g.PKeys = 5e6, 2e5 // 25 倍
	ext := parseSlowExt([]byte(extHdr + extRow("d1", "p1", 3, 3, 9, 0, 0, 0, 0, 0, 0)))
	fs := findingsByID(DiagnoseSlowExt([]model.SlowGroup{g}, ext))
	if f, ok := fs["slowx-mvcc-d1"]; !ok || f.Sev != "info" || !strings.Contains(strings.Join(f.Evidence, " "), "25 倍") {
		t.Errorf("应有 MVCC 旧版本的提示: %+v", fs)
	}
	// 已经被判定为「缺索引」的语句，不再重复报
	g.Tag = "缺索引 / 统计信息过期"
	if fs := DiagnoseSlowExt([]model.SlowGroup{g}, ext); len(fs) != 0 {
		t.Errorf("缺索引的语句不应再报 MVCC: %+v", fs)
	}
}

func TestSlowExtLimitPerKind(t *testing.T) {
	var tsv strings.Builder
	tsv.WriteString(extHdr)
	var slow []model.SlowGroup
	for i := 0; i < 6; i++ {
		d := fmt.Sprintf("d%d", i)
		tsv.WriteString(extRow(d, "p1", 4, 4, 8, 0, 4, 0, 0, 0, 0))
		slow = append(slow, groupOf(d, "SELECT 1 FROM t"))
	}
	if fs := DiagnoseSlowExt(slow, parseSlowExt([]byte(tsv.String()))); len(fs) != extMaxFindings {
		t.Errorf("每类最多 %d 条，实际 %d", extMaxFindings, len(fs))
	}
}

func TestApplyExtRetag(t *testing.T) {
	lock := parseSlowExt([]byte(extHdr + extRow("d1", "p1", 10, 10, 20, 0, 0, 5, 0, 0, 0)))["d1"]

	g := groupOf("d1", "UPDATE t SET a = 1")
	applyExt(&g, lock)
	if g.Tag != "锁冲突" || len(g.Tips) == 0 || !strings.Contains(g.Tips[0].T, "txnLock") {
		t.Errorf("默认标签应被替换为锁冲突并带建议: %q %+v", g.Tag, g.Tips)
	}
	// 锁冲突证据比「TiKV 排队等待」更具体 → 可以替换
	q := groupOf("d1", "UPDATE t SET a = 1")
	q.Tag = TagKVQueue
	applyExt(&q, lock)
	if q.Tag != "锁冲突" {
		t.Errorf("TiKV 排队等待应被锁冲突证据替换: %q", q.Tag)
	}
	// 其它已有的具体标签不覆盖，但建议仍会追加
	o := groupOf("d1", "SELECT 1")
	o.Tag = "索引列上使用函数"
	applyExt(&o, lock)
	if o.Tag != "索引列上使用函数" || len(o.Tips) == 0 {
		t.Errorf("已有具体标签不应被覆盖: %q %+v", o.Tag, o.Tips)
	}
	// 没有扩展数据 / 当前窗口没有样本：原样
	n := groupOf("d1", "SELECT 1")
	applyExt(&n, nil)
	applyExt(&n, &slowAgg{})
	if n.Tag != "耗时较长" || len(n.Tips) != 0 {
		t.Errorf("无扩展数据时不应改动: %+v", n)
	}
}
