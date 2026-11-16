package cfdata

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestStartServesPageAndWebSocket(t *testing.T) {
	url, err := Start(t.TempDir())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer Stop()
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("Start() = %q, 期望 127.0.0.1 地址", url)
	}
	if !strings.HasSuffix(url, ":18080") {
		t.Fatalf("Start() = %q, 期望固定端口 18080", url)
	}

	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	resp, err := client.Get(url + "/")
	if err != nil {
		t.Fatalf("GET / error = %v", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Cloudflare IP Scan") {
		t.Fatalf("页面响应异常: status=%d len=%d", resp.StatusCode, len(body))
	}

	wsURL := "ws" + strings.TrimPrefix(url, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("WebSocket 连接失败: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, msg, err := conn.ReadMessage()
	conn.Close()
	if err != nil {
		t.Fatalf("读取 state_sync 失败: %v", err)
	}
	if !strings.Contains(string(msg), "state_sync") {
		t.Fatalf("首条消息 = %s, 期望 state_sync", msg)
	}

	Stop()
	if _, err := client.Get(url + "/"); err == nil {
		t.Fatal("Stop() 后服务仍可访问")
	}

	// 重启后端口必须保持一致，WebView 的 localStorage 才能按同一 origin 复用
	url2, err := Start(t.TempDir())
	if err != nil {
		t.Fatalf("第二次 Start() error = %v", err)
	}
	if url2 != url {
		t.Fatalf("重启后地址 = %q, 期望 %q", url2, url)
	}
	Stop()
}
