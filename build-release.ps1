# 打包 TiDB Ops 发布包（在 Windows 开发机上运行，产物是 Linux tar.gz）。
# 用法：
#   .\build-release.ps1                              # 版本取 VERSION 文件，架构 amd64+arm64，输出到 backend\dist\release
#   .\build-release.ps1 -Version 1.2.0 -Arch amd64
#   .\build-release.ps1 -Version 1.2.0 -Notes notes.md -MinFrom 1.0.0 -Out D:\release
#   .\build-release.ps1 -Verify backend\dist\release\tidbops-1.2.0-linux-amd64.tar.gz
param(
  [string]$Version = "",
  [string]$Arch = "amd64,arm64",
  [string]$Out = "",
  [string]$Notes = "",
  [string]$MinFrom = "",
  [string]$DeployTool = "",
  [string]$Verify = ""
)
$ErrorActionPreference = "Stop"
$root = $PSScriptRoot
$backend = Join-Path $root "backend"
if (-not $env:GOTOOLCHAIN) { $env:GOTOOLCHAIN = "local" }
if (-not $env:GOFLAGS) { $env:GOFLAGS = "-mod=mod" }
if (-not $env:GOPROXY) { $env:GOPROXY = "off" }

Push-Location $backend
try {
  if ($Verify) {
    go run ./cmd/tidbops-pack --verify (Resolve-Path $Verify).Path
    if ($LASTEXITCODE -ne 0) { throw "校验失败" }
    return
  }
  if (-not $Out) { $Out = Join-Path $backend "dist\release" }
  $argv = @("run", "./cmd/tidbops-pack", "--web", $root, "--arch", $Arch, "--out", $Out)
  if ($Version)    { $argv += @("--version", $Version) }
  if ($Notes)      { $argv += @("--notes", (Resolve-Path $Notes).Path) }
  if ($MinFrom)    { $argv += @("--min-from", $MinFrom) }
  if ($DeployTool) { $argv += @("--deploy-tool", (Resolve-Path $DeployTool).Path) }
  & go @argv
  if ($LASTEXITCODE -ne 0) { throw "打包失败（退出码 $LASTEXITCODE）" }
} finally {
  Pop-Location
}
