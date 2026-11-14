// @author: kevinkelly
// @license: Apache License 2.0
// @file: repair.go
// @time: 2026/10/28

package exec

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	osexec "os/exec"
)

// 自动修复「tiup 组件文件缺失」：
//
// 现象：`tiup ctl:v8.5.8 pd …` 报 "Failed to start component `ctl`: fork/exec …/components/ctl/v8.5.8/ctl: no such file or directory"。
// 组件目录在（tiup 认为已安装），但可执行文件不在（安装被中断、被清理、离线镜像不完整），所有依赖它的操作都会失败。
//
// 做法：只在这个报错特征出现时，对**这条命令自己用到的那个组件版本**执行一次 `tiup uninstall <组件>:<版本>` +
// `tiup install <组件>:<版本>`，然后重试原命令一次。
//   - 报错特征意味着命令根本没有跑起来，没有任何副作用，所以重试是安全的（写命令的一次性令牌已在授权时消耗，不需要新令牌）；
//   - 组件限定为 ctl / br / cdc，版本取自原命令 argv（来自拓扑，不来自请求），格式严格校验；
//   - 同一组件版本 10 分钟内只尝试一次，失败也不重复，避免每条命令都卡几分钟；
//   - 安装需要能访问 tiup 镜像（离线环境需镜像里有该组件），失败时原样返回原报错，并在输出里写明自动修复失败的原因。

// repairComp 匹配 argv[1]，如 ctl:v8.5.8 / br:v8.5.8 / cdc:v8.5.8。
var repairComp = regexp.MustCompile(`^(ctl|br|cdc):(v[0-9][0-9A-Za-z.+-]{0,30})$`)

const (
	repairCooldown = 10 * time.Minute
	repairUninstTO = 60 * time.Second
	repairInstTO   = 5 * time.Minute
)

var repairLast = struct {
	mu sync.Mutex
	at map[string]time.Time
}{at: map[string]time.Time{}}

// repairTarget 判断一次失败的命令是否属于「组件文件缺失」，是则返回组件名与版本。
func repairTarget(argv []string, output string) (comp, ver string, ok bool) {
	if len(argv) < 2 {
		return "", "", false
	}
	m := repairComp.FindStringSubmatch(argv[1])
	if m == nil {
		return "", "", false
	}
	low := strings.ToLower(output)
	if !strings.Contains(low, "failed to start component `"+m[1]+"`") || !strings.Contains(low, "no such file or directory") {
		return "", "", false
	}
	return m[1], m[2], true
}

// repairAllowed 在冷却期内返回 false；允许时登记本次尝试时间。
func repairAllowed(key string, now time.Time) bool {
	repairLast.mu.Lock()
	defer repairLast.mu.Unlock()
	if t, ok := repairLast.at[key]; ok && now.Sub(t) < repairCooldown {
		return false
	}
	repairLast.at[key] = now
	return true
}

// runQuiet 执行一条固定 argv，输出逐行交给 say（带前缀），返回合并输出与错误。
func runQuiet(ctx context.Context, argv []string, timeout time.Duration, say func(string)) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ob := newOutBuf(func(l string) { say("  " + l) }, nil)
	cmd := osexec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Env = minimalEnv()
	cmd.Stdout, cmd.Stderr = ob, ob
	cmd.WaitDelay = 3 * time.Second
	err := cmd.Run()
	ob.flush()
	if err != nil && cctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("超过 %s 未完成", timeout)
	}
	return ob.String(), err
}

// repairComponent 重新安装缺失文件的组件。返回是否可以重试原命令；notes 是要并入输出的说明行。
func (r DirectRunner) repairComponent(ctx context.Context, tiup, comp, ver string, say func(string)) bool {
	spec := comp + ":" + ver
	if !repairAllowed(spec, r.now()) {
		say("[自动修复] " + spec + " 在 10 分钟内已尝试过自动修复，这次不再重试；请在中控机上手动执行：tiup uninstall " + spec + " && tiup install " + spec)
		return false
	}
	r.logf("exec repair start component=%s", spec)
	say("[自动修复] 组件 " + spec + " 的可执行文件缺失，正在重新安装（tiup uninstall → tiup install）")
	if _, err := runQuiet(ctx, []string{tiup, "uninstall", spec}, repairUninstTO, say); err != nil {
		say("[自动修复] 卸载未成功（继续尝试安装）：" + err.Error())
	}
	if _, err := runQuiet(ctx, []string{tiup, "install", spec}, repairInstTO, say); err != nil {
		r.logf("exec repair failed component=%s err=%v", spec, err)
		say("[自动修复] 重新安装失败：" + err.Error() + "。离线环境需要镜像里包含 " + spec + "；请在中控机上手动处理：tiup install " + spec)
		return false
	}
	r.logf("exec repair ok component=%s", spec)
	say("[自动修复] " + spec + " 已重新安装，重试原命令")
	return true
}
