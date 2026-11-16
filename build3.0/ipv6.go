package main

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"
)

// IPv6 出口探测目标：任一目标能建立 TCP 连接即认为当前网络可用 IPv6。
// 同时放境内与境外地址，避免单一目标被防火墙拦截造成误判。
var ipv6ProbeTargets = []string{
	"[2606:4700:4700::1111]:443", // Cloudflare DNS
	"[2400:3200::1]:53",          // 阿里公共 DNS
	"[2402:4e00::]:53",           // DNSPod 公共 DNS
	"[2001:4860:4860::8888]:53",  // Google Public DNS
}

const (
	ipv6ProbeTimeout = 2500 * time.Millisecond
	ipv6CacheTTL     = 30 * time.Second
)

// ipv6Status.State 取值：checking / ok / no / unknown
type ipv6Status struct {
	State  string `json:"state"`
	Text   string `json:"text"`
	Detail string `json:"detail"`
}

var (
	ipv6Mu       sync.Mutex
	ipv6Cached   ipv6Status
	ipv6CachedAt time.Time
	ipv6Checking bool
)

func ipv6CheckingStatus() ipv6Status {
	return ipv6Status{
		State:  "checking",
		Text:   "IPv6 检测中",
		Detail: "正在检测当前网络是否支持 IPv6",
	}
}

// requestIPv6Check 由页面连接、网络变化或用户点击胶囊触发。结果广播给所有页面并缓存 30 秒，
// 并发请求合并成一次探测，避免多开页面或频繁刷新时重复发起。
func requestIPv6Check(ws *pageConn, force bool) {
	ipv6Mu.Lock()
	if !force && ipv6Cached.State != "" && time.Since(ipv6CachedAt) < ipv6CacheTTL {
		cached := ipv6Cached
		ipv6Mu.Unlock()
		_ = sendWSMessage(ws, "ipv6_status", cached)
		return
	}
	if ipv6Checking {
		ipv6Mu.Unlock()
		return
	}
	ipv6Checking = true
	ipv6Mu.Unlock()

	_ = sendWSMessage(ws, "ipv6_status", ipv6CheckingStatus())

	go func() {
		result := detectIPv6()
		ipv6Mu.Lock()
		ipv6Cached = result
		ipv6CachedAt = time.Now()
		ipv6Checking = false
		ipv6Mu.Unlock()
		broadcastWSMessage("ipv6_status", result)
	}()
}

// detectIPv6 先确认系统有 IPv6 出口路由，再并发探测公网 IPv6 连通性。
// 本机地址与浏览器所在网络一致，比前端去请求第三方 IPv6 站点可靠。
func detectIPv6() ipv6Status {
	addrs := globalIPv6Addrs()

	// UDP connect 不实际发包，只让系统为公网 IPv6 目标选择源地址和路由；
	// 没有 IPv6 默认路由时会立即失败，比等 TCP 超时更快也更准。
	routeDialer := net.Dialer{Timeout: 1500 * time.Millisecond}
	routeConn, err := routeDialer.Dial("udp6", "[2606:4700:4700::1111]:443")
	if err != nil {
		return ipv6Status{
			State:  "no",
			Text:   "IPv6 不可用",
			Detail: "未检测到可用的 IPv6 出口路由，当前网络无法使用 IPv6",
		}
	}
	localAddr := routeConn.LocalAddr().String()
	_ = routeConn.Close()

	if target, ok := probeIPv6Targets(); ok {
		detail := "公网 IPv6 连通正常，已连通 " + target
		if localAddr != "" {
			detail = "本机 IPv6 出口 " + localAddr + "，已连通 " + target
		}
		return ipv6Status{State: "ok", Text: "IPv6 可用", Detail: detail}
	}

	detail := "检测到 IPv6 出口，但公网 IPv6 连通探测全部失败，可能是防火墙或运营商限制"
	if len(addrs) > 0 {
		detail = "本机 IPv6 地址 " + strings.Join(addrs, "、") + "；" + detail
	}
	return ipv6Status{State: "unknown", Text: "IPv6 未连通", Detail: detail}
}

// globalIPv6Addrs 收集可用于公网的本机 IPv6 地址，仅用于结果说明。
func globalIPv6Addrs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	addrs := []string{}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		ifAddrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range ifAddrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP
			if ip.To4() != nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || !ip.IsGlobalUnicast() {
				continue
			}
			addrs = append(addrs, ip.String())
		}
	}
	return addrs
}

// probeIPv6Targets 并发探测，任一成功即返回；全部失败或整体超时返回 false。
func probeIPv6Targets() (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), ipv6ProbeTimeout+300*time.Millisecond)
	defer cancel()

	type probeResult struct {
		target string
		err    error
	}
	results := make(chan probeResult, len(ipv6ProbeTargets))
	dialer := net.Dialer{Timeout: ipv6ProbeTimeout}
	for _, target := range ipv6ProbeTargets {
		go func(t string) {
			conn, err := dialer.DialContext(ctx, "tcp6", t)
			if err == nil {
				_ = conn.Close()
			}
			results <- probeResult{target: t, err: err}
		}(target)
	}

	for range ipv6ProbeTargets {
		select {
		case r := <-results:
			if r.err == nil {
				cancel() // 已确认可用，其余探测没必要继续
				return r.target, true
			}
		case <-ctx.Done():
			return "", false
		}
	}
	return "", false
}
