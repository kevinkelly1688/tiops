// @author: kevinkelly
// @license: Apache License 2.0
// @file: import_test.go
// @time: 2026/10/28

package clusters

import (
	"strings"
	"testing"
)

func openLocal(t *testing.T, localTiup bool) *Registry {
	t.Helper()
	r, err := Open("", make([]byte, 32), Spec{ID: PlatformID, Name: PlatformName, Env: "prod", Mode: ModeLive, Platform: true}, Options{LocalTiup: localTiup, RequirePD: true})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// 导入 = Local 集群；手工添加恒不是 Local（Input 里根本没有 local 字段）。
func TestPrepareImportSetsLocalAndManualAddNeverDoes(t *testing.T) {
	r := openLocal(t, true)
	in := Input{ID: "prod-a", Name: sp("prod-a"), Env: sp("prod"), Mode: sp(ModeLive), PD: sp("http://10.0.1.21:2379")}
	imp, err := r.PrepareImport(in, "admin")
	if err != nil || !imp.Local || imp.Mode != ModeLive {
		t.Fatalf("导入应得到 Local 集群: %+v %v", imp, err)
	}
	man, err := r.PrepareAdd(Input{ID: "ext", Env: sp("test"), Mode: sp(ModeLive), PD: sp("http://10.9.9.9:2379")}, "admin")
	if err != nil || man.Local {
		t.Fatalf("手工添加不能是 Local: %+v %v", man, err)
	}
}

func TestPrepareImportRules(t *testing.T) {
	// 本机不是 tiup 中控机：不能导入
	r := openLocal(t, false)
	if _, err := r.PrepareImport(Input{ID: "a1", Env: sp("prod"), PD: sp("http://10.0.1.21:2379")}, "admin"); err == nil || !strings.Contains(err.Error(), "中控机") {
		t.Errorf("非中控机应拒绝导入: %v", err)
	}
	r = openLocal(t, true)
	// 回放集群不能是 Local
	if _, err := r.PrepareImport(Input{ID: "a1", Env: sp("prod"), Mode: sp(ModeReplay)}, "admin"); err == nil {
		t.Error("导入只能是 live")
	}
	// 与手工集群同名冲突
	man, _ := r.PrepareAdd(Input{ID: "ext", Name: sp("prod-a"), Env: sp("test"), Mode: sp(ModeLive), PD: sp("http://10.9.9.9:2379")}, "admin")
	if err := r.Put(man); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PrepareImport(Input{ID: "prod-a", Env: sp("prod"), PD: sp("http://10.0.1.21:2379")}, "admin"); err == nil {
		t.Error("与手工集群同名应冲突")
	}
	// 保留 id
	for _, id := range []string{"import", "discover"} {
		if _, err := r.PrepareAdd(Input{ID: id, Env: sp("prod"), Mode: sp(ModeLive), PD: sp("http://10.0.1.21:2379")}, "admin"); err == nil {
			t.Errorf("id %q 是保留字", id)
		}
	}
}

// 更新不能把远程集群变成本机 tiup 集群，也不能把本机 tiup 集群变成别的。
func TestUpdateKeepsLocalFlag(t *testing.T) {
	r := openLocal(t, true)
	imp, err := r.PrepareImport(Input{ID: "prod-a", Env: sp("prod"), PD: sp("http://10.0.1.21:2379")}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Put(imp); err != nil {
		t.Fatal(err)
	}
	man, _ := r.PrepareAdd(Input{ID: "ext", Env: sp("test"), Mode: sp(ModeLive), PD: sp("http://10.9.9.9:2379")}, "admin")
	if err := r.Put(man); err != nil {
		t.Fatal(err)
	}
	_, next, err := r.PrepareUpdate("prod-a", Input{Display: sp("生产 A")}, "admin")
	if err != nil || !next.Local {
		t.Errorf("更新不应丢掉 Local: %+v %v", next, err)
	}
	_, next, err = r.PrepareUpdate("ext", Input{Display: sp("外部")}, "admin")
	if err != nil || next.Local {
		t.Errorf("更新不应让远程集群变成 Local: %+v %v", next, err)
	}
}
