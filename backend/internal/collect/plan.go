// @author: kevinkelly
// @license: Apache License 2.0
// @file: plan.go
// @time: 2026/10/28

package collect

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"tidbops/internal/model"
)

// 执行计划来自慢日志里的 Plan 列，经 tidb_decode_plan() 解码，是该 SQL 实际执行时的计划（含 actRows），
// 因此无需在生产上对业务 SQL 执行 EXPLAIN ANALYZE。

var reAccess = regexp.MustCompile(`^(table:[^,]+(?:, partition:[^,]+)?(?:, index:[^,(]*\([^)]*\))?)(?:, (.*))?$`)

// ParsePlan 解析 tidb_decode_plan 的输出。列按表头名定位，兼容不同版本的列顺序；缺少 actRows 时取 estRows。
// 返回行格式：[算子, estRows, actRows, task, access object, operator info]。
func ParsePlan(text string) [][]any {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	var header []string
	var out [][]any
	idx := map[string]int{}
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		f := strings.Split(ln, "\t")
		if header == nil {
			header = f
			for i, h := range f {
				idx[strings.ToLower(strings.TrimSpace(h))] = i
			}
			if _, ok := idx["id"]; !ok {
				return nil
			}
			continue
		}
		at := func(name string) string {
			if i, ok := idx[name]; ok && i < len(f) {
				return strings.TrimRight(f[i], " ")
			}
			return ""
		}
		id := at("id")
		if strings.TrimSpace(id) == "" {
			continue
		}
		est := f64(strings.TrimSpace(at("estrows")))
		act := est
		if v := strings.TrimSpace(at("actrows")); v != "" {
			act = f64(v)
		}
		info := at("operator info")
		access := at("access object")
		if access == "" {
			if m := reAccess.FindStringSubmatch(info); m != nil {
				access, info = m[1], m[2]
			}
		}
		out = append(out, []any{id, est, act, strings.TrimSpace(at("task")), access, info})
	}
	return out
}

// planTips 根据执行计划补充建议：统计信息缺失（stats:pseudo）、估算与实际行数严重偏差。
func planTips(plan [][]any, existing []model.Tip) (tips []model.Tip, tag string) {
	var pseudo, skew []string
	for _, r := range plan {
		id, _ := r[0].(string)
		est, _ := r[1].(float64)
		act, _ := r[2].(float64)
		info, _ := r[5].(string)
		if strings.Contains(info, "stats:pseudo") {
			pseudo = append(pseudo, strings.TrimLeft(id, " │└─"))
		}
		if act/maxF(est, 1) >= 10 && act >= 1000 || est/maxF(act, 1) >= 10 && est > 1000 {
			skew = append(skew, strings.TrimLeft(id, " │└─"))
		}
	}
	has := func(sub string) bool {
		for _, t := range existing {
			if strings.Contains(t.T+t.Cmd, sub) {
				return true
			}
		}
		return false
	}
	if len(pseudo) > 0 && !has("ANALYZE") {
		tips = append(tips, model.Tip{T: fmt.Sprintf("执行计划里 %s 使用了 pseudo 统计信息（表从未 ANALYZE 或统计信息已失效）：先收集统计信息", strings.Join(pseudo, "、")), Cmd: "ANALYZE TABLE <表名>;"})
		tag = "统计信息缺失"
	}
	if len(skew) > 0 {
		tips = append(tips, model.Tip{T: fmt.Sprintf("%s 的 estRows 与 actRows 偏差 ≥ 10 倍：优化器估算失真，可能选错索引或 Join 顺序，建议更新统计信息并检查直方图", strings.Join(skew, "、"))})
		if tag == "" {
			tag = "估算与实际行数偏差大"
		}
	}
	return
}

// planMap 读取 sql/slow-plan，返回 digest → 计划。条目缺失不影响其余慢查询分析。
func (c *Collector) planMap(ctx context.Context) map[string][][]any {
	b, err := c.get(ctx, "sql/slow-plan")
	if err != nil {
		return nil
	}
	_, rows := parseTSV(b)
	m := map[string][][]any{}
	for _, r := range rows {
		d := r["digest"]
		if _, ok := m[d]; ok || d == "" {
			continue // 同一 digest 只取耗时最长的那条（查询已按耗时降序）
		}
		if p := ParsePlan(r["plan"]); len(p) > 0 {
			m[d] = p
		}
	}
	return m
}
