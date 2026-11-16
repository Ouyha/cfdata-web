package cfdata

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// 移动端不做“页面全部关闭就退出”的判断，只保留 ping/pong 和连接跟踪。

const pingInterval = 5 * time.Second

// pageConn 为每个页面连接保留独立写锁，慢客户端不会拖住其他页面的广播
type pageConn struct {
	*websocket.Conn
	writeMu sync.Mutex
}

var (
	pageSessions   = make(map[*pageConn]struct{})
	pageSessionsMu sync.Mutex
	shutdownHook   func()
)

func trackPage(ws *pageConn) {
	pageSessionsMu.Lock()
	pageSessions[ws] = struct{}{}
	pageSessionsMu.Unlock()
}

func untrackPage(ws *pageConn) {
	pageSessionsMu.Lock()
	delete(pageSessions, ws)
	pageSessionsMu.Unlock()
}

// snapshotPages 返回当前连接快照，供广播使用
func snapshotPages() []*pageConn {
	pageSessionsMu.Lock()
	defer pageSessionsMu.Unlock()
	conns := make([]*pageConn, 0, len(pageSessions))
	for conn := range pageSessions {
		conns = append(conns, conn)
	}
	return conns
}

// refreshReadDeadline 在移动端不设置读超时，由 Activity 生命周期决定服务何时停止
func refreshReadDeadline(ws *pageConn) {
	_ = ws.SetReadDeadline(time.Time{})
}

// startPagePing 为单个页面启动 ping 循环
func startPagePing(ws *pageConn) {
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for range ticker.C {
			deadline := time.Now().Add(writeWait)
			if err := ws.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
				ws.Close()
				return
			}
		}
	}()
}

// requestShutdown 用户点击网页上的「关闭程序」按钮：停止本地服务，保留 Activity
func requestShutdown(ws *pageConn) {
	_ = sendWSMessage(ws, "log", "正在关闭程序...")
	broadcastWSMessage("shutting_down", map[string]string{"reason": "收到网页发送的关闭指令"})
	if shutdownHook != nil {
		shutdownHook()
	}
}
