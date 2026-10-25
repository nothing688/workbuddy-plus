//go:build !windows

package selfupdate

import (
	"os"
	"syscall"
)

// IsWindows 见 exec_windows.go。
func IsWindows() bool { return false }

// Exec 用新二进制替换当前进程镜像（PID 不变，容器不会重启，systemd/docker 也不会
// 认为服务退出过）。args 不含 argv[0]，这里自动补上。成功不返回。
func Exec(path string, args []string) error {
	return syscall.Exec(path, append([]string{path}, args...), os.Environ())
}
