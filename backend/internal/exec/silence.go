// @author: kevinkelly
// @license: Apache License 2.0
// @file: silence.go
// @time: 2026/10/28

package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// runHTTP 执行 alert.silence：直接对 Alertmanager 发起 POST，不经 shell、不使用环境代理、不跟随重定向。
func (r DirectRunner) runHTTP(ctx context.Context, req Request, plan Plan, start time.Time, emit func(string)) (Result, error) {
	h := plan.HTTP
	timeout := timeoutFor(r.Pol, req.Cmd.Kind)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	hreq, err := http.NewRequestWithContext(cctx, h.Method, h.URL, bytes.NewReader(h.Body(r.now(), req.Actor)))
	if err != nil {
		return failResult(start, fmt.Errorf("构造请求失败"))
	}
	hreq.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		Timeout:       timeout,
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	r.logf("exec start kind=%s cluster=%s actor=%s change=%s cmd=%s", req.Cmd.Kind, req.Cmd.Cluster, truncate(req.Actor, 64), req.ChangeID, plan)
	resp, err := client.Do(hreq)
	if err != nil {
		var msg string
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			msg = fmt.Sprintf("请求超过 %s，已终止", timeout)
		} else {
			msg = "请求 Alertmanager 失败: " + truncate(err.Error(), 200)
		}
		res := Result{Exit: -1, Err: msg, Millis: time.Since(start).Milliseconds()}
		return res, errors.New(msg)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	ob := newOutBuf(emit, nil)
	res := Result{}
	if resp.StatusCode/100 == 2 {
		var out struct {
			SilenceID string `json:"silenceID"`
		}
		_ = json.Unmarshal(body, &out)
		_, _ = ob.Write([]byte("静默已创建 silenceID=" + logSafe(out.SilenceID) + "\n"))
	} else {
		_, _ = ob.Write([]byte(fmt.Sprintf("Alertmanager 返回 HTTP %d\n%s\n", resp.StatusCode, strings.TrimSpace(string(body)))))
		res.Exit, res.Err = 1, fmt.Sprintf("HTTP 状态 %d", resp.StatusCode)
	}
	ob.flush()
	res.Output = ob.String()
	res.Millis = time.Since(start).Milliseconds()
	r.logf("exec end kind=%s cluster=%s change=%s exit=%d millis=%d", req.Cmd.Kind, req.Cmd.Cluster, req.ChangeID, res.Exit, res.Millis)
	return res, nil
}
