package main

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ----------------------- 页面连接存活与自动退出 -----------------------
//
// 存活检测改为服务端 ping / 浏览器 pong：
//   - 以前靠页面里的 setInterval 每 5 秒发心跳，标签页切到后台后
//     浏览器会把定时器限流到每分钟一次，宽限期一到程序就误退；
//   - WebSocket 的 ping/pong 由浏览器协议栈直接回应，不受后台限流影响。
//
// 关掉网页就关掉程序仍然沿用「最后一次活动 + 宽限期」的思路，
// 这样刷新页面、网络抖动导致的短暂断开不会误退出。
//
// 关闭自动退出：启动参数 -auto-exit=false，或环境变量 CFD_NO_AUTO_EXIT=1

const (
	// pingInterval 服务端向页面发 ping 的间隔
	pingInterval = 5 * time.Second
	// defaultIdleTimeout 最后一次页面活动之后，多久没动静就退出程序
	defaultIdleTimeout = 10 * time.Second
	// watchdogTick 看门狗检查间隔
	watchdogTick = 2 * time.Second
	// readGrace 读超时 = 宽限期 + 两个 ping 周期，给慢网络留响应时间
	readGrace = 2 * pingInterval
)

// pageConn 为每个页面连接保留独立写锁，慢客户端不会拖住其他页面的广播
type pageConn struct {
	*websocket.Conn
	writeMu sync.Mutex
}

var (
	// 页面活动记录：键是连接，值是最后一次活动时间（pong、消息或断开）
	pageSessions   = make(map[*pageConn]time.Time)
	pageSessionsMu sync.Mutex

	// 曾经有过页面连接（区分“程序刚起来还没人访问”和“用户已经关掉页面”）
	pageEverSeen bool
	// 正在关闭，避免重复触发
	shuttingDown bool
)

// trackPage 登记/刷新一个页面的活动时间
func trackPage(ws *pageConn) {
	pageSessionsMu.Lock()
	pageSessions[ws] = time.Now()
	pageEverSeen = true
	pageSessionsMu.Unlock()
}

// untrackPage 页面连接断开时，把断开时刻记为最后一次活动
//
// 关键：这里不能直接删除记录。如果一断开就删，看门狗下一拍（最多 2 秒后）
// 就会认为“没有活跃页面”而立即退出，宽限期等于没生效，
// 刷新页面的重连窗口也扛不住。保留断开时间，让宽限期真正起作用。
func untrackPage(ws *pageConn) {
	pageSessionsMu.Lock()
	if _, tracked := pageSessions[ws]; tracked {
		pageSessions[ws] = time.Now()
	}
	pageSessionsMu.Unlock()
}

// snapshotPages 返回当前仍在跟踪的连接快照，供广播使用
func snapshotPages() []*pageConn {
	pageSessionsMu.Lock()
	defer pageSessionsMu.Unlock()
	conns := make([]*pageConn, 0, len(pageSessions))
	for conn := range pageSessions {
		conns = append(conns, conn)
	}
	return conns
}

// refreshReadDeadline 按自动退出配置刷新读超时
//
// 超时后 ReadMessage 会返回错误，连接被关闭；随后看门狗在宽限期内
// 看不到任何活动，才会真正退出程序。
func refreshReadDeadline(ws *pageConn) {
	if !shouldAutoExit() {
		_ = ws.SetReadDeadline(time.Time{})
		return
	}
	_ = ws.SetReadDeadline(time.Now().Add(idleTimeout + readGrace))
}

// startPagePing 为单个页面启动 ping 循环
//
// 发送失败说明连接已经不可用，主动关闭让读循环退出并清理记录。
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

// shouldAutoExit 综合命令行参数与环境变量判断是否启用自动退出
func shouldAutoExit() bool {
	if isTruthyEnv(os.Getenv("CFD_NO_AUTO_EXIT")) {
		return false
	}
	return autoExit
}

// startExitWatchdog 启动看门狗
//
// 即使关闭了自动退出也会启动：只做过期记录清理，避免 map 无限增长。
func startExitWatchdog() {
	autoExitEnabled := shouldAutoExit()
	if autoExitEnabled {
		fmt.Printf("页面全部关闭后程序会自动退出（心跳超时 %s）\n", idleTimeout)
	} else {
		fmt.Println("已关闭「关掉网页自动退出程序」功能，程序会一直运行")
	}

	go func() {
		ticker := time.NewTicker(watchdogTick)
		defer ticker.Stop()

		for range ticker.C {
			pageSessionsMu.Lock()
			now := time.Now()
			seen := pageEverSeen
			recent := false
			for conn, lastActive := range pageSessions {
				if now.Sub(lastActive) <= idleTimeout {
					recent = true
					continue
				}
				delete(pageSessions, conn)
			}
			pageSessionsMu.Unlock()

			// 只有“确实来过页面，且宽限期内一直没动静”才退出
			if autoExitEnabled && seen && !recent {
				exitProgram("检测到网页已关闭")
				return
			}
		}
	}()
}

// requestShutdown 用户点击网页上的「关闭程序」按钮
func requestShutdown(ws *pageConn) {
	_ = sendWSMessage(ws, "log", "正在关闭程序...")
	exitProgram("收到网页发送的关闭指令")
}

// exitProgram 通知所有页面后退出进程
func exitProgram(reason string) {
	pageSessionsMu.Lock()
	if shuttingDown {
		pageSessionsMu.Unlock()
		return
	}
	shuttingDown = true
	conns := make([]*pageConn, 0, len(pageSessions))
	for conn := range pageSessions {
		conns = append(conns, conn)
	}
	pageSessionsMu.Unlock()

	fmt.Printf("%s，程序即将退出\n", reason)

	// 先启动退出计时，再逐个通知：慢连接最多阻塞 writeWait，
	// 不能让它拖住整个退出流程。
	go func() {
		time.Sleep(600 * time.Millisecond)
		os.Exit(0)
	}()

	for _, conn := range conns {
		_ = sendWSMessage(conn, "shutting_down", map[string]string{"reason": reason})
	}
}
