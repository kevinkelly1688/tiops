# tidbops backend（Go）

TiDB 运维平台后端。目前包含 B7 的内容：只读采集、样例数据包、回放。

```
cmd/tidbops-fixtures/   命令行：export / verify / list / show / demo
internal/source/        Source 接口；Live / Replay / Recorder；只读校验；采集目录；tiup display 解析
internal/fixtures/      数据包格式（manifest + data）、哈希校验、脱敏
internal/fakecluster/   进程内模拟集群（测试与 demo）
```

## 开发

> 本机 Go 配置里 `GOTOOLCHAIN` 异常，命令前加 `GOTOOLCHAIN=local`。

```bash
GOTOOLCHAIN=local go vet ./...
GOTOOLCHAIN=local go test ./...
# 构建（Windows / Linux）
GOTOOLCHAIN=local go build -o dist/tidbops-fixtures.exe ./cmd/tidbops-fixtures
GOTOOLCHAIN=local GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/tidbops-fixtures-linux-amd64 ./cmd/tidbops-fixtures
# 生成演示数据包
dist/tidbops-fixtures.exe demo --out ../fixtures/demo
```

只使用标准库，无第三方依赖。各子命令的用法见 `tidbops-fixtures -h`。
