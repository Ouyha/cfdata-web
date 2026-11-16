package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestParseSubnetsIPv4(t *testing.T) {
	content := "1.2.3.4/24\n1.2.3.0/24\n# comment\n\n5.6.7.99/24\n"
	got, err := parseSubnets(content, 4)
	if err != nil {
		t.Fatalf("parseSubnets() error = %v", err)
	}
	want := []string{"1.2.3.0/24", "5.6.7.0/24"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("parseSubnets() = %v, 期望 %v", got, want)
	}
}

func TestParseSubnetsIPv6(t *testing.T) {
	content := "2001:db8:abcd:1234::/48\n2001:db8:abcd::/48\n"
	got, err := parseSubnets(content, 6)
	if err != nil {
		t.Fatalf("parseSubnets() error = %v", err)
	}
	if len(got) != 1 || got[0] != "2001:db8:abcd::/48" {
		t.Fatalf("parseSubnets() = %v, 期望 [2001:db8:abcd::/48]", got)
	}
}

func TestParseSubnetsRejectsWrongPrefix(t *testing.T) {
	cases := []struct {
		name    string
		content string
		ipType  int
		wantErr string
	}{
		{"IPv4 非 /24", "1.2.3.0/23\n", 4, "不是 IPv4 /24"},
		{"IPv4 缺少前缀", "1.2.3.4\n", 4, "不是合法 CIDR"},
		{"IPv4 列表混入 IPv6", "2001:db8::/48\n", 4, "不是 IPv4 /24"},
		{"IPv6 列表混入 IPv4", "1.2.3.0/24\n", 6, "不是 IPv6 /48"},
		{"IPv6 前缀不是 /48", "2001:db8::/32\n", 6, "不是 IPv6 /48"},
		{"空列表", "# only comment\n\n", 4, "IP 列表为空"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSubnets(tc.content, tc.ipType)
			if err == nil {
				t.Fatal("期望报错，实际通过")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息 = %q, 期望包含 %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseSubnetsReportsLineNumber(t *testing.T) {
	_, err := parseSubnets("1.2.3.0/24\nbad-line\n", 4)
	if err == nil || !strings.Contains(err.Error(), "第 2 行") {
		t.Fatalf("错误信息 = %v, 期望包含行号", err)
	}
}

func TestRandomIPv4WithinSubnet(t *testing.T) {
	subnets := []string{"104.16.0.0/24", "1.2.3.0/24"}
	ips := getRandomIPv4s(subnets)
	if len(ips) != len(subnets) {
		t.Fatalf("生成数量 = %d, 期望 %d", len(ips), len(subnets))
	}
	for i, raw := range ips {
		_, ipnet, err := net.ParseCIDR(subnets[i])
		if err != nil {
			t.Fatal(err)
		}
		ip := net.ParseIP(raw)
		if ip == nil || !ipnet.Contains(ip) {
			t.Fatalf("生成的 IP %s 不在 %s 内", raw, subnets[i])
		}
	}
}

func TestRandomIPv6WithinSubnet(t *testing.T) {
	const subnet = "2001:db8:abcd::/48"
	ips := getRandomIPv6s([]string{subnet})
	if len(ips) != 1 {
		t.Fatalf("生成数量 = %d, 期望 1", len(ips))
	}
	ip := net.ParseIP(ips[0])
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		t.Fatal(err)
	}
	if ip == nil || ip.To4() != nil || !ipnet.Contains(ip) {
		t.Fatalf("生成的 IPv6 %s 不在 %s 内", ips[0], subnet)
	}
	// /48 的前 6 字节必须保持不变
	if !bytes.Equal(ip.To16()[:6], ipnet.IP.To16()[:6]) {
		t.Fatalf("生成的 IPv6 %s 改动了 /48 前缀", ips[0])
	}
}

func TestSchemeForPort(t *testing.T) {
	cases := map[int]string{
		80:   "http",
		8080: "http",
		2052: "http",
		443:  "https",
		2053: "https",
		2083: "https",
		2087: "https",
		2096: "https",
		8443: "https",
	}
	for port, want := range cases {
		if got := schemeForPort(port); got != want {
			t.Errorf("schemeForPort(%d) = %q, 期望 %q", port, got, want)
		}
	}
}

func TestSpeedURLHost(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"无 scheme 的默认 URL", "speed.cloudflare.com/__down?bytes=10000000", "speed.cloudflare.com", false},
		{"https URL", "https://speed.cloudflare.com/__down?bytes=10000000", "speed.cloudflare.com", false},
		{"带端口", "http://example.com:8443/path", "example.com", false},
		{"IPv6 字面量", "https://[2001:db8::1]/", "2001:db8::1", false},
		{"空 URL", "", "", true},
		{"没有主机", "http://", "", true},
		{"含空格", "not a url", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := speedURLHost(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("speedURLHost(%q) 期望报错，实际返回 %q", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("speedURLHost(%q) error = %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("speedURLHost(%q) = %q, 期望 %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestWsOriginAllowed(t *testing.T) {
	cases := []struct {
		name     string
		origin   string
		host     string
		loopback bool
		want     bool
	}{
		{"回环：无 Origin", "", "127.0.0.1:8080", true, true},
		{"回环：localhost", "http://localhost:8080", "localhost:8080", true, true},
		{"回环：IPv6 回环", "http://[::1]:8080", "[::1]:8080", true, true},
		{"回环：外部 Origin", "http://evil.example", "127.0.0.1:8080", true, false},
		{"回环：域名 Host", "", "evil.example:8080", true, false},
		{"局域网：IP Host + 同源", "http://192.168.1.5:8080", "192.168.1.5:8080", false, true},
		{"局域网：域名 Host 被拒", "http://evil.example:8080", "evil.example:8080", false, false},
		{"局域网：Origin 与 Host 不一致", "http://192.168.1.6:8080", "192.168.1.5:8080", false, false},
		{"局域网：localhost 可用", "http://localhost:8080", "localhost:8080", false, true},
		{"空 Host", "http://localhost", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wsOriginAllowed(tc.origin, tc.host, tc.loopback); got != tc.want {
				t.Fatalf("wsOriginAllowed(%q, %q, %v) = %v, 期望 %v",
					tc.origin, tc.host, tc.loopback, got, tc.want)
			}
		})
	}
}

func TestHostnameOnly(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8080": "127.0.0.1",
		"localhost":      "localhost",
		"[::1]:8080":     "::1",
		"[::1]":          "::1",
	}
	for input, want := range cases {
		if got := hostnameOnly(input); got != want {
			t.Errorf("hostnameOnly(%q) = %q, 期望 %q", input, got, want)
		}
	}
}

func TestClampInt(t *testing.T) {
	if got := clampInt(0, 1, 500); got != 1 {
		t.Errorf("clampInt(0) = %d, 期望 1", got)
	}
	if got := clampInt(9999, 1, 500); got != 500 {
		t.Errorf("clampInt(9999) = %d, 期望 500", got)
	}
	if got := clampInt(42, 1, 500); got != 42 {
		t.Errorf("clampInt(42) = %d, 期望 42", got)
	}
}

func TestSaveToFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "ips-v4.txt")

	if err := saveToFileAtomic(path, []byte("1.1.1.0/24\n")); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "1.1.1.0/24\n" {
		t.Fatalf("读取内容 = %q, err = %v", got, err)
	}

	if err := saveToFileAtomic(path, []byte("2.2.2.0/24\n")); err != nil {
		t.Fatalf("覆盖写入失败: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "2.2.2.0/24\n" {
		t.Fatalf("覆盖后内容 = %q", got)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".cfdata-") {
			t.Fatalf("残留临时文件: %s", entry.Name())
		}
	}
}

func TestFetchURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte("1.1.1.0/24\n"))
		case "/empty":
			w.WriteHeader(http.StatusOK)
		case "/fail":
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	body, err := fetchURL(context.Background(), server.URL+"/ok")
	if err != nil || string(body) != "1.1.1.0/24\n" {
		t.Fatalf("fetchURL(ok) = %q, %v", body, err)
	}
	if _, err := fetchURL(context.Background(), server.URL+"/empty"); err == nil {
		t.Fatal("空响应应报错")
	}
	if _, err := fetchURL(context.Background(), server.URL+"/fail"); err == nil {
		t.Fatal("非 200 响应应报错")
	}
}

func TestBuildDCList(t *testing.T) {
	results := []ScanResult{
		{IP: "1.1.1.1", DataCenter: "SJC", Region: "US", City: "San Jose", TCPDuration: 120 * time.Millisecond},
		{IP: "1.1.1.2", DataCenter: "LAX", Region: "US", City: "Los Angeles", TCPDuration: 80 * time.Millisecond},
		{IP: "1.1.1.3", DataCenter: "SJC", Region: "US", City: "San Jose", TCPDuration: 60 * time.Millisecond},
	}

	list := buildDCList(results)
	if len(list) != 2 {
		t.Fatalf("数据中心数量 = %d, 期望 2", len(list))
	}
	if list[0].DataCenter != "SJC" || list[0].MinLatency != 60 || list[0].IPCount != 2 {
		t.Fatalf("首个数据中心 = %+v, 期望 SJC/60ms/2 个 IP", list[0])
	}
	if list[1].DataCenter != "LAX" || list[1].MinLatency != 80 {
		t.Fatalf("第二个数据中心 = %+v, 期望 LAX/80ms", list[1])
	}
}

type speedTestReaderFunc func([]byte) (int, error)

func (f speedTestReaderFunc) Read(p []byte) (int, error) { return f(p) }

func TestPeakDownloadSpeedReadErrors(t *testing.T) {
	networkErr := errors.New("network read failed")
	for _, tc := range []struct {
		name      string
		body      io.Reader
		wantBytes int64
		wantErr   error
	}{
		{"success", strings.NewReader("sample"), 6, nil},
		{"empty", strings.NewReader(""), 0, nil},
		{"data-and-EOF", speedTestReaderFunc(func(p []byte) (int, error) { return copy(p, "sample"), io.EOF }), 6, nil},
		{"truncated", speedTestReaderFunc(func(p []byte) (int, error) { return copy(p, "sample"), io.ErrUnexpectedEOF }), 6, io.ErrUnexpectedEOF},
		{"network", speedTestReaderFunc(func(p []byte) (int, error) { return copy(p, "sample"), networkErr }), 6, networkErr},
		{"external-cancel", speedTestReaderFunc(func(p []byte) (int, error) { return copy(p, "sample"), context.Canceled }), 6, context.Canceled},
		{"sample-complete", speedTestReaderFunc(func(p []byte) (int, error) { return copy(p, "sample"), fmt.Errorf("read: %w", errSpeedSampleComplete) }), 6, errSpeedSampleComplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Windows 时钟可能无法分辨纯内存读取耗时，显式模拟一次 I/O 等待。
			reader := speedTestReaderFunc(func(p []byte) (int, error) {
				<-time.After(10 * time.Millisecond)
				return tc.body.Read(p)
			})
			speed, total, err := peakDownloadSpeed(reader)
			if total != tc.wantBytes || !errors.Is(err, tc.wantErr) {
				t.Fatalf("peakDownloadSpeed() = %g, %d, %v; want bytes=%d, err=%v", speed, total, err, tc.wantBytes, tc.wantErr)
			}
			if (total == 0 && speed != 0) || (total > 0 && speed <= 0) {
				t.Fatalf("speed = %g for %d bytes", speed, total)
			}
		})
	}
}

func speedServerAddress(server *httptest.Server) (string, int) {
	addr := server.Listener.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

func speedGzipPayload(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	zw := gzip.NewWriter(&encoded)
	if _, err := zw.Write(bytes.Repeat([]byte("a"), 1<<20)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestSpeedHTTPCountsEncodedBytes(t *testing.T) {
	encoded := speedGzipPayload(t)
	for _, acceptEncoding := range []string{"", "identity"} {
		t.Run("accept="+acceptEncoding, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Accept-Encoding"); got != acceptEncoding {
					t.Errorf("Accept-Encoding = %q, want %q", got, acceptEncoding)
				}
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
				_, _ = w.Write(encoded)
			}))
			defer server.Close()
			ip, port := speedServerAddress(server)
			client := newIPBoundHTTPClient(ip, port)
			if !client.Transport.(*http.Transport).DisableCompression {
				t.Fatal("测速 Transport 必须禁用自动解压")
			}
			req, err := http.NewRequest(http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			if acceptEncoding != "" {
				req.Header.Set("Accept-Encoding", acceptEncoding)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.Uncompressed || resp.Header.Get("Content-Encoding") != "gzip" {
				t.Fatal("gzip 响应被自动解压")
			}
			_, total, err := peakDownloadSpeed(resp.Body)
			if err != nil || total != int64(len(encoded)) {
				t.Fatalf("统计字节 = %d, err = %v; want encoded bytes %d", total, err, len(encoded))
			}
		})
	}
}

// 两条测速入口共享这些 loopback HTTP 响应，不进行公网请求。
func newSpeedResponseServer(t *testing.T) *httptest.Server {
	t.Helper()
	encoded := speedGzipPayload(t)
	payload := bytes.Repeat([]byte("x"), 64*1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept-Encoding"); got != "identity" {
			t.Errorf("测速请求 Accept-Encoding = %q, want identity", got)
		}
		w.Header().Set("CF-RAY", "local-HKG")
		switch r.URL.Path {
		case "/ok", "/gzip":
			body := payload
			if r.URL.Path == "/gzip" {
				w.Header().Set("Content-Encoding", "gzip")
				body = encoded
			}
			_, _ = w.Write(body[:1])
			w.(http.Flusher).Flush()
			// 分段发送，避免纯 loopback 缓冲读取耗时低于 Windows 时钟分辨率。
			<-time.After(20 * time.Millisecond)
			_, _ = w.Write(body[1:])
		case "/empty":
			w.WriteHeader(http.StatusOK)
		case "/truncated":
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)*2))
			_, _ = w.Write(payload)
		case "/broken-chunked":
			_, _ = w.Write(payload)
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func singleSpeedResult(t *testing.T, ip string, port int, speedURL string) (string, string) {
	t.Helper()
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade: %v", err)
			return
		}
		defer conn.Close()
		page := &pageConn{Conn: conn}
		trackPage(page)
		defer func() {
			pageSessionsMu.Lock()
			delete(pageSessions, page)
			pageSessionsMu.Unlock()
		}()
		runSpeedTest(page, ip, port, speedURL)
	}))
	defer server.Close()
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var speed string
	var logs []string
	results := 0
	for {
		var msg struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := conn.ReadJSON(&msg); err != nil {
			break
		}
		switch msg.Type {
		case "speed_test_result":
			var result map[string]string
			if err := json.Unmarshal(msg.Data, &result); err != nil {
				t.Fatal(err)
			}
			if result["ip"] != ip {
				t.Fatalf("result IP = %q, want %q", result["ip"], ip)
			}
			speed = result["speed"]
			results++
		case "log":
			var text string
			if err := json.Unmarshal(msg.Data, &text); err != nil {
				t.Fatal(err)
			}
			logs = append(logs, text)
		}
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("单 IP 测速未结束")
	}
	if results != 1 {
		t.Fatalf("speed_test_result count = %d, want 1", results)
	}
	return speed, strings.Join(logs, "\n")
}

func TestRunSpeedTestResponses(t *testing.T) {
	server := newSpeedResponseServer(t)
	ip, port := speedServerAddress(server)
	for _, tc := range []struct{ path, want string }{
		{"/ok", ""}, {"/gzip", ""}, {"/empty", "无数据"},
		{"/truncated", "下载错误"}, {"/broken-chunked", "下载错误"},
		{"/status", "HTTP 503 错误"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			speed, logs := singleSpeedResult(t, ip, port, server.URL+tc.path)
			if tc.want == "" {
				value, err := strconv.ParseFloat(strings.TrimSuffix(speed, " MB/s"), 64)
				if err != nil || value <= 0 || !strings.HasSuffix(speed, " MB/s") {
					t.Fatalf("成功速度 = %q", speed)
				}
			} else if speed != tc.want || strings.Contains(logs, "测速完成") {
				t.Fatalf("speed = %q, want %q; logs = %s", speed, tc.want, logs)
			}
			if tc.want == "下载错误" && (!strings.Contains(logs, "下载错误") || !strings.Contains(logs, "unexpected EOF")) {
				t.Fatalf("缺少下载错误日志: %s", logs)
			}
		})
	}
}

// 故意延迟响应头，确认五秒窗口从成功响应头之后开始；服务器只等待客户端取消。
func newSpeedSamplingServer(t *testing.T) (*httptest.Server, <-chan time.Time) {
	t.Helper()
	headersAt := make(chan time.Time, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(200 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("采样请求必须指定 identity")
		}
		w.Header().Set("CF-RAY", "local-HKG")
		w.Header().Set("Content-Length", "1000000000")
		headersAt <- time.Now()
		_, _ = w.Write(bytes.Repeat([]byte("x"), 64*1024))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	return server, headersAt
}

func assertSpeedSampleDuration(t *testing.T, headersAt <-chan time.Time) {
	t.Helper()
	select {
	case started := <-headersAt:
		if elapsed := time.Since(started); elapsed < 4900*time.Millisecond || elapsed > 8*time.Second {
			t.Fatalf("收到响应头后的采样时长 = %v, want about 5s", elapsed)
		}
	default:
		t.Fatal("未收到成功响应头")
	}
}

func TestRunSpeedTestSampleComplete(t *testing.T) {
	server, headersAt := newSpeedSamplingServer(t)
	ip, port := speedServerAddress(server)
	speed, logs := singleSpeedResult(t, ip, port, server.URL)
	assertSpeedSampleDuration(t, headersAt)
	if !strings.HasSuffix(speed, " MB/s") || strings.Contains(logs, "测速失败") {
		t.Fatalf("五秒正常采样被误判失败: speed=%q, logs=%s", speed, logs)
	}
}

// 验证真实 Transport 的 Body.Read 保留取消 cause，而不是仅返回 context.Canceled。
func TestSpeedHTTPBodyPreservesCancelCause(t *testing.T) {
	for _, framing := range []string{"content-length", "chunked"} {
		t.Run(framing, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if framing == "content-length" {
					w.Header().Set("Content-Length", "1000000")
				}
				_, _ = w.Write([]byte("sample"))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()

			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			ip, port := speedServerAddress(server)
			client := newIPBoundHTTPClient(ip, port)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			cause := errors.New("sample complete test cause")
			timer := time.AfterFunc(50*time.Millisecond, func() { cancel(cause) })
			defer timer.Stop()
			body, readErr := io.ReadAll(resp.Body)
			if string(body) != "sample" || !errors.Is(readErr, cause) {
				t.Fatalf("ReadAll() = %q, %v; want sample and cancellation cause", body, readErr)
			}
		})
	}
}
