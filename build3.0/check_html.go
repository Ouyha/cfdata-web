//go:build ignore

// 一次性校验脚本：不依赖第三方库，检查页面结构完整性。
// 运行：go run check_html.go index.html
package main

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

var (
	reID       = regexp.MustCompile(`\bid="([^"]+)"`)
	reOnclick  = regexp.MustCompile(`\bonclick="([^"]+)"`)
	reFuncDef  = regexp.MustCompile(`function\s+([A-Za-z_$][\w$]*)\s*\(`)
	reTagOpen  = regexp.MustCompile(`<([a-zA-Z][\w-]*)`)
	reScript   = regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`)
	reStyle    = regexp.MustCompile(`(?s)<style[^>]*>(.*?)</style>`)
	reVoidTags = map[string]bool{
		"area": true, "base": true, "br": true, "col": true, "embed": true,
		"hr": true, "img": true, "input": true, "link": true, "meta": true,
		"param": true, "source": true, "track": true, "wbr": true,
	}
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: go run check_html.go index.html")
		os.Exit(2)
	}
	path := os.Args[1]
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("读取失败:", err)
		os.Exit(1)
	}
	page := string(raw)

	if !strings.HasPrefix(strings.TrimSpace(page), "<!DOCTYPE html>") {
		fmt.Println("警告: 缺少 <!DOCTYPE html>")
	}
	if !strings.Contains(page, `charset="UTF-8"`) {
		fmt.Println("警告: 缺少 UTF-8 声明")
	} else {
		fmt.Println("UTF-8 声明: 存在")
	}

	// ---- 标签配平（忽略 script/style 内部内容与注释）----
	clean := reScript.ReplaceAllString(page, "")
	clean = reStyle.ReplaceAllString(clean, "")
	clean = regexp.MustCompile(`(?s)<!--.*?-->`).ReplaceAllString(clean, "")

	stack := []string{}
	mismatch := []string{}
	for _, m := range regexp.MustCompile(`<(/?)([a-zA-Z][\w-]*)([^>]*?)(/?)>`).FindAllStringSubmatch(clean, -1) {
		closing, name, attrs, selfClose := m[1] == "/", strings.ToLower(m[2]), m[3], m[4] == "/"
		if reVoidTags[name] || selfClose || strings.HasSuffix(strings.TrimSpace(attrs), "/") {
			continue
		}
		if !closing {
			stack = append(stack, name)
			continue
		}
		if len(stack) == 0 {
			mismatch = append(mismatch, "多余的结束标签 </"+name+">")
			continue
		}
		top := stack[len(stack)-1]
		if top != name {
			mismatch = append(mismatch, fmt.Sprintf("</%s> 与最近的 <%s> 不匹配", name, top))
			// 尝试恢复
			for i := len(stack) - 1; i >= 0; i-- {
				if stack[i] == name {
					stack = stack[:i]
					break
				}
			}
			continue
		}
		stack = stack[:len(stack)-1]
	}
	if len(mismatch) == 0 && len(stack) == 0 {
		fmt.Println("标签配平: 通过")
	} else {
		for _, m := range mismatch {
			fmt.Println("标签问题:", m)
		}
		if len(stack) > 0 {
			fmt.Println("未闭合的标签:", strings.Join(stack, " > "))
		}
	}

	// ---- 重复 id ----
	counts := map[string]int{}
	for _, m := range reID.FindAllStringSubmatch(page, -1) {
		counts[m[1]]++
	}
	dups := []string{}
	for id, n := range counts {
		if n > 1 {
			dups = append(dups, fmt.Sprintf("%s(%d次)", id, n))
		}
	}
	sort.Strings(dups)
	if len(dups) == 0 {
		fmt.Println("重复 id 检查: 通过")
	} else {
		fmt.Println("重复 id:", strings.Join(dups, ", "))
	}

	// ---- 新增标记 ----
	for _, id := range []string{
		"themeCycleBtn", "btnShutdown", "btnAutoSpeed",
		"modeQuick", "modeFull", "qStart", "qTargetMbps", "qDCFilterBtn",
		"qHistoryTable", "dcFilterModal", "dcFilterApply",
	} {
		if counts[id] == 1 {
			fmt.Printf("id %-13s 存在\n", id)
		} else {
			fmt.Printf("id %-13s 异常（出现 %d 次）\n", id, counts[id])
		}
	}

	// ---- 脚本与样式 ----
	sm := reScript.FindAllStringSubmatch(page, -1)
	styles := reStyle.FindAllStringSubmatch(page, -1)
	var js string
	for _, m := range sm {
		js += m[1]
	}
	var css string
	for _, m := range styles {
		css += m[1]
	}
	fmt.Printf("script 块=%d style 块=%d\n", len(sm), len(styles))

	jsPair := strings.Count(js, "{") == strings.Count(js, "}")
	cssPair := strings.Count(css, "{") == strings.Count(css, "}")
	fmt.Printf("脚本大括号 { =%d } =%d %s\n", strings.Count(js, "{"), strings.Count(js, "}"), pairText(jsPair))
	fmt.Printf("样式大括号 { =%d } =%d %s\n", strings.Count(css, "{"), strings.Count(css, "}"), pairText(cssPair))

	defined := map[string]bool{}
	for _, m := range reFuncDef.FindAllStringSubmatch(js, -1) {
		defined[m[1]] = true
	}
	missing := []string{}
	for _, m := range reOnclick.FindAllStringSubmatch(page, -1) {
		name := strings.TrimSpace(strings.SplitN(m[1], "(", 2)[0])
		if name == "" || strings.ContainsAny(name, ". ") {
			continue
		}
		if !defined[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		fmt.Println("onclick 引用的函数: 全部有定义")
	} else {
		fmt.Println("onclick 引用但未定义:", strings.Join(missing, ", "))
	}

	want := []string{
		"initTheme", "applyTheme", "setThemeMode", "resolveThemeMode", "systemPrefersDark",
		"shutdownApp", "connect", "handleMessage", "sendWS",
		"applyStateSync", "applyScanState", "applyScanSummary", "applyScanFinished",
		"buildScanRow", "buildTestRow", "updateSpeedResult", "refreshControls",
	}
	absent := []string{}
	for _, fn := range want {
		if !defined[fn] {
			absent = append(absent, fn)
		}
	}
	if len(absent) == 0 {
		fmt.Println("关键函数: 全部存在")
	} else {
		fmt.Println("缺少关键函数:", strings.Join(absent, ", "))
	}

	fmt.Printf("脚本里 heartbeat 出现 %d 次, innerHTML 赋值 %d 处, 内联 onclick %d 处\n",
		strings.Count(js, "heartbeat"), strings.Count(js, ".innerHTML ="), len(reOnclick.FindAllString(page, -1)))
}

func pairText(ok bool) string {
	if ok {
		return "配平"
	}
	return "不配平"
}
