// @author: kevinkelly
// @license: Apache License 2.0
// @file: exec_glue.go
// @time: 2026/10/28

package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"tidbops/internal/change"
	"tidbops/internal/exec"
)

// ExecTopo 用最近一次快照为模拟执行器提供拓扑（回放 / 演示用；真实环境由 exec.DisplayTopo 通过 tiup 取得）。
func (s *Server) ExecTopo() exec.TopoFunc {
	return func(ctx context.Context, cluster string) (exec.Topo, error) {
		snap := s.Snapshot(ctx, false)
		t := exec.Topo{Version: snap.Topology.Version, Nodes: map[string]string{}, Status: map[string]string{}, User: snap.Topology.User, SSHPort: 22, SQLPort: 4000}
		if t.User == "" {
			t.User = "tidb"
		}
		for _, in := range snap.Topology.Instances {
			a := fmt.Sprintf("%s:%d", in.Host, in.Port)
			t.Nodes[a] = in.Type
			sp := in.StatusPort
			switch {
			case sp > 0:
			case in.Type == "tikv":
				sp = in.Port + 20
			case in.Type == "tidb":
				sp = 10080
			default:
				sp = in.Port
			}
			t.Status[fmt.Sprintf("%s:%d", in.Host, sp)] = a
			if in.Type == "tidb" && t.SQLHost == "" {
				t.SQLHost, t.SQLPort = in.Host, in.Port
			}
			if in.Type == "pd" && t.PD == "" && (in.Status == "up" || in.Status == "warn") {
				t.PD = "http://" + a
			}
		}
		return t, nil
	}
}

// NewPDProber 返回通过 PD HTTP API 与 tiup cluster display 探测状态的 Prober。
func (s *Server) NewPDProber(r exec.Runner) change.Prober {
	cluster := s.cfg.Col.Params.Cluster
	cl := &http.Client{Timeout: 10 * time.Second}
	return change.PDProber{
		PD: func() string {
			t, _ := s.ExecTopo()(context.Background(), cluster)
			return t.PD
		},
		Get: func(ctx context.Context, url string) ([]byte, int, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return nil, 0, err
			}
			resp, err := cl.Do(req)
			if err != nil {
				return nil, 0, err
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			return b, resp.StatusCode, err
		},
		Display: func(ctx context.Context) (string, error) {
			var sb strings.Builder
			res, err := r.Run(ctx, exec.Request{Cmd: exec.Cmd{Kind: exec.KindClusterDisplay, Cluster: cluster}}, func(l string) { sb.WriteString(l + "\n") })
			if err != nil {
				return "", err
			}
			if res.Exit != 0 {
				return "", fmt.Errorf("tiup cluster display 退出码 %d", res.Exit)
			}
			return sb.String(), nil
		},
	}
}
