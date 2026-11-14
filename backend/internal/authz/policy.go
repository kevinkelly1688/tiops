// @author: kevinkelly
// @license: Apache License 2.0
// @file: policy.go
// @time: 2026/10/28

package authz

import (
	"errors"
	"fmt"
)

// 审批策略表（A3 §2.4）：风险 × 环境 → 审批级数与是否需要申请人 MFA。
//
// 默认值与 A3 §2.4 完全一致，可由管理员经「双人复核」修改，但受硬性下限约束（见 Validate）。
// 「紧急变更」规则固定在代码里、不可配置：staging / prod 至少 2 级（test 无紧急通道，按常规）。

// Risks 与 Envs 按展示顺序返回风险等级与环境。
func Risks() []string { return []string{RiskLow, RiskMid, RiskHigh} }
func Envs() []string  { return []string{EnvTest, EnvStaging, EnvProd} }

// MaxLevels 是审批级数的上限。
const MaxLevels = 3

// PolicyTable 是可配置的审批策略。Levels[risk][env] 是审批级数（0–3），MFA[risk][env] 表示申请人是否必须二次验证。
type PolicyTable struct {
	Levels map[string]map[string]int  `json:"levels"`
	MFA    map[string]map[string]bool `json:"mfa"`
}

// DefaultPolicy 返回与 A3 §2.4 一致的默认策略：
//
//	低：任何环境 0 级
//	中：test 0 / staging 1 / prod 1
//	高：test 1 / staging 2 / prod 2 + 申请人 MFA
func DefaultPolicy() PolicyTable {
	return PolicyTable{
		Levels: map[string]map[string]int{
			RiskLow:  {EnvTest: 0, EnvStaging: 0, EnvProd: 0},
			RiskMid:  {EnvTest: 0, EnvStaging: 1, EnvProd: 1},
			RiskHigh: {EnvTest: 1, EnvStaging: 2, EnvProd: 2},
		},
		MFA: map[string]map[string]bool{
			RiskLow:  {EnvTest: false, EnvStaging: false, EnvProd: false},
			RiskMid:  {EnvTest: false, EnvStaging: false, EnvProd: false},
			RiskHigh: {EnvTest: false, EnvStaging: false, EnvProd: true},
		},
	}
}

// Clone 深拷贝。
func (t PolicyTable) Clone() PolicyTable {
	out := PolicyTable{Levels: map[string]map[string]int{}, MFA: map[string]map[string]bool{}}
	for r, m := range t.Levels {
		out.Levels[r] = map[string]int{}
		for e, v := range m {
			out.Levels[r][e] = v
		}
	}
	for r, m := range t.MFA {
		out.MFA[r] = map[string]bool{}
		for e, v := range m {
			out.MFA[r][e] = v
		}
	}
	return out
}

// Equal 报告两张表是否逐格相同（缺格视为不同）。
func (t PolicyTable) Equal(o PolicyTable) bool {
	for _, r := range Risks() {
		for _, e := range Envs() {
			l1, ok1 := t.Levels[r][e]
			l2, ok2 := o.Levels[r][e]
			if ok1 != ok2 || l1 != l2 {
				return false
			}
			m1, ok1 := t.MFA[r][e]
			m2, ok2 := o.MFA[r][e]
			if ok1 != ok2 || m1 != m2 {
				return false
			}
		}
	}
	return true
}

// PolicyFloor 是一条硬性下限：该格的审批级数不得低于 MinLevels；MFA 为 true 时该格必须要求申请人 MFA。
type PolicyFloor struct {
	Risk      string `json:"risk"`
	Env       string `json:"env"`
	MinLevels int    `json:"minLevels"`
	MFA       bool   `json:"mfa"`
}

// PolicyFloors 返回全部硬性下限（写死在代码里，不可通过配置放宽）：
// prod 的高风险至少 2 级且必须 MFA；prod 的中风险至少 1 级；staging 的高风险至少 2 级。
func PolicyFloors() []PolicyFloor {
	return []PolicyFloor{
		{RiskHigh, EnvProd, 2, true},
		{RiskMid, EnvProd, 1, false},
		{RiskHigh, EnvStaging, 2, false},
	}
}

// ErrPolicyInvalid 是策略表校验失败的错误基类（errors.Is 可判断）。
var ErrPolicyInvalid = errors.New("审批策略不合法")

func envLabel(e string) string {
	switch e {
	case EnvTest:
		return "测试"
	case EnvStaging:
		return "预发"
	case EnvProd:
		return "生产"
	}
	return e
}

func riskLabel(r string) string {
	switch r {
	case RiskLow:
		return "低"
	case RiskMid:
		return "中"
	case RiskHigh:
		return "高"
	}
	return r
}

// Validate 校验策略表：九个格子必须齐全且不含未知的风险 / 环境；级数在 0–3；不得违反硬性下限。
// 低风险任何环境都允许 0 级。返回的错误可用 errors.Is(err, ErrPolicyInvalid) 判断，文案为中文，可直接展示。
func (t PolicyTable) Validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w：%s", ErrPolicyInvalid, fmt.Sprintf(format, a...))
	}
	known := func(list []string, s string) bool {
		for _, x := range list {
			if x == s {
				return true
			}
		}
		return false
	}
	for r, m := range t.Levels {
		if !known(Risks(), r) {
			return bad("未知的风险等级 %q", r)
		}
		for e := range m {
			if !known(Envs(), e) {
				return bad("未知的环境 %q", e)
			}
		}
	}
	for r, m := range t.MFA {
		if !known(Risks(), r) {
			return bad("未知的风险等级 %q", r)
		}
		for e := range m {
			if !known(Envs(), e) {
				return bad("未知的环境 %q", e)
			}
		}
	}
	for _, r := range Risks() {
		for _, e := range Envs() {
			l, ok := t.Levels[r][e]
			if !ok {
				return bad("缺少 %s风险 / %s环境 的审批级数", riskLabel(r), envLabel(e))
			}
			if l < 0 || l > MaxLevels {
				return bad("%s风险 / %s环境 的审批级数 %d 超出范围 0–%d", riskLabel(r), envLabel(e), l, MaxLevels)
			}
			if _, ok := t.MFA[r][e]; !ok {
				return bad("缺少 %s风险 / %s环境 的 MFA 设置", riskLabel(r), envLabel(e))
			}
		}
	}
	for _, f := range PolicyFloors() {
		if t.Levels[f.Risk][f.Env] < f.MinLevels {
			return bad("%s环境的%s风险变更至少需要 %d 级审批（硬性下限）", envLabel(f.Env), riskLabel(f.Risk), f.MinLevels)
		}
		if f.MFA && !t.MFA[f.Risk][f.Env] {
			return bad("%s环境的%s风险变更必须要求申请人 MFA（硬性下限）", envLabel(f.Env), riskLabel(f.Risk))
		}
	}
	return nil
}

// Lookup 返回审批级数与是否需要申请人 MFA。
//
// 未知的 risk / env、或表里缺格，一律按最严格处理（2 级 + MFA），fail closed。
// 紧急变更规则固定在代码里：staging / prod 至少 2 级（prod 另需事后复盘，由调用方处理）；test 无紧急通道，按常规。
func (t PolicyTable) Lookup(risk, env string, emergency bool) (levels int, applicantMFA bool) {
	l, ok1 := t.Levels[risk][env]
	m, ok2 := t.MFA[risk][env]
	if !ok1 || !ok2 {
		return 2, true
	}
	if l < 0 {
		l = 0
	}
	if l > MaxLevels {
		l = MaxLevels
	}
	if emergency && (env == EnvStaging || env == EnvProd) && l < 2 {
		l = 2
	}
	return l, m
}

// Diff 返回两张表之间差异格的中文描述（按风险、环境顺序），用于审计摘要。
func (t PolicyTable) Diff(o PolicyTable) []string {
	var out []string
	yn := func(b bool) string {
		if b {
			return "要"
		}
		return "不要"
	}
	for _, r := range Risks() {
		for _, e := range Envs() {
			if t.Levels[r][e] != o.Levels[r][e] {
				out = append(out, fmt.Sprintf("%s·%s风险 级数 %d→%d", envLabel(e), riskLabel(r), t.Levels[r][e], o.Levels[r][e]))
			}
			if t.MFA[r][e] != o.MFA[r][e] {
				out = append(out, fmt.Sprintf("%s·%s风险 MFA %s→%s", envLabel(e), riskLabel(r), yn(t.MFA[r][e]), yn(o.MFA[r][e])))
			}
		}
	}
	return out
}

// ApprovalPolicy 返回默认策略下的审批级数与是否需要申请人 MFA（A3 §2.4）。
// 可配置的版本见 PolicyTable.Lookup；本函数等价于 DefaultPolicy().Lookup。
//
// 未知的 risk / env 按最严格处理（2 级 + MFA），fail closed。
func ApprovalPolicy(risk, env string, emergency bool) (levels int, applicantMFA bool) {
	return defaultPolicy.Lookup(risk, env, emergency)
}

var defaultPolicy = DefaultPolicy()
