//go:build !windows

package main

import "syscall"

// hiddenSysProcAttr 非 Windows 系统无需隐藏窗口
func hiddenSysProcAttr() *syscall.SysProcAttr {
	return nil
}
