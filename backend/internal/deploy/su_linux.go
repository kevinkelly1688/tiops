// @author: kevinkelly
// @license: Apache License 2.0
// @file: su_linux.go
// @time: 2026/10/28

//go:build linux

package deploy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// SuRunner 用 `su - root -c <cmdline>` 取得 root。su 要从控制终端读口令，所以给它开一个伪终端（pty），
// 见到口令提示后把口令写进去。口令只写一次，写完立即清零；伪终端在输入口令时是关闭回显的。
type SuRunner struct {
	Su      string        // su 的路径，默认 /usr/bin/su（找不到再试 /bin/su）
	Timeout time.Duration // 整个过程的超时，默认 30 秒
}

// DefaultRootRunner 返回生产用的 root 执行器。
func DefaultRootRunner() RootRunner { return SuRunner{} }

func ioctl(fd, req, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); e != 0 {
		return e
	}
	return nil
}

func openPTY() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var n uint32
	if err := ioctl(m.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		m.Close()
		return nil, nil, err
	}
	var unlock int32
	if err := ioctl(m.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		m.Close()
		return nil, nil, err
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return m, s, nil
}

// RunAsRoot 实现 RootRunner。
func (r SuRunner) RunAsRoot(ctx context.Context, cmdline string, password []byte) (string, error) {
	defer zero(password)
	su := r.Su
	if su == "" {
		su = "/usr/bin/su"
		if _, err := os.Stat(su); err != nil {
			su = "/bin/su"
		}
	}
	to := r.Timeout
	if to <= 0 {
		to = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()

	master, slave, err := openPTY()
	if err != nil {
		return "", fmt.Errorf("无法创建伪终端：%w", err)
	}
	defer master.Close()
	cmd := exec.CommandContext(ctx, su, "-", "root", "-c", cmdline)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "LANG=C", "TERM=dumb"}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		slave.Close()
		return "", fmt.Errorf("无法启动 su：%w", err)
	}
	slave.Close() // 父进程不再持有从端：子进程退出后主端读到 EOF / EIO

	var (
		mu   sync.Mutex
		buf  bytes.Buffer
		sent bool
		done = make(chan struct{})
	)
	go func() {
		defer close(done)
		tmp := make([]byte, 4096)
		for {
			n, err := master.Read(tmp)
			if n > 0 {
				mu.Lock()
				buf.Write(tmp[:n])
				if !sent && IsPasswordPrompt(buf.String()) {
					sent = true
					line := append(append([]byte{}, password...), '\n')
					_, _ = master.Write(line)
					zero(line)
					buf.Reset() // 提示本身没有价值，丢掉；之后只保留 su / 命令的输出
				}
				mu.Unlock()
			}
			if err != nil { // 子进程退出后读 pty 主端会得到 EIO（或 EOF），都是正常结束
				return
			}
		}
	}()

	waitErr := cmd.Wait()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if ctx.Err() != nil && waitErr != nil {
		return CleanOutput(out), fmt.Errorf("su 超时（%s）：%w", to, ctx.Err())
	}
	return CleanOutput(out), ClassifySu(out, waitErr)
}
