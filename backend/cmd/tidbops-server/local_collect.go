// @author: kevinkelly
// @license: Apache License 2.0
// @file: local_collect.go
// @time: 2026/10/28

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	xexec "tidbops/internal/exec"
	"tidbops/internal/source"
)

// localTiupExec 是中控机模式下「本机 tiup 集群」的采集命令执行函数。
//
// 平台的 Web 进程以 tidbops 账号运行：PATH 里没有 tiup，也进不去 tidb 用户的家目录（0700），所以不能像同机部署那样直接
// 执行 tiup。采集目录里需要 tiup 的只读命令（tiup cluster display <集群> / tiup cluster list / tiup --version / tiup cluster --version）改走受限执行器
// （sudo -n -u tidb … exec --stdin，与导入、运维用的是同一个入口，只放行固定的只读命令）；其他命令（hostname、uname 等）
// 仍由本进程直接执行。其他 tiup 命令没有对应的执行器命令，直接报明确的错误，页面上显示为缺失项。
func localTiupExec(r xexec.Runner, cluster string, direct source.ExecFunc) source.ExecFunc {
	return func(ctx context.Context, argv []string, env []string) ([]byte, error) {
		if len(argv) == 0 || filepath.Base(argv[0]) != "tiup" {
			return direct(ctx, argv, env)
		}
		var cmd xexec.Cmd
		switch {
		case len(argv) == 4 && argv[1] == "cluster" && argv[2] == "display" && argv[3] == cluster:
			cmd = xexec.Cmd{Kind: xexec.KindClusterDisplay, Cluster: cluster}
		case len(argv) == 3 && argv[1] == "cluster" && argv[2] == "list":
			cmd = xexec.Cmd{Kind: xexec.KindClusterList}
		case len(argv) == 2 && argv[1] == "--version":
			cmd = xexec.Cmd{Kind: xexec.KindTiupVersion}
		case len(argv) == 3 && argv[1] == "cluster" && argv[2] == "--version":
			cmd = xexec.Cmd{Kind: xexec.KindTiupClusterVer}
		default:
			return nil, fmt.Errorf("本进程不能直接执行 tiup（平台账号没有它），这条命令也没有对应的受限执行器命令：%s", strings.Join(argv, " "))
		}
		res, err := r.Run(ctx, xexec.Request{Cmd: cmd}, nil)
		if err != nil {
			return nil, fmt.Errorf("经执行器运行 %s 失败: %w", strings.Join(argv, " "), err)
		}
		if res.Exit != 0 {
			msg := res.Err
			if msg == "" {
				msg = fmt.Sprintf("退出码 %d", res.Exit)
			}
			return nil, fmt.Errorf("经执行器运行 %s 失败: %s: %s", strings.Join(argv, " "), msg, strings.TrimSpace(res.Output))
		}
		return []byte(res.Output), nil
	}
}
