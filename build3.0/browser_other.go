//go:build !windows

package main

// windowsDirectBrowserCommands 在非 Windows 系统上没有注册表可查，直接返回空
func windowsDirectBrowserCommands(rawURL string) []browserCommand {
	return nil
}
