// @author: kevinkelly
// @license: Apache License 2.0
// @file: local_collect_test.go
// @time: 2026/10/28

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	xexec "tidbops/internal/exec"
)

type fakeRunner struct {
	got []xexec.Request
	res xexec.Result
	err error
}

func (f *fakeRunner) Run(_ context.Context, req xexec.Request, _ func(string)) (xexec.Result, error) {
	f.got = append(f.got, req)
	return f.res, f.err
}

func TestLocalTiupExecRoutesTiupCommandsThroughExecutor(t *testing.T) {
	directCalls := 0
	direct := func(_ context.Context, argv []string, _ []string) ([]byte, error) {
		directCalls++
		return []byte("direct:" + strings.Join(argv, " ")), nil
	}
	r := &fakeRunner{res: xexec.Result{Output: "Cluster type: tidb\n"}}
	ex := localTiupExec(r, "tidb-test", direct)

	// tiup cluster display <本集群> → 执行器 cluster.display
	out, err := ex(context.Background(), []string{"tiup", "cluster", "display", "tidb-test"}, nil)
	if err != nil || string(out) != "Cluster type: tidb\n" {
		t.Fatalf("display: out=%q err=%v", out, err)
	}
	if len(r.got) != 1 || r.got[0].Cmd.Kind != xexec.KindClusterDisplay || r.got[0].Cmd.Cluster != "tidb-test" {
		t.Fatalf("应走 cluster.display: %+v", r.got)
	}
	// 带路径的 tiup（--tiup /home/tidb/.tiup/bin/tiup）同样识别
	if _, err := ex(context.Background(), []string{"/home/tidb/.tiup/bin/tiup", "cluster", "list"}, nil); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(r.got) != 2 || r.got[1].Cmd.Kind != xexec.KindClusterList {
		t.Fatalf("应走 cluster.list: %+v", r.got)
	}
	// tiup --version / tiup cluster --version 走受限执行器的只读命令（「环境信息」里显示 tiup 版本）
	for i, want := range []struct {
		argv []string
		kind string
	}{{[]string{"tiup", "--version"}, xexec.KindTiupVersion}, {[]string{"/home/tidb/.tiup/bin/tiup", "cluster", "--version"}, xexec.KindTiupClusterVer}} {
		if _, err := ex(context.Background(), want.argv, nil); err != nil {
			t.Fatalf("%v: %v", want.argv, err)
		}
		if len(r.got) != 3+i || r.got[2+i].Cmd.Kind != want.kind || r.got[2+i].Cmd.Cluster != "" {
			t.Fatalf("%v 应走 %s: %+v", want.argv, want.kind, r.got)
		}
	}
	// 非 tiup 命令仍直接执行
	if out, err := ex(context.Background(), []string{"hostname", "-I"}, nil); err != nil || string(out) != "direct:hostname -I" || directCalls != 1 {
		t.Fatalf("非 tiup 命令应直接执行: %q %v %d", out, err, directCalls)
	}
	if len(r.got) != 4 {
		t.Errorf("非 tiup 命令不应经过执行器")
	}
}

func TestLocalTiupExecRefusesOtherTiupCommands(t *testing.T) {
	r := &fakeRunner{}
	ex := localTiupExec(r, "tidb-test", func(context.Context, []string, []string) ([]byte, error) {
		t.Fatal("不应直接执行 tiup")
		return nil, nil
	})
	for _, argv := range [][]string{
		{"tiup", "--version", "--verbose"},
		{"tiup", "cluster", "--version", "x"},
		{"tiup", "cluster", "display", "other"}, // 别的集群名：不替它跑
		{"tiup", "cluster", "destroy", "tidb-test"},
	} {
		if _, err := ex(context.Background(), argv, nil); err == nil {
			t.Errorf("%v 应被拒绝", argv)
		}
	}
	if len(r.got) != 0 {
		t.Errorf("被拒绝的命令不应到达执行器: %+v", r.got)
	}
}

func TestLocalTiupExecReportsExecutorFailure(t *testing.T) {
	direct := func(context.Context, []string, []string) ([]byte, error) { return nil, nil }
	r := &fakeRunner{res: xexec.Result{Exit: 1, Output: "Error: boom\n", Err: "退出码 1"}}
	if _, err := localTiupExec(r, "c", direct)(context.Background(), []string{"tiup", "cluster", "display", "c"}, nil); err == nil || !strings.Contains(err.Error(), "退出码 1") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("非 0 退出应带上输出: %v", err)
	}
	r2 := &fakeRunner{err: errors.New("sudo: a password is required")}
	if _, err := localTiupExec(r2, "c", direct)(context.Background(), []string{"tiup", "cluster", "list"}, nil); err == nil || !strings.Contains(err.Error(), "password") {
		t.Errorf("执行器错误应透出: %v", err)
	}
}
