// @author: kevinkelly
// @license: Apache License 2.0
// @file: serve.go
// @time: 2026/10/28

package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// 特权分离的 JSON 行协议（exec --stdin 子进程 ↔ SudoRunner）：
//
//	stdin ：一个 JSON Request
//	stdout：若干 {"t":"out","line":"..."}，最后一行 {"t":"result","exit":..,"output":..,"err":..,"millis":..}
//
// 校验失败 / 令牌无效 / 请求格式错误同样以 result 输出（exit=-1，err 为文案）；此时 Serve 返回 nil。
// 输出里不会出现令牌、拓扑内容或任何凭据（Request 不会被回显）。

type outMsg struct {
	T    string `json:"t"`
	Line string `json:"line"`
}

type resultMsg struct {
	T string `json:"t"`
	Result
}

const maxRequestBytes = 1 << 20

// Serve 读取一个 JSON Request，交给 runner 执行并按协议输出。只在写 out 失败等 I/O 问题时返回错误。
func Serve(ctx context.Context, in io.Reader, out io.Writer, runner Runner) error {
	var mu sync.Mutex
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	var werr error
	write := func(v any) {
		mu.Lock()
		defer mu.Unlock()
		if werr == nil {
			werr = enc.Encode(v)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var req Request
	dec := json.NewDecoder(io.LimitReader(in, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		write(resultMsg{"result", Result{Exit: -1, Err: "请求格式错误: " + err.Error()}})
		return werr
	}
	if dec.More() {
		write(resultMsg{"result", Result{Exit: -1, Err: "请求格式错误: 含多余内容"}})
		return werr
	}

	res, err := safeRun(ctx, runner, req, func(line string) {
		write(outMsg{"out", line})
		mu.Lock()
		failed := werr != nil
		mu.Unlock()
		if failed {
			cancel() // 读端已断开，终止执行
		}
	})
	if err != nil {
		res.Exit = -1
		if res.Err == "" {
			res.Err = err.Error()
		}
	}
	write(resultMsg{"result", res})
	return werr
}

func safeRun(ctx context.Context, r Runner, req Request, emit func(string)) (res Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			res, err = Result{Exit: -1}, fmt.Errorf("执行器内部错误")
		}
	}()
	return r.Run(ctx, req, emit)
}
