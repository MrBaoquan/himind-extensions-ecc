//go:build windows

package eccsync

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

// configureHiddenCommand 让同步链路拉起的 go / git / gh 不弹控制台窗口。
//
// 定时计划是在后台跑的：一条命令闪一下黑框，用户会以为程序出了问题。
func configureHiddenCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}
