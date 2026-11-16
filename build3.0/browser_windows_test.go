//go:build windows

package main

import (
	"os"
	"strings"
	"testing"
)

// 覆盖注册表里各种浏览器命令模板的解析
func TestParseWindowsCommand(t *testing.T) {
	const url = "http://localhost:8080"

	cases := []struct {
		name        string
		command     string
		wantProgram string
		wantArgs    []string
		wantOK      bool
	}{
		{
			name:        "chrome 默认浏览器模板",
			command:     `"C:\Program Files\Google\Chrome\Application\chrome.exe" --single-argument %1`,
			wantProgram: `C:\Program Files\Google\Chrome\Application\chrome.exe`,
			wantArgs:    []string{"--single-argument", url},
			wantOK:      true,
		},
		{
			name:        "edge 无附加参数",
			command:     `"C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe" %1`,
			wantProgram: `C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
			wantArgs:    []string{url},
			wantOK:      true,
		},
		{
			name:        "firefox 参数在前网址在后",
			command:     `"C:\Program Files\Mozilla Firefox\firefox.exe" -osint -url %1`,
			wantProgram: `C:\Program Files\Mozilla Firefox\firefox.exe`,
			wantArgs:    []string{"-osint", "-url", url},
			wantOK:      true,
		},
		{
			name:        "丢弃 %* 之类的多余占位符",
			command:     `"C:\browser.exe" %1 %*`,
			wantProgram: `C:\browser.exe`,
			wantArgs:    []string{url},
			wantOK:      true,
		},
		{
			name:        "没有引号且路径不含空格",
			command:     `C:\browser.exe --open %1`,
			wantProgram: `C:\browser.exe`,
			wantArgs:    []string{"--open", url},
			wantOK:      true,
		},
		{
			name:        "命令里没有网址占位符时补上网址",
			command:     `"C:\browser.exe"`,
			wantProgram: `C:\browser.exe`,
			wantArgs:    []string{url},
			wantOK:      true,
		},
		{
			name:    "空命令",
			command: "   ",
			wantOK:  false,
		},
		{
			name:    "引号不闭合",
			command: `"C:\browser.exe --open %1`,
			wantOK:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			program, args, ok := parseWindowsCommand(tc.command, url)
			if ok != tc.wantOK {
				t.Fatalf("parseWindowsCommand() ok = %v, 期望 %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if program != tc.wantProgram {
				t.Errorf("program = %q, 期望 %q", program, tc.wantProgram)
			}
			if strings.Join(args, "\x00") != strings.Join(tc.wantArgs, "\x00") {
				t.Errorf("args = %q, 期望 %q", args, tc.wantArgs)
			}
		})
	}
}

// 模拟 reg.exe 在中文系统隐藏控制台下的真实输出：
// 默认值标记是 GBK 字节，且 "REG_SZ" 不在行首
func TestRegSZValueEncodings(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "中文系统 GBK 输出、值带引号",
			out:  "\r\nHKEY_LOCAL_MACHINE\\SOFTWARE\\Clients\\StartMenuInternet\\Google Chrome\\shell\\open\\command\r\n    (\xc4\xac\xc8\xcf)    REG_SZ    \"C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe\"\r\n\r\n",
			want: `"C:\Program Files\Google\Chrome\Application\chrome.exe"`,
		},
		{
			name: "英文系统输出、带启动参数",
			out:  "\r\nHKEY_CLASSES_ROOT\\ChromeHTML\\shell\\open\\command\r\n    (Default)    REG_SZ    \"C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe\" --single-argument %1\r\n\r\n",
			want: `"C:\Program Files\Google\Chrome\Application\chrome.exe" --single-argument %1`,
		},
		{
			name: "值未设置",
			out:  "\r\nHKEY_CURRENT_USER\\Software\\Microsoft\\Windows\\Shell\\Associations\\UrlAssociations\\http\\UserChoice\r\n    (Default)    REG_SZ    (value not set)\r\n\r\n",
			want: "",
		},
		{
			name: "值未设置（中文系统 GBK 输出）",
			out:  "\r\nHKEY_CURRENT_USER\\Software\\Microsoft\\Windows\\Shell\\Associations\\UrlAssociations\\http\\UserChoice\r\n    (\xc4\xac\xc8\xcf)    REG_SZ    (\xca\xfd\xd6\xb5\xce\xb4\xc9\xe8\xd6\xc3)\r\n\r\n",
			want: "",
		},
		{
			name: "键不存在时没有任何输出",
			out:  "",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := regSZValue([]byte(tc.out)); got != tc.want {
				t.Errorf("regSZValue() = %q, 期望 %q", got, tc.want)
			}
		})
	}
}

func TestBrowserPreference(t *testing.T) {
	if browserPreference("Google Chrome") >= browserPreference("Microsoft Edge") {
		t.Error("Chrome 应优先于 Edge")
	}
	if browserPreference("Microsoft Edge") >= browserPreference("IEXPLORE.EXE") {
		t.Error("Edge 应优先于 IE")
	}
}

// 针对「读默认浏览器」这个修复的回归测试：
// UserChoice 里的 ProgId 是命名值，必须用 /v ProgId 才读得到。
// 之前用 /ve 读默认值永远为空，主路径静默失效、退化成枚举已安装浏览器。
func TestUserChoiceProgId(t *testing.T) {
	const key = `HKEY_CURRENT_USER\Software\Microsoft\Windows\Shell\Associations\UrlAssociations\http\UserChoice`

	byName := regQueryValue(key, "ProgId")
	byDefault := regQuery(key)
	t.Logf("命名值 /v ProgId = %q", byName)
	t.Logf("默认值 /ve       = %q", byDefault)

	if byName == "" {
		t.Skip("本机读不到 UserChoice\\ProgId（可能无注册表访问权限），跳过")
	}
	if byDefault != "" && byDefault == byName {
		t.Logf("注意：本机默认值与命名值相同，无法区分两种读法")
	}
	// 读到的东西必须像个 ProgId
	if strings.ContainsAny(byName, `\/`) || !isASCII(byName) {
		t.Errorf("ProgId 形状异常: %q", byName)
	}
}

// 在真实注册表上验证能解析出可执行的浏览器命令
func TestDirectBrowserCommand(t *testing.T) {
	const url = "http://localhost:8080"

	t.Logf("默认浏览器 ProgId（命名值）= %q",
		regQueryValue(`HKEY_CURRENT_USER\Software\Microsoft\Windows\Shell\Associations\UrlAssociations\http\UserChoice`, "ProgId"))
	t.Logf("已注册浏览器 = %q", regEnumSubKeys(`HKEY_LOCAL_MACHINE\SOFTWARE\Clients\StartMenuInternet`))

	candidates := windowsDirectBrowserCommands(url)
	if len(candidates) == 0 {
		t.Skip("本机未解析出浏览器启动命令，运行时会退回 rundll32/explorer")
	}

	candidate := candidates[0]
	t.Logf("解析到启动方式: %s %q", candidate.program, candidate.args)

	if _, err := os.Stat(candidate.program); err != nil {
		t.Errorf("解析出的浏览器路径不存在: %s", candidate.program)
	}
	if joined := strings.Join(candidate.args, " "); !strings.Contains(joined, url) {
		t.Errorf("启动参数里没有带上网址: %q", candidate.args)
	}
}
