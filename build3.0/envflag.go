package main

import "strings"

// isTruthyEnv 判断环境变量是否表示“开启”。
// 空字符串、0、false、no、off 都算关闭，其余非空值算开启。
func isTruthyEnv(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	switch value {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}
