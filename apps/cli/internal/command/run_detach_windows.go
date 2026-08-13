//go:build windows

package command

import "syscall"

func detachSysProcAttr() *syscall.SysProcAttr {
	return nil
}
