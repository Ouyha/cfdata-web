package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// ----------------------- 自动打开浏览器 -----------------------
//
// 服务监听成功后，自动调用系统默认浏览器打开网页。
// 如需关闭该行为：启动参数加 -open-browser=false，或设置环境变量 CFD_NO_BROWSER=1
// 环境变量优先：只要 CFD_NO_BROWSER=1，即使 -open-browser=true 也不会自动打开。

// browserCommand 描述一种"打开 URL"的调用方式
type browserCommand struct {
	program string
	args    []string
}

// browserCandidates 返回当前系统下按优先级排列的打开方式
func browserCandidates(rawURL string) []browserCommand {
	switch runtime.GOOS {
	case "windows":
		// 优先按注册表里登记的默认浏览器直接启动，最确定；
		// 其次用 URL 协议处理程序；最后交给资源管理器。
		candidates := windowsDirectBrowserCommands(rawURL)
		return append(candidates,
			browserCommand{"rundll32.exe", []string{"url.dll,FileProtocolHandler", rawURL}},
			browserCommand{"explorer.exe", []string{rawURL}},
		)
	case "darwin":
		return []browserCommand{
			{"open", []string{rawURL}},
		}
	default:
		return []browserCommand{
			{"xdg-open", []string{rawURL}},
			{"gio", []string{"open", rawURL}},
			{"sensible-browser", []string{rawURL}},
		}
	}
}

// openBrowserURL 调用系统默认浏览器打开地址，返回实际生效的调用方式
func openBrowserURL(rawURL string) (string, error) {
	var attempts []string
	for _, candidate := range browserCandidates(rawURL) {
		cmd := exec.Command(candidate.program, candidate.args...)
		if runtime.GOOS == "windows" {
			// 隐藏子进程窗口，避免弹出命令行黑框
			cmd.SysProcAttr = hiddenSysProcAttr()
		}
		if err := cmd.Start(); err != nil {
			attempts = append(attempts, fmt.Sprintf("%s: %v", candidate.program, err))
			continue
		}
		// 交给系统处理，不等待浏览器退出
		go func(c *exec.Cmd) { _ = c.Wait() }(cmd)
		return strings.Join(append([]string{candidate.program}, candidate.args...), " "), nil
	}
	if len(attempts) == 0 {
		return "", fmt.Errorf("没有可用的浏览器启动方式")
	}
	return "", fmt.Errorf("未能调用系统浏览器（%s）", strings.Join(attempts, "; "))
}

// scheduleBrowserOpen 等监听就绪后打开浏览器，避免打开过早出现"无法访问此网站"
func scheduleBrowserOpen(rawURL string, delay time.Duration) {
	go func() {
		if delay > 0 {
			time.Sleep(delay)
		}
		if used, err := openBrowserURL(rawURL); err != nil {
			fmt.Printf("自动打开浏览器失败: %v\n", err)
			fmt.Printf("请手动在浏览器中访问: %s\n", rawURL)
		} else {
			fmt.Printf("已自动打开默认浏览器: %s\n", used)
		}
	}()
}
