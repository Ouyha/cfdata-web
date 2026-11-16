//go:build ignore

// 一次性测试客户端：模拟网页的 WebSocket 行为。
//
// 用法：
//
//	go run wsclient.go <url> heartbeat <秒数>   发送心跳并保持连接
//	go run wsclient.go <url> nobeat <秒数>      连接但不发心跳
//	go run wsclient.go <url> shutdown           发送关闭指令
//	go run wsclient.go <url> resume             发送恢复扫描指令
//	go run wsclient.go <url> trace <秒数>       只收消息，按类型打印
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("参数不足")
		os.Exit(1)
	}
	url, action := os.Args[1], os.Args[2]
	seconds := 5
	if len(os.Args) > 3 {
		if n, err := strconv.Atoi(os.Args[3]); err == nil {
			seconds = n
		}
	}

	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		fmt.Println("连接失败:", err)
		os.Exit(1)
	}
	fmt.Println("已连接", url)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			// 服务端可能一直不关连接，设个读取超时让客户端自己收尾
			_ = conn.SetReadDeadline(time.Now().Add(time.Duration(seconds+5) * time.Second))
			_, msg, err := conn.ReadMessage()
			if err != nil {
				fmt.Println("读取结束:", err)
				return
			}
			text := string(msg)
			if len(text) > 260 {
				text = text[:260] + "..."
			}
			fmt.Println("收到:", text)
		}
	}()

	switch action {
	case "heartbeat":
		fmt.Printf("开始发送心跳，保持 %d 秒（每 2 秒一次）\n", seconds)
		deadline := time.Now().Add(time.Duration(seconds) * time.Second)
		for time.Now().Before(deadline) {
			if err := conn.WriteJSON(map[string]string{"type": "heartbeat"}); err != nil {
				fmt.Println("发送心跳失败:", err)
				return
			}
			time.Sleep(2 * time.Second)
		}
		fmt.Println("模拟关闭网页：断开连接")
		conn.Close()

	case "nobeat":
		fmt.Printf("连接但不发心跳，等待 %d 秒\n", seconds)
		time.Sleep(time.Duration(seconds) * time.Second)
		conn.Close()

	case "shutdown":
		fmt.Println("发送关闭指令")
		if err := conn.WriteJSON(map[string]string{"type": "shutdown"}); err != nil {
			fmt.Println("发送失败:", err)
			os.Exit(1)
		}

	case "resume":
		fmt.Println("发送 resume_task 指令")
		if err := conn.WriteJSON(map[string]string{"type": "resume_task"}); err != nil {
			fmt.Println("发送失败:", err)
			os.Exit(1)
		}

	case "trace":
		time.Sleep(time.Duration(seconds) * time.Second)
		conn.Close()
	}

	<-done
	fmt.Println("客户端退出")
}
