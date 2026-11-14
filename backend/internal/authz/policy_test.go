// @author: kevinkelly
// @license: Apache License 2.0
// @file: policy_test.go
// @time: 2026/10/28

package authz

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// 旧的写死表（A3 §2.4），逐格作为回归基线：DefaultPolicy().Lookup 必须与之完全一致。
func legacyPolicy(risk, env string, emergency bool) (int, bool) {
	var levels int
	var mfa bool
	switch env {
	case EnvTest, EnvStaging, EnvProd:
	default:
		return 2, true
	}
	switch risk {
	case RiskLow:
		levels = 0
	case RiskMid:
		if env == EnvTest {
			levels = 0
		} else {
			levels = 1
		}
	case RiskHigh:
		if env == EnvTest {
			levels = 1
		} else {
			levels, mfa = 2, env == EnvProd
		}
	default:
		return 2, true
	}
	if emergency && env != EnvTest && levels < 2 {
		levels = 2
	}
	return levels, mfa
}

func TestDefaultPolicyMatchesLegacyTable(t *testing.T) {
	d := DefaultPolicy()
	if err := d.Validate(); err != nil {
		t.Fatalf("默认策略必须通过校验: %v", err)
	}
	risks := append(Risks(), "critical", "")
	envs := append(Envs(), "dev", "", EnvDefault)
	for _, r := range risks {
		for _, e := range envs {
			for _, em := range []bool{false, true} {
				wl, wm := legacyPolicy(r, e, em)
				gl, gm := d.Lookup(r, e, em)
				if gl != wl || gm != wm {
					t.Errorf("Lookup(%q,%q,%v)=(%d,%v)，旧行为 (%d,%v)", r, e, em, gl, gm, wl, wm)
				}
				al, am := ApprovalPolicy(r, e, em)
				if al != wl || am != wm {
					t.Errorf("ApprovalPolicy(%q,%q,%v)=(%d,%v)，旧行为 (%d,%v)", r, e, em, al, am, wl, wm)
				}
			}
		}
	}
}

func TestPolicyValidateFloors(t *testing.T) {
	mut := func(f func(p *PolicyTable)) PolicyTable {
		p := DefaultPolicy()
		f(&p)
		return p
	}
	bad := map[string]PolicyTable{
		"prod 高风险降到 1 级":    mut(func(p *PolicyTable) { p.Levels[RiskHigh][EnvProd] = 1 }),
		"prod 高风险关闭 MFA":    mut(func(p *PolicyTable) { p.MFA[RiskHigh][EnvProd] = false }),
		"prod 中风险免审批":       mut(func(p *PolicyTable) { p.Levels[RiskMid][EnvProd] = 0 }),
		"staging 高风险降到 1 级": mut(func(p *PolicyTable) { p.Levels[RiskHigh][EnvStaging] = 1 }),
		"级数超过 3":            mut(func(p *PolicyTable) { p.Levels[RiskLow][EnvTest] = 4 }),
		"级数为负":              mut(func(p *PolicyTable) { p.Levels[RiskLow][EnvTest] = -1 }),
		"缺少级数格":             mut(func(p *PolicyTable) { delete(p.Levels[RiskMid], EnvStaging) }),
		"缺少 MFA 格":          mut(func(p *PolicyTable) { delete(p.MFA[RiskMid], EnvStaging) }),
		"未知风险":              mut(func(p *PolicyTable) { p.Levels["critical"] = map[string]int{EnvProd: 2} }),
		"未知环境":              mut(func(p *PolicyTable) { p.MFA[RiskLow]["dev"] = true }),
		"空表":                {},
	}
	for name, p := range bad {
		err := p.Validate()
		if err == nil {
			t.Errorf("%s：应被拒绝", name)
		} else if !errors.Is(err, ErrPolicyInvalid) {
			t.Errorf("%s：错误应能用 errors.Is(ErrPolicyInvalid) 判断: %v", name, err)
		}
	}
	// 硬性下限的错误文案要说明是哪一格
	err := bad["prod 高风险降到 1 级"].Validate()
	if err == nil || !strings.Contains(err.Error(), "生产") || !strings.Contains(err.Error(), "2 级") {
		t.Errorf("文案应指出生产环境高风险至少 2 级: %v", err)
	}

	ok := map[string]PolicyTable{
		"低风险任何环境 0 级":        mut(func(p *PolicyTable) {}),
		"test 高风险免审批":        mut(func(p *PolicyTable) { p.Levels[RiskHigh][EnvTest] = 0 }),
		"prod 高风险 3 级":       mut(func(p *PolicyTable) { p.Levels[RiskHigh][EnvProd] = 3 }),
		"staging 中风险免审批":     mut(func(p *PolicyTable) { p.Levels[RiskMid][EnvStaging] = 0 }),
		"staging 低风险 MFA":    mut(func(p *PolicyTable) { p.MFA[RiskLow][EnvStaging] = true }),
		"test 高风险 MFA + 3 级": mut(func(p *PolicyTable) { p.Levels[RiskHigh][EnvTest], p.MFA[RiskHigh][EnvTest] = 3, true }),
	}
	for name, p := range ok {
		if err := p.Validate(); err != nil {
			t.Errorf("%s：应允许: %v", name, err)
		}
	}
}

func TestPolicyLookupCustomAndEmergency(t *testing.T) {
	p := DefaultPolicy()
	p.Levels[RiskMid][EnvStaging] = 2
	p.Levels[RiskLow][EnvProd] = 1
	p.MFA[RiskMid][EnvProd] = true
	if l, m := p.Lookup(RiskMid, EnvStaging, false); l != 2 || m {
		t.Errorf("自定义级数: (%d,%v)", l, m)
	}
	if l, _ := p.Lookup(RiskLow, EnvProd, false); l != 1 {
		t.Errorf("低风险 prod 改为 1 级: %d", l)
	}
	if _, m := p.Lookup(RiskMid, EnvProd, false); !m {
		t.Error("中风险 prod MFA 应为 true")
	}
	// 紧急变更：固定规则，不受表影响（表里 0 级也会被抬到 2）；test 无紧急通道
	p.Levels[RiskLow][EnvStaging] = 0
	if l, _ := p.Lookup(RiskLow, EnvStaging, true); l != 2 {
		t.Errorf("staging 紧急变更至少 2 级: %d", l)
	}
	if l, _ := p.Lookup(RiskLow, EnvProd, true); l != 2 {
		t.Errorf("prod 紧急变更至少 2 级: %d", l)
	}
	if l, _ := p.Lookup(RiskLow, EnvTest, true); l != 0 {
		t.Errorf("test 无紧急通道: %d", l)
	}
	// 3 级的格子不会被紧急规则降低
	p.Levels[RiskHigh][EnvProd] = 3
	if l, m := p.Lookup(RiskHigh, EnvProd, true); l != 3 || !m {
		t.Errorf("紧急变更不应降低级数: (%d,%v)", l, m)
	}
	// 缺格 / 未知输入 fail closed
	if l, m := (PolicyTable{}).Lookup(RiskLow, EnvTest, false); l != 2 || !m {
		t.Errorf("空表应 fail closed: (%d,%v)", l, m)
	}
	if l, m := p.Lookup("x", EnvProd, false); l != 2 || !m {
		t.Errorf("未知风险应 fail closed: (%d,%v)", l, m)
	}
}

func TestPolicyCloneEqualDiffJSON(t *testing.T) {
	d := DefaultPolicy()
	c := d.Clone()
	if !d.Equal(c) {
		t.Fatal("克隆应相等")
	}
	c.Levels[RiskHigh][EnvProd] = 3
	if d.Levels[RiskHigh][EnvProd] != 2 {
		t.Fatal("克隆不应与原表共享数据")
	}
	if d.Equal(c) {
		t.Fatal("修改后不应相等")
	}
	c.MFA[RiskMid][EnvStaging] = true
	diff := d.Diff(c)
	if len(diff) != 2 || !strings.Contains(diff[0], "预发·中风险 MFA 不要→要") || !strings.Contains(diff[1], "生产·高风险 级数 2→3") {
		t.Errorf("差异描述: %v", diff)
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var back PolicyTable
	if err := json.Unmarshal(b, &back); err != nil || !back.Equal(d) {
		t.Errorf("JSON 往返: %v %s", err, b)
	}
}
