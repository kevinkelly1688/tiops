// @author: kevinkelly
// @license: Apache License 2.0
// @file: scalein_zone_test.go
// @time: 2026/10/28

package change

import (
	"strings"
	"testing"
)

// 缩容 TiKV 的 zone 打散检查：没有标签的不参与；只有「由够用降到不够用」才阻断；本来就不足的只提示。
func TestScaleInZoneRule(t *testing.T) {
	// 6 个 TiKV（tikv-1..tikv-6），按顺序设置可用区
	plan := func(t *testing.T, azs [6]string, pick string) Plan {
		t.Helper()
		topo := testTopo()
		i := 0
		for k := range topo.Instances {
			if topo.Instances[k].Type == "tikv" {
				topo.Instances[k].AZ = azs[i]
				i++
			}
		}
		p, err := (Ctx{Cluster: "prod", Topo: topo}).Plan(KindScaleIn, js(scaleInParams{Nodes: []string{pick}}))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	zoneCheck := func(p Plan) (lv, text string) {
		for _, c := range p.Checks {
			if strings.Contains(c.T, "将没有 TiKV") {
				return c.Lv, c.T
			}
		}
		return "", ""
	}

	t.Run("老节点无标签、只有被下线的带标签：只提示不阻断", func(t *testing.T) {
		p := plan(t, [6]string{"—", "—", "—", "—", "—", "hz"}, "tikv-6")
		if lv, _ := zoneCheck(p); lv != "warn" {
			t.Errorf("应只给警告: %+v", p.Checks)
		}
		if p.Blocked {
			t.Errorf("不应阻断: %+v", p.Checks)
		}
	})
	t.Run("全部无标签：不出现 zone 提示", func(t *testing.T) {
		p := plan(t, [6]string{"—", "—", "—", "—", "—", "—"}, "tikv-6")
		if lv, text := zoneCheck(p); lv != "" {
			t.Errorf("无标签不应有 zone 提示: %s", text)
		}
	})
	t.Run("下线后仍有足够可用区", func(t *testing.T) {
		p := plan(t, [6]string{"az1", "az2", "az3", "az4", "az1", "az2"}, "tikv-4")
		if lv, _ := zoneCheck(p); lv != "ok" {
			t.Errorf("az4 消失但仍有 3 个可用区，应为 ✔: %+v", p.Checks)
		}
		if p.Blocked {
			t.Errorf("不应阻断: %+v", p.Checks)
		}
	})
	t.Run("由够用降到不够用：阻断", func(t *testing.T) {
		p := plan(t, [6]string{"az1", "az1", "az2", "az2", "az3", "az2"}, "tikv-5")
		if lv, text := zoneCheck(p); lv != "bad" || !strings.Contains(text, "由 3 降到 2") {
			t.Errorf("应阻断并说明 3 → 2: %+v", p.Checks)
		}
		if !p.Blocked {
			t.Errorf("应被阻断")
		}
	})
	t.Run("本来就不足：只提示", func(t *testing.T) {
		p := plan(t, [6]string{"az1", "az1", "az1", "az1", "az1", "az2"}, "tikv-6")
		if lv, text := zoneCheck(p); lv != "warn" || !strings.Contains(text, "本来只有 2 个") {
			t.Errorf("本来只有 2 个可用区，应只给警告: %+v", p.Checks)
		}
		if p.Blocked {
			t.Errorf("不应阻断: %+v", p.Checks)
		}
	})
	t.Run("可用区仍有节点：无提示", func(t *testing.T) {
		p := plan(t, [6]string{"az1", "az2", "az3", "az1", "az2", "az3"}, "tikv-1")
		if lv, text := zoneCheck(p); lv != "" {
			t.Errorf("az1 仍有 tikv-4，不应有提示: %s", text)
		}
	})
}
