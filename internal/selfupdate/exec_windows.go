//go:build windows

package selfupdate

import "fmt"

// IsWindows 决定安装文件的扩展名（.exe）。
func IsWindows() bool { return true }

// Exec Windows 上无法像 unix 那样原进程换镜像（运行中的 exe 还被占用），
// 只把新版本落盘，由用户在方便时重启进程；启动逻辑会优先运行新版本。
func Exec(path string, args []string) error {
	return fmt.Errorf("Windows 不支持原地重启，新版本已下载：请手动重启进程（%s）", path)
}
