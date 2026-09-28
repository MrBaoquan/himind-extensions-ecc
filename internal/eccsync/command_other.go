//go:build !windows

package eccsync

import "os/exec"

func configureHiddenCommand(_ *exec.Cmd) {}
