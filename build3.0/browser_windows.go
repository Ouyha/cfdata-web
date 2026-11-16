//go:build windows

package main

import (
	"bytes"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// 注册表标识符在各语言版本下都是 ASCII，可安全地按字节匹配。
var (
	regSZMarker  = []byte("REG_SZ")
	hiveMarkers  = [][]byte{[]byte("HKEY_"), []byte("HKEY")}
	notSetMarker = []byte("(value not set)")
)

// regQuery 读取注册表某个键的默认值，失败或未设置时返回空字符串
//
// 注意三点：
//  1. 必须带 /ve，否则 reg.exe 只列出子键，不会给出默认值；
//     但 /ve 只能读到「默认值」，命名值要用 regQueryValue；
//  2. 必须写完整的 HKEY_CURRENT_USER / HKEY_LOCAL_MACHINE，
//     中文等本地化系统上 reg.exe 不接受 HKCU/HKLM 缩写；
//  3. 隐藏控制台后 reg.exe 会按系统 OEM 代码页（中文系统是 GBK）输出，
//     所以不能用 UTF-8 按行解析，这里按字节查找 "REG_SZ" 标记。
func regQuery(key string) string {
	out, ok := runRegQuery("reg.exe", "query", key, "/ve")
	if !ok {
		return ""
	}
	return regSZValue(out)
}

// regQueryValue 读取注册表某个键下的「命名值」，读不到返回空字符串
//
// /v 指定值名。Win8 以后 UserChoice 键里的默认浏览器是命名值 ProgId，
// 而不是默认值，所以这里必须用 /v ProgId。
func regQueryValue(key, name string) string {
	out, ok := runRegQuery("reg.exe", "query", key, "/v", name)
	if !ok {
		return ""
	}
	return regSZValue(out)
}

// regSZValue 从 reg.exe 输出里取出 REG_SZ 的值
//
// 只去掉行尾空白，保留原有引号：命令行模板形如
//
//	"C:\Program Files\Google\Chrome\Application\chrome.exe" --single-argument %1
//
// 引号是路径含空格时的必要信息，去掉后反而解析不出程序路径。
func regSZValue(out []byte) string {
	// 未设置时输出为“(value not set)”，中文系统是“(数值未设置)”，
	// 前缀随语言变化，只判断英文整体标记与“整段被圆括号包住”这个共同特征
	if bytes.Contains(out, notSetMarker) {
		return ""
	}
	idx := bytes.Index(out, regSZMarker)
	if idx < 0 {
		return ""
	}
	value := out[idx+len(regSZMarker):]
	if end := bytes.IndexByte(value, '\n'); end >= 0 {
		value = value[:end]
	}
	trimmed := strings.Trim(string(bytes.Trim(value, " \t\r")), " \t\r")
	if strings.HasPrefix(trimmed, "(") && strings.HasSuffix(trimmed, ")") {
		return ""
	}
	return trimmed
}

// regEnumSubKeys 列出注册表某个键下的子键名，失败时返回空列表
func regEnumSubKeys(root string) []string {
	out, ok := runRegQuery("reg.exe", "query", root)
	if !ok {
		return nil
	}

	root = strings.TrimRight(root, `\`)
	var names []string
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.Trim(line, " \t\r")
		if !hasHivePrefix(line) {
			continue
		}
		if len(line) <= len(root)+1 {
			continue
		}
		if !strings.EqualFold(string(line[:len(root)]), root) || line[len(root)] != '\\' {
			continue
		}
		name := strings.TrimSpace(string(line[len(root)+1:]))
		// 只保留直接子键，跳过键自身与更深的层级
		if name == "" || strings.Contains(name, `\`) {
			continue
		}
		names = append(names, name)
	}
	return names
}

// hasHivePrefix 判断一行是否是注册表键路径（HKEY_ 纯 ASCII，不受代码页影响）
func hasHivePrefix(line []byte) bool {
	for _, marker := range hiveMarkers {
		if bytes.HasPrefix(line, marker) {
			return true
		}
	}
	return false
}

// runRegQuery 执行 reg.exe 查询，成功返回原始输出
func runRegQuery(name string, args ...string) ([]byte, bool) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = hiddenSysProcAttr()
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	return out, true
}

// directBrowserCommand 读取系统登记的默认浏览器打开命令，生成直接启动方式
//
// 默认浏览器在注册表里的写法举例：
//
//	HKCU\...\UrlAssociations\http\UserChoice -> 命名值 ProgId = ChromeHTML
//	HKCR\ChromeHTML\shell\open\command       -> "chrome.exe" --single-argument %1
//
// 把其中的 %1 换成实际网址后直接启动，不经过 shell 转发，最稳定。
//
// 注意：Win8 以后 UserChoice 里的 ProgId 是**命名值**，不是默认值。
// 用 /ve 读默认值永远拿到空，主路径会静默失效、退化成枚举已安装浏览器，
// 结果就是「默认浏览器是 Firefox 却打开了 Chrome」。所以这里必须用 /v ProgId。
func directBrowserCommand(rawURL string) (browserCommand, bool) {
	for _, key := range []string{
		`HKEY_CURRENT_USER\Software\Microsoft\Windows\Shell\Associations\UrlAssociations\http\UserChoice`,
		`HKEY_LOCAL_MACHINE\Software\Microsoft\Windows\Shell\Associations\UrlAssociations\http\UserChoice`,
	} {
		// 先按命名值读（正确姿势），再退回默认值（兼容极老的写法）
		progID := regQueryValue(key, "ProgId")
		if progID == "" {
			progID = regQuery(key)
		}
		// ProgId 只会是 ASCII 标识；含路径分隔符或非 ASCII 时说明读到的不是 ProgId
		if progID == "" || strings.ContainsAny(progID, `\/`) || !isASCII(progID) {
			continue
		}
		if candidate, ok := commandFromProgID(progID, rawURL); ok {
			return candidate, true
		}
	}
	return installedBrowserCommand(rawURL)
}

// isASCII 判断字符串是否全为 ASCII（reg.exe 在中文系统上按 GBK 输出，
// 非 ASCII 字节无法安全地拼回注册表路径）
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// commandFromProgID 按 ProgID 找到浏览器打开命令
func commandFromProgID(progID, rawURL string) (browserCommand, bool) {
	for _, root := range []string{
		`HKEY_CURRENT_USER\Software\Classes\`,
		`HKEY_LOCAL_MACHINE\Software\Classes\`,
	} {
		command := regQuery(root + progID + `\shell\open\command`)
		if command == "" {
			continue
		}
		program, args, ok := parseWindowsCommand(command, rawURL)
		if !ok {
			continue
		}
		return browserCommand{program: program, args: args}, true
	}
	return browserCommand{}, false
}

// installedBrowserCommand 在默认浏览器信息读不到时，
// 从系统已注册的浏览器列表里挑一个（优先 Chrome、Edge）直接启动。
func installedBrowserCommand(rawURL string) (browserCommand, bool) {
	var names []string
	for _, root := range []string{
		`HKEY_LOCAL_MACHINE\SOFTWARE\Clients\StartMenuInternet`,
		`HKEY_CURRENT_USER\SOFTWARE\Clients\StartMenuInternet`,
	} {
		for _, name := range regEnumSubKeys(root) {
			if !isASCII(name) {
				continue
			}
			names = append(names, name)
		}
	}

	// 排序让 Chrome、Edge 优先尝试
	sort.SliceStable(names, func(i, j int) bool {
		return browserPreference(names[i]) < browserPreference(names[j])
	})

	for _, name := range names {
		clientKey := `\SOFTWARE\Clients\StartMenuInternet\` + name + `\shell\open\command`
		for _, root := range []string{"HKEY_LOCAL_MACHINE", "HKEY_CURRENT_USER"} {
			command := regQuery(root + clientKey)
			if command == "" {
				continue
			}
			program, args, ok := parseWindowsCommand(command, rawURL)
			if !ok {
				continue
			}
			candidate := browserCommand{program: program, args: args}
			// 命令里的程序必须真实存在，避免选到已卸载的浏览器
			if _, err := os.Stat(candidate.program); err != nil {
				continue
			}
			return candidate, true
		}
	}
	return browserCommand{}, false
}

// browserPreference 给出浏览器尝试顺序的优先级，数字越小越优先
func browserPreference(name string) int {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "chrome"):
		return 0
	case strings.Contains(lower, "edge"):
		return 1
	case strings.Contains(lower, "firefox"):
		return 2
	case strings.Contains(lower, "iexplore"):
		return 90
	default:
		return 50
	}
}

// parseWindowsCommand 拆解注册表里的命令行，并把 %1 替换成网址
func parseWindowsCommand(command, rawURL string) (string, []string, bool) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", nil, false
	}

	var program string
	rest := command
	if strings.HasPrefix(rest, `"`) {
		end := strings.Index(rest[1:], `"`)
		if end < 0 {
			return "", nil, false
		}
		program = rest[1 : end+1]
		rest = rest[end+2:]
	} else {
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return "", nil, false
		}
		program = fields[0]
		rest = strings.TrimPrefix(rest, fields[0])
	}
	if program == "" {
		return "", nil, false
	}

	// 替换网址，并丢掉 %* / %2 之类的额外占位参数
	var args []string
	for _, field := range strings.Fields(rest) {
		if strings.HasPrefix(field, "%") && field != "%1" {
			continue
		}
		args = append(args, strings.ReplaceAll(field, "%1", rawURL))
	}
	if len(args) == 0 {
		args = []string{rawURL}
	}
	return program, args, true
}

// windowsDirectBrowserCommands 返回直接启动默认浏览器的调用方式
func windowsDirectBrowserCommands(rawURL string) []browserCommand {
	if candidate, ok := directBrowserCommand(rawURL); ok {
		return []browserCommand{candidate}
	}
	return nil
}
