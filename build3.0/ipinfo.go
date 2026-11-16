package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 当前出口 IPv4 与归属信息：页面打开、网络变化或手动重测 IPv6 时顺带刷新一次，
// 结果写进运行日志，方便确认扫描/测速使用的是哪条宽带。
const (
	ipInfoJSONURL  = "https://myip.ipip.net/json"
	ipInfoTextURL  = "https://myip.ipip.net/"
	ipInfoTimeout  = 6 * time.Second
	ipInfoCacheTTL = 30 * time.Second
)

// 固定走 IPv4：即使本机双栈且默认优先 IPv6，也要拿到 IPv4 出口地址。
var ipInfoClient = &http.Client{
	Timeout: ipInfoTimeout,
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialer := &net.Dialer{Timeout: ipInfoTimeout}
			return dialer.DialContext(ctx, "tcp4", addr)
		},
		TLSHandshakeTimeout: ipInfoTimeout,
	},
}

var (
	ipInfoMu       sync.Mutex
	ipInfoCachedAt time.Time
	ipInfoChecking bool
	ipInfoLastLine string
)

var ipv4Pattern = regexp.MustCompile(`\b(\d{1,3}(?:\.\d{1,3}){3})\b`)

// requestIPInfo 与 IPv6 检测同源触发；缓存 30 秒并合并并发请求，
// 避免多开页面或刷新时重复请求第三方接口。
func requestIPInfo(force bool) {
	ipInfoMu.Lock()
	if !force && time.Since(ipInfoCachedAt) < ipInfoCacheTTL {
		ipInfoMu.Unlock()
		return
	}
	if ipInfoChecking {
		ipInfoMu.Unlock()
		return
	}
	ipInfoChecking = true
	ipInfoMu.Unlock()

	go func() {
		line, err := fetchIPInfoLine()
		ipInfoMu.Lock()
		ipInfoCachedAt = time.Now()
		ipInfoChecking = false
		changed := line != "" && line != ipInfoLastLine
		if changed {
			ipInfoLastLine = line
		}
		ipInfoMu.Unlock()

		if err != nil {
			logf("获取当前 IPv4 归属信息失败：%v", err)
			return
		}
		if changed {
			logf("%s", line)
		}
	}()
}

func fetchIPInfoLine() (string, error) {
	ip, location, err := fetchIPInfoJSON()
	if err != nil {
		ip, location, err = fetchIPInfoText()
	}
	if err != nil {
		return "", err
	}
	if ip == "" {
		return "", fmt.Errorf("未解析到 IPv4 地址")
	}
	if location == "" {
		return "当前 IP：" + ip, nil
	}
	return "当前 IP：" + ip + "  来自于：" + location, nil
}

func fetchIPInfoJSON() (string, string, error) {
	body, err := fetchIPInfoBody(ipInfoJSONURL)
	if err != nil {
		return "", "", err
	}
	var resp struct {
		Ret  string `json:"ret"`
		Data struct {
			IP       string   `json:"ip"`
			Location []string `json:"location"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", "", err
	}
	ip := normalizeIPv4(resp.Data.IP)
	if ip == "" {
		return "", "", fmt.Errorf("响应缺少 IPv4 地址")
	}
	parts := make([]string, 0, len(resp.Data.Location))
	for _, part := range resp.Data.Location {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	return ip, strings.Join(parts, " "), nil
}

// fetchIPInfoText 兼容接口返回纯文本「当前 IP：…  来自于：…」的情况。
func fetchIPInfoText() (string, string, error) {
	body, err := fetchIPInfoBody(ipInfoTextURL)
	if err != nil {
		return "", "", err
	}
	text := strings.TrimSpace(string(body))
	ip := normalizeIPv4(ipv4Pattern.FindString(text))
	if ip == "" {
		return "", "", fmt.Errorf("响应缺少 IPv4 地址")
	}

	location := ""
	for _, marker := range []string{"来自于：", "来自："} {
		if idx := strings.Index(text, marker); idx >= 0 {
			location = strings.Join(strings.Fields(text[idx+len(marker):]), " ")
			break
		}
	}
	return ip, location, nil
}

func fetchIPInfoBody(rawURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ipInfoTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	resp, err := ipInfoClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<10))
}

func normalizeIPv4(raw string) string {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil || ip.To4() == nil {
		return ""
	}
	return ip.To4().String()
}
