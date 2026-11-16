//go:build windows

package main

import "syscall"

// hiddenSysProcAttr 让子进程不弹出控制台窗口
func hiddenSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
