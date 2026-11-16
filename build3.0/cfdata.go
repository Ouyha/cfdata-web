package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ----------------------- 嵌入静态文件 -----------------------

//go:embed index.html
var staticFiles embed.FS

// ----------------------- 常量 -----------------------

const (
	minScanThreads = 1
	maxScanThreads = 500
	defaultThreads = 100

	// WebSocket 单次写入超时，防止慢客户端把全局写锁和任务 goroutine 卡死
	writeWait = 5 * time.Second

	// 联网下载限制，避免坏响应或超大响应把磁盘写满
	fetchTimeout    = 30 * time.Second
	maxDownloadSize = 20 << 20

	// 资源文件下载地址
	locationsURL = "https://www.baipiao.eu.org/cloudflare/locations"
	ipsV4URL     = "https://www.baipiao.eu.org/cloudflare/ips-v4"
	ipsV6URL     = "https://www.baipiao.eu.org/cloudflare/ips-v6"

	// 扫描时每个 IP 只做一次 TCP 握手；HTTPS 端口额外完成 TLS + HTTP 头读取
	scanConnectTimeout = 1 * time.Second
	scanRequestTimeout = 3 * time.Second
)

// ----------------------- 数据类型定义 -----------------------

// DataCenterInfo 数据中心信息
type DataCenterInfo struct {
	DataCenter string
	Region     string
	City       string
	IPCount    int
	MinLatency int // 毫秒
}

// ScanResult 扫描结果
type ScanResult struct {
	IP          string
	DataCenter  string
	Region      string
	City        string
	LatencyStr  string
	TCPDuration time.Duration `json:"-"`
}

// location 位置信息
type location struct {
	Iata   string  `json:"iata"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Cca2   string  `json:"cca2"`
	Region string  `json:"region"`
	City   string  `json:"city"`
}

// stateSync 新页面连接或任务切换时下发的完整状态快照
type stateSync struct {
	TaskRunning bool             `json:"taskRunning"`
	TaskKind    string           `json:"taskKind"`
	ScanPaused  bool             `json:"scanPaused"`
	ScanPhase   string           `json:"scanPhase"`
	ScanCurrent int              `json:"scanCurrent"`
	ScanTotal   int              `json:"scanTotal"`
	ScanResults []ScanResult     `json:"scanResults"`
	DCList      []DataCenterInfo `json:"dcList"`
	Fast        fastSnapshot     `json:"fast"`
}

type scanController struct {
	mu       sync.Mutex
	cond     *sync.Cond
	paused   bool
	canceled bool
	cancel   context.CancelFunc
}

func newScanController() *scanController {
	c := &scanController{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *scanController) Pause() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused || c.canceled {
		return false
	}
	c.paused = true
	return true
}

func (c *scanController) Resume() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.paused || c.canceled {
		return false
	}
	c.paused = false
	c.cond.Broadcast()
	return true
}

// SetCancel 登记当前扫描任务的取消函数；如果登记前已经请求取消，立即执行。
func (c *scanController) SetCancel(cancel context.CancelFunc) {
	c.mu.Lock()
	c.cancel = cancel
	canceled := c.canceled
	c.mu.Unlock()
	if canceled {
		cancel()
	}
}

// Cancel 取消扫描：停止派发新 IP，并中断在途探测。
func (c *scanController) Cancel() bool {
	c.mu.Lock()
	if c.canceled {
		c.mu.Unlock()
		return false
	}
	c.canceled = true
	c.paused = false
	cancel := c.cancel
	c.cond.Broadcast()
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

func (c *scanController) Reset() {
	c.mu.Lock()
	c.paused = false
	c.canceled = false
	c.cancel = nil
	c.cond.Broadcast()
	c.mu.Unlock()
}

func (c *scanController) IsPaused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

func (c *scanController) IsCanceled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.canceled
}

// ----------------------- 全局变量 -----------------------

var (
	// 扫描结果与进度
	scanResults  []ScanResult
	scanMutex    sync.Mutex
	scanCurrent  int
	scanTotal    int
	scanInFlight int
	scanPhase    string

	// 位置信息映射
	locationMap map[string]location
	locationMu  sync.RWMutex

	// 服务监听地址是否为回环地址；用于 WebSocket Origin 校验
	boundToLoopback bool

	// WebSocket 升级器
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return wsOriginAllowed(r.Header.Get("Origin"), r.Host, boundToLoopback)
		},
	}

	// 随机数生成器
	randomMu        sync.Mutex
	randomGenerator = rand.New(rand.NewSource(time.Now().UnixNano()))

	// 扫描控制
	scanCtrl = newScanController()

	// 全局任务锁（扫描 / 快速优选 / 重置资源互斥）
	taskMutex     sync.Mutex
	isTaskRunning bool
	taskKind      string

	// 运行参数
	httpPort    int
	bindHost    string
	dataDir     string
	openBrowser bool
	autoExit    bool
	idleTimeout time.Duration
)

// shouldOpenBrowser 综合命令行参数与环境变量判断是否需要自动打开浏览器
func shouldOpenBrowser() bool {
	if isTruthyEnv(os.Getenv("CFD_NO_BROWSER")) {
		return false
	}
	return openBrowser
}

func dataPath(name string) string {
	if dataDir == "" {
		return name
	}
	return filepath.Join(dataDir, name)
}

// ----------------------- 任务状态辅助 -----------------------

func tryStartTask(kind string) bool {
	taskMutex.Lock()
	defer taskMutex.Unlock()
	if isTaskRunning {
		return false
	}
	isTaskRunning = true
	taskKind = kind
	return true
}

func finishTask() {
	taskMutex.Lock()
	isTaskRunning = false
	taskKind = ""
	taskMutex.Unlock()
}

// finishScanTask 把「任务结束」和「清掉暂停/取消标记」放在同一把锁内，
// 避免 pause_task 恰好落在两步之间，把 paused 残留到下一次扫描。
func finishScanTask() {
	taskMutex.Lock()
	isTaskRunning = false
	taskKind = ""
	scanCtrl.Reset()
	scanMutex.Lock()
	scanPhase = ""
	scanMutex.Unlock()
	taskMutex.Unlock()
}

func taskSnapshot() (bool, string) {
	taskMutex.Lock()
	defer taskMutex.Unlock()
	return isTaskRunning, taskKind
}

// pauseScanTask 在任务状态锁内暂停扫描，避免与扫描收尾交错
func pauseScanTask() bool {
	taskMutex.Lock()
	defer taskMutex.Unlock()
	if !isTaskRunning || taskKind != "scan" {
		return false
	}
	return scanCtrl.Pause()
}

// resumeScanTask 在任务状态锁内继续扫描
func resumeScanTask() bool {
	taskMutex.Lock()
	defer taskMutex.Unlock()
	if !isTaskRunning || taskKind != "scan" {
		return false
	}
	return scanCtrl.Resume()
}

// stopScanTask 在任务状态锁内取消扫描
func stopScanTask() bool {
	taskMutex.Lock()
	defer taskMutex.Unlock()
	if !isTaskRunning || taskKind != "scan" {
		return false
	}
	return scanCtrl.Cancel()
}

func clampInt(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

// ----------------------- 主函数 -----------------------

func main() {
	flag.IntVar(&httpPort, "port", 8080, "网页服务监听端口")
	flag.StringVar(&bindHost, "host", "127.0.0.1", "监听地址；默认仅本机，局域网访问可设为 0.0.0.0 或指定网卡 IP")
	flag.StringVar(&dataDir, "data-dir", "", "资源文件目录，留空则使用当前目录")
	flag.BoolVar(&openBrowser, "open-browser", true, "服务启动后自动打开默认浏览器（设为 false 可关闭）")
	flag.BoolVar(&autoExit, "auto-exit", true, "网页全部关闭后自动退出程序（设为 false 可关闭）")
	flag.DurationVar(&idleTimeout, "idle-timeout", defaultIdleTimeout, "收不到网页心跳多久后退出程序，例如 30s")
	flag.Parse()

	if httpPort < 1 || httpPort > 65535 {
		fmt.Printf("端口无效: %d（有效范围 1-65535）\n", httpPort)
		os.Exit(2)
	}
	if strings.TrimSpace(bindHost) == "" {
		bindHost = "127.0.0.1"
	}
	if idleTimeout < 3*time.Second {
		fmt.Printf("idle-timeout 太短: %s（最少 3s）\n", idleTimeout)
		os.Exit(2)
	}

	if err := runServer(); err != nil {
		fmt.Printf("启动失败: %v\n", err)
		// 启动失败要以非 0 退出，方便脚本/快捷方式判断
		os.Exit(1)
	}
}

func runServer() error {
	loadFastHistory()
	prepareResourceFilesOnStartup(context.Background())

	mux := http.NewServeMux()

	// 只对根路径返回页面
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := staticFiles.ReadFile("index.html")
		if err != nil {
			http.Error(w, "无法加载页面", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	mux.HandleFunc("/ws", handleWebSocket)

	boundToLoopback = hostIsLoopback(bindHost)

	listenAddr := net.JoinHostPort(bindHost, strconv.Itoa(httpPort))
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}

	pageHost := bindHost
	if pageHost == "0.0.0.0" || pageHost == "::" {
		pageHost = "127.0.0.1"
	}
	pageURL := "http://" + net.JoinHostPort(pageHost, strconv.Itoa(httpPort))

	fmt.Printf("网页服务已启动: %s\n", pageURL)
	fmt.Printf("监听地址: %s\n", listenAddr)
	if dataDir != "" {
		fmt.Printf("资源目录: %s\n", dataDir)
	}
	if !boundToLoopback {
		fmt.Println("注意: 当前监听局域网地址，同一网络中的其他设备可以访问本程序（无额外鉴权）")
	}

	if shouldOpenBrowser() {
		fmt.Println("正在自动打开默认浏览器...")
		scheduleBrowserOpen(pageURL, 900*time.Millisecond)
	} else {
		fmt.Println("已关闭自动打开浏览器，请手动访问上面的地址")
	}

	startExitWatchdog()

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return server.Serve(listener)
}

// wsOriginAllowed 判断 WebSocket 握手是否允许
//
// boundToLoopback 为 true 时，要求请求的 Host 本身也是回环地址，
// 这样攻击者域名通过 DNS rebinding 指向 127.0.0.1 时会因为 Host 是攻击者
// 域名而被拒绝。监听局域网地址时，只接受 IP 字面量或 localhost，
// 且要求 Origin 与 Host 一致。
func wsOriginAllowed(origin, host string, boundToLoopback bool) bool {
	requestHost := hostnameOnly(host)
	if requestHost == "" {
		return false
	}
	if boundToLoopback {
		if !hostIsLoopback(requestHost) {
			return false
		}
	} else {
		if !hostIsLoopback(requestHost) && net.ParseIP(requestHost) == nil {
			return false
		}
	}

	if origin == "" {
		return true // 非浏览器客户端
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	originHost := parsed.Hostname()
	if boundToLoopback {
		return hostIsLoopback(originHost)
	}
	return strings.EqualFold(parsed.Host, host)
}

func hostnameOnly(hostPort string) string {
	if host, _, err := net.SplitHostPort(hostPort); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(hostPort, "[]")
}

func hostIsLoopback(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ----------------------- WebSocket 处理 -----------------------

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	rawWS, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Println("WebSocket 升级失败:", err)
		return
	}
	ws := &pageConn{Conn: rawWS}
	defer func() {
		untrackPage(ws)
		ws.Close()
	}()

	// 页面只会发送很小的 JSON 指令，限制上限防止异常客户端占满内存
	ws.SetReadLimit(1 << 20)

	trackPage(ws)
	startPagePing(ws)
	ws.SetPongHandler(func(string) error {
		trackPage(ws)
		refreshReadDeadline(ws)
		return nil
	})
	refreshReadDeadline(ws)

	// 新页面（含刷新后重连）先拿到完整任务状态，避免看不到正在跑的任务
	sendStateSync(ws)
	// IPv6 检测由页面通过 check_ipv6 指令发起，便于区分首次打开、重连与手动重测

	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			break
		}
		trackPage(ws)
		refreshReadDeadline(ws)

		var request struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(msg, &request); err != nil {
			continue
		}

		switch request.Type {
		case "heartbeat":
			// 兼容旧客户端；现在主要靠服务端 ping/pong 探活

		case "check_ipv6":
			// 页面打开 / 网络变化 / 用户点击胶囊时发起；force=true 忽略缓存重新探测
			var params struct {
				Force bool `json:"force"`
			}
			if err := json.Unmarshal(request.Data, &params); err != nil {
				params.Force = false
			}
			requestIPv6Check(ws, params.Force)
			// 与 IPv6 检测同步刷新当前 IPv4 出口与运营商，写进运行日志
			requestIPInfo(params.Force)

		case "shutdown":
			// 用户在网页上点了「关闭程序」
			requestShutdown(ws)
			return

		case "start_task":
			var params struct {
				IPType   int    `json:"ipType"`
				Threads  int    `json:"threads"`
				Port     int    `json:"port"`
				SpeedURL string `json:"speedUrl"`
			}
			if err := json.Unmarshal(request.Data, &params); err != nil {
				sendWSMessage(ws, "notice", "任务参数格式错误")
				continue
			}
			if strings.TrimSpace(params.SpeedURL) == "" {
				sendWSMessage(ws, "notice", "缺少测速 URL，请在网页中填写后再开始扫描")
				continue
			}
			speedHost, hostErr := speedURLHost(params.SpeedURL)
			if hostErr != nil {
				sendWSMessage(ws, "notice", "测速 URL 无效，请检查后重试")
				continue
			}
			if params.Port < 1 || params.Port > 65535 {
				sendWSMessage(ws, "notice", "端口无效")
				continue
			}
			if params.IPType != 4 && params.IPType != 6 {
				sendWSMessage(ws, "notice", "IP 类型无效，只能是 IPv4 或 IPv6")
				continue
			}
			if running, kind := taskSnapshot(); running && kind == "scan" && !scanCtrl.IsPaused() {
				// 重复点击开始：忽略即可
				sendWSMessage(ws, "notice", "扫描任务已在运行")
				continue
			}
			if !tryStartTask("scan") {
				sendWSMessage(ws, "notice", "已有任务正在运行，请等待完成后再试")
				continue
			}
			threads := params.Threads
			if threads <= 0 {
				threads = defaultThreads
			}
			threads = clampInt(threads, minScanThreads, maxScanThreads)
			ctx, cancel := context.WithCancel(context.Background())
			scanCtrl.SetCancel(cancel)
			go func() {
				defer cancel()
				runScanTask(ctx, params.IPType, threads, params.Port, speedHost)
			}()

		case "start_fast_task":
			var params struct {
				IPType     int      `json:"ipType"`
				Port       int      `json:"port"`
				SpeedURL   string   `json:"speedUrl"`
				TargetMbps int      `json:"targetMbps"`
				DCFilter   []string `json:"dcFilter"`
			}
			if err := json.Unmarshal(request.Data, &params); err != nil {
				sendWSMessage(ws, "notice", "快速优选参数格式错误")
				continue
			}
			if params.IPType != 4 && params.IPType != 6 {
				sendWSMessage(ws, "notice", "IP 类型无效，只能是 IPv4 或 IPv6")
				continue
			}
			if params.Port < 1 || params.Port > 65535 {
				sendWSMessage(ws, "notice", "端口无效")
				continue
			}
			if strings.TrimSpace(params.SpeedURL) == "" {
				sendWSMessage(ws, "notice", "缺少测速 URL，请填写后再开始快速优选")
				continue
			}
			if _, _, _, err := fastSpeedTarget(params.SpeedURL, params.Port); err != nil {
				sendWSMessage(ws, "notice", "测速 URL 无效，请检查后重试")
				continue
			}
			targetMbps := params.TargetMbps
			if targetMbps <= 0 {
				targetMbps = 50
			}
			if targetMbps > fastMaxTargetMbps {
				sendWSMessage(ws, "notice", fmt.Sprintf("目标带宽过大（最多 %d Mbps）", fastMaxTargetMbps))
				continue
			}
			dcFilter := normalizeDCFilter(params.DCFilter)
			if !tryStartTask("fast") {
				sendWSMessage(ws, "notice", "已有任务正在运行，请等待完成后再试")
				continue
			}
			speedURL := strings.TrimSpace(params.SpeedURL)
			ctx, cancel := context.WithCancel(context.Background())
			beginFastState(targetMbps, dcFilter, cancel)
			broadcastStateSync()
			go runFastScanTask(ctx, params.IPType, params.Port, speedURL, targetMbps, dcFilter)

		case "stop_fast_task":
			if stopFastScan() {
				broadcastWSMessage("log", "正在停止快速优选...")
			} else {
				sendWSMessage(ws, "notice", "当前没有正在运行的快速优选")
			}

		case "clear_fast_history":
			clearFastHistory()
			broadcastWSMessage("fast_history_cleared", nil)

		case "get_dc_options":
			sendWSMessage(ws, "dc_options", buildDCOptions())

		case "pause_task":
			if pauseScanTask() {
				broadcastScanState()
				broadcastScanSummary(true)
			} else {
				if _, kind := taskSnapshot(); kind != "scan" {
					sendWSMessage(ws, "notice", "当前没有正在运行的扫描任务")
				} else if scanCtrl.IsCanceled() {
					sendWSMessage(ws, "notice", "扫描正在取消")
				} else {
					sendWSMessage(ws, "notice", "扫描已处于暂停状态")
				}
				broadcastScanState()
			}

		case "resume_task":
			if resumeScanTask() {
				broadcastScanState()
				broadcastWSMessage("log", "继续扫描中...")
			} else {
				sendWSMessage(ws, "notice", "当前没有处于暂停状态的扫描任务")
				broadcastScanState()
			}

		case "stop_scan_task":
			if stopScanTask() {
				broadcastScanState()
				broadcastWSMessage("log", "正在取消扫描...")
				broadcastScanPhase("正在取消扫描...")
			} else {
				sendWSMessage(ws, "notice", "当前没有正在运行的扫描任务")
			}

		case "reset_resources":
			if !tryStartTask("reset") {
				sendWSMessage(ws, "notice", "已有任务正在运行，请等待完成后再试")
				continue
			}
			broadcastStateSync()
			go runResetResources()

		case "start_speed_test":
			var params struct {
				IP       string `json:"ip"`
				Port     int    `json:"port"`
				SpeedURL string `json:"speedUrl"`
			}
			if err := json.Unmarshal(request.Data, &params); err != nil {
				sendWSMessage(ws, "notice", "测速参数格式错误")
				continue
			}
			if net.ParseIP(strings.TrimSpace(params.IP)) == nil {
				sendWSMessage(ws, "speed_test_result", map[string]string{
					"ip":    params.IP,
					"speed": "IP无效",
				})
				continue
			}
			if params.Port < 1 || params.Port > 65535 {
				sendWSMessage(ws, "speed_test_result", map[string]string{
					"ip":    params.IP,
					"speed": "端口无效",
				})
				continue
			}
			if strings.TrimSpace(params.SpeedURL) == "" {
				sendWSMessage(ws, "speed_test_result", map[string]string{
					"ip":    params.IP,
					"speed": "缺少测速 URL",
				})
				sendWSMessage(ws, "log", "测速失败: 未提供测速 URL")
				continue
			}
			go runSpeedTest(ws, params.IP, params.Port, params.SpeedURL)
		}
	}
}

func sendWSMessage(ws *pageConn, msgType string, data any) error {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()
	_ = ws.SetWriteDeadline(time.Now().Add(writeWait))
	return ws.WriteJSON(map[string]any{
		"type": msgType,
		"data": data,
	})
}

func broadcastWSMessage(msgType string, data any) {
	for _, ws := range snapshotPages() {
		_ = sendWSMessage(ws, msgType, data)
	}
}

func logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Println(msg)
	broadcastWSMessage("log", msg)
}

func sendStateSync(ws *pageConn) {
	_ = sendWSMessage(ws, "state_sync", buildStateSync())
}

func broadcastStateSync() {
	broadcastWSMessage("state_sync", buildStateSync())
}

func broadcastScanState() {
	running, kind := taskSnapshot()
	broadcastWSMessage("scan_state", map[string]any{
		"running": running && kind == "scan",
		"paused":  scanCtrl.IsPaused(),
	})
}

// broadcastScanPhase 在扫描准备阶段（加载资源）推送进度文案。
func broadcastScanPhase(text string) {
	scanMutex.Lock()
	scanPhase = text
	scanMutex.Unlock()
	broadcastWSMessage("scan_progress", map[string]any{"phase": text})
}

func buildStateSync() stateSync {
	running, kind := taskSnapshot()

	scanMutex.Lock()
	scanCur := scanCurrent
	scanTot := scanTotal
	phase := scanPhase
	results := append([]ScanResult(nil), scanResults...)
	scanMutex.Unlock()

	var dcList []DataCenterInfo
	if len(results) > 0 {
		dcList = buildDCList(results)
	}
	if len(results) == 0 {
		results = nil
	}

	return stateSync{
		TaskRunning: running,
		TaskKind:    kind,
		ScanPaused:  running && kind == "scan" && scanCtrl.IsPaused(),
		ScanPhase:   phase,
		ScanCurrent: scanCur,
		ScanTotal:   scanTot,
		ScanResults: results,
		DCList:      dcList,
		Fast:        fastStateSnapshot(),
	}
}

// ----------------------- 资源加载 -----------------------

var fetchClient = &http.Client{Timeout: fetchTimeout}

func fetchURL(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := fetchClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadSize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxDownloadSize {
		return nil, fmt.Errorf("响应超过 %d 字节上限", maxDownloadSize)
	}
	if strings.TrimSpace(string(body)) == "" {
		return nil, fmt.Errorf("响应内容为空")
	}
	return body, nil
}

func saveToFileAtomic(filename string, data []byte) error {
	dir := filepath.Dir(filename)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, ".cfdata-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0644); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filename); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func loadLocations(ctx context.Context) {
	filename := dataPath("locations.json")

	var locations []location
	body, err := os.ReadFile(filename)
	if err == nil {
		if jsonErr := json.Unmarshal(body, &locations); jsonErr == nil && len(locations) > 0 {
			applyLocations(locations)
			logf("已加载 %d 个数据中心位置信息", len(locationSnapshot()))
			return
		}
		logf("本地 %s 内容无效，正在重新下载...", filename)
	} else if os.IsNotExist(err) {
		logf("本地 %s 不存在，正在下载 locations 数据...", filename)
	} else {
		logf("读取本地位置文件失败: %v", err)
	}

	body, err = fetchURL(ctx, locationsURL)
	if err != nil {
		if ctx.Err() == nil {
			logf("获取位置信息失败: %v", err)
		}
		return
	}
	if err := json.Unmarshal(body, &locations); err != nil {
		logf("解析位置信息 JSON 失败: %v", err)
		return
	}
	if len(locations) == 0 {
		logf("位置信息为空，忽略本次下载")
		return
	}
	if err := saveToFileAtomic(filename, body); err != nil {
		logf("警告: 保存位置信息文件失败: %v", err)
	} else {
		logf("locations 数据已保存到 %s", filename)
	}
	applyLocations(locations)
	logf("已加载 %d 个数据中心位置信息", len(locationSnapshot()))
}

func applyLocations(locations []location) {
	loadedMap := make(map[string]location, len(locations))
	for _, loc := range locations {
		if loc.Iata == "" {
			continue
		}
		loadedMap[loc.Iata] = loc
	}
	locationMu.Lock()
	locationMap = loadedMap
	locationMu.Unlock()
}

func locationSnapshot() map[string]location {
	locationMu.RLock()
	defer locationMu.RUnlock()
	result := make(map[string]location, len(locationMap))
	for k, v := range locationMap {
		result[k] = v
	}
	return result
}

// parseSubnets 校验并规范化 IP 段列表
//
// 当前扫描逻辑依赖 IPv4 /24 与 IPv6 /48，其它前缀直接报错，
// 避免出现“能跑但结果不对”的静默问题。
func parseSubnets(content string, ipType int) ([]string, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	seen := make(map[string]bool)
	var subnets []string
	lineNo := 0

	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ip, ipnet, err := net.ParseCIDR(line)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行不是合法 CIDR: %q", lineNo, line)
		}
		ones, bits := ipnet.Mask.Size()
		if ipType == 6 {
			if ip.To4() != nil || bits != 128 || ones != 48 {
				return nil, fmt.Errorf("第 %d 行不是 IPv6 /48: %q", lineNo, line)
			}
		} else {
			if ip.To4() == nil || bits != 32 || ones != 24 {
				return nil, fmt.Errorf("第 %d 行不是 IPv4 /24: %q", lineNo, line)
			}
		}
		canonical := ipnet.String()
		if !seen[canonical] {
			seen[canonical] = true
			subnets = append(subnets, canonical)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(subnets) == 0 {
		return nil, fmt.Errorf("IP 列表为空")
	}
	return subnets, nil
}

func loadSubnets(ctx context.Context, ipType int) ([]string, error) {
	var filename, downloadURL string
	if ipType == 6 {
		filename = dataPath("ips-v6.txt")
		downloadURL = ipsV6URL
	} else {
		filename = dataPath("ips-v4.txt")
		downloadURL = ipsV4URL
	}

	if body, err := os.ReadFile(filename); err == nil {
		subnets, parseErr := parseSubnets(string(body), ipType)
		if parseErr == nil {
			logf("读取本地 %s，共 %d 个子网", filename, len(subnets))
			return subnets, nil
		}
		logf("本地 %s 内容无效（%v），正在重新下载...", filename, parseErr)
	} else if os.IsNotExist(err) {
		logf("本地 %s 不存在，正在下载...", filename)
	} else {
		return nil, fmt.Errorf("读取本地 IP 列表失败: %w", err)
	}

	body, err := fetchURL(ctx, downloadURL)
	if err != nil {
		return nil, fmt.Errorf("下载 IP 列表失败: %w", err)
	}
	subnets, err := parseSubnets(string(body), ipType)
	if err != nil {
		return nil, fmt.Errorf("下载的 IP 列表无效: %w", err)
	}
	if err := saveToFileAtomic(filename, body); err != nil {
		logf("警告: 保存 IP 文件失败: %v", err)
	} else {
		logf("IP 列表已保存到 %s", filename)
	}
	return subnets, nil
}

// ----------------------- 扫描任务 -----------------------

// schemeForPort 与测速保持一致：HTTPS 端口走 TLS，其余走明文 HTTP
func schemeForPort(port int) string {
	switch port {
	case 443, 2053, 2083, 2087, 2096, 8443:
		return "https"
	default:
		return "http"
	}
}

// speedURLHost 从测速 URL 中取出 Host，用于扫描时的 SNI 和 Host 头
func speedURLHost(rawURL string) (string, error) {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return "", fmt.Errorf("测速 URL 不能为空")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return "", fmt.Errorf("测速 URL 无法解析")
	}
	return parsed.Hostname(), nil
}

// probeCloudflareIP 对 ip:port 做一次 TCP 连接 + HTTP(S) 请求，返回 TCP 握手耗时和 CF-RAY 数据中心。
// 扫描和快速优选的 RTT 探测共用这段逻辑；ctx 取消时会立即关闭连接，
// 不会卡在 ReadResponse 上等超时。
func probeCloudflareIP(ctx context.Context, ip string, port int, scheme, host string, insecureTLS bool) (time.Duration, string, error) {
	dialer := &net.Dialer{Timeout: scanConnectTimeout}
	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return 0, "", err
	}
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()

	rwc := net.Conn(conn)
	defer func() { _ = rwc.Close() }()
	tcpDuration := time.Since(start)

	if scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName:         host,
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: insecureTLS,
			NextProtos:         []string{"http/1.1"},
		})
		_ = tlsConn.SetDeadline(time.Now().Add(scanRequestTimeout))
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return 0, "", err
		}
		rwc = tlsConn
	}

	requestURL := scheme + "://" + fastHostForURL(host) + "/"
	req, err := http.NewRequestWithContext(ctx, "GET", requestURL, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Close = true

	_ = rwc.SetDeadline(time.Now().Add(scanRequestTimeout))
	if err := req.Write(rwc); err != nil {
		return 0, "", err
	}
	resp, err := http.ReadResponse(bufio.NewReader(rwc), req)
	if err != nil {
		return 0, "", err
	}
	_ = resp.Body.Close()

	dataCenter := fastExtractDataCenter(resp.Header.Get("CF-RAY"))
	if dataCenter == "" {
		return 0, "", fmt.Errorf("响应缺少 CF-RAY")
	}
	return tcpDuration, dataCenter, nil
}

func runScanTask(ctx context.Context, ipType, scanMaxThreads, scanPort int, scanHost string) {
	defer finishScanTask()

	scanScheme := schemeForPort(scanPort)

	scanMutex.Lock()
	scanResults = nil
	scanCurrent = 0
	scanTotal = 0
	scanInFlight = 0
	scanPhase = ""
	scanMutex.Unlock()

	broadcastStateSync()
	logf("开始扫描任务（%s 端口 %d，Host: %s）...", strings.ToUpper(scanScheme), scanPort, scanHost)

	broadcastScanPhase("正在准备数据中心位置和 IP 列表...")

	// locations 和 IP 列表互不依赖，并行加载；最坏等待从两次下载之和降为一次。
	var loadWG sync.WaitGroup
	loadWG.Add(2)
	go func() {
		defer loadWG.Done()
		loadLocations(ctx)
	}()
	var subnets []string
	var subnetsErr error
	go func() {
		defer loadWG.Done()
		subnets, subnetsErr = loadSubnets(ctx, ipType)
	}()
	loadWG.Wait()

	if ctx.Err() != nil {
		broadcastScanFinished(0, true)
		logf("扫描已取消")
		return
	}
	if subnetsErr != nil {
		broadcastWSMessage("error", subnetsErr.Error())
		return
	}

	var ipList []string
	if ipType == 6 {
		ipList = getRandomIPv6s(subnets)
	} else {
		ipList = getRandomIPv4s(subnets)
	}
	if ctx.Err() != nil {
		broadcastScanFinished(0, true)
		logf("扫描已取消")
		return
	}
	total := len(ipList)
	if total == 0 {
		broadcastWSMessage("error", "IP 列表为空，无法扫描")
		return
	}

	scanMutex.Lock()
	scanTotal = total
	scanPhase = ""
	scanMutex.Unlock()

	broadcastWSMessage("scan_progress", map[string]int{
		"current": 0,
		"total":   total,
		"matched": 0,
	})
	logf("正在扫描 %d 个 IP 地址（%s 端口 %d）...", total, strings.ToUpper(scanScheme), scanPort)

	var wg sync.WaitGroup
	thread := make(chan struct{}, scanMaxThreads)

	for _, ip := range ipList {
		scanCtrl.mu.Lock()
		for scanCtrl.paused && !scanCtrl.canceled {
			scanCtrl.cond.Wait()
		}
		canceled := scanCtrl.canceled
		scanCtrl.mu.Unlock()
		if canceled {
			break
		}

		thread <- struct{}{}
		scanCtrl.mu.Lock()
		canceled = scanCtrl.canceled
		scanCtrl.mu.Unlock()
		if canceled {
			<-thread
			break
		}

		wg.Add(1)
		scanMutex.Lock()
		scanInFlight++
		scanMutex.Unlock()
		go func(ip string) {
			defer func() {
				<-thread
				wg.Done()
				scanMutex.Lock()
				scanCurrent++
				currentCount := scanCurrent
				if scanInFlight > 0 {
					scanInFlight--
				}
				remaining := scanInFlight
				matchedCount := len(scanResults)
				scanMutex.Unlock()
				if currentCount%10 == 0 || currentCount == total {
					broadcastWSMessage("scan_progress", map[string]int{
						"current": currentCount,
						"total":   total,
						"matched": matchedCount,
					})
				}
				// 暂停瞬间还在途的请求会继续完成；等它们全部落地后再补一份汇总，
				// 否则暂停时看到的 dcList 可能不包含这批刚返回的结果。
				if remaining == 0 && scanCtrl.IsPaused() {
					broadcastScanSummary(true)
				}
			}()

			tcpDuration, dataCenter, err := probeCloudflareIP(ctx, ip, scanPort, scanScheme, scanHost, false)
			if err != nil {
				return
			}

			locationMu.RLock()
			loc := locationMap[dataCenter]
			locationMu.RUnlock()

			res := ScanResult{
				IP:          ip,
				DataCenter:  dataCenter,
				Region:      loc.Region,
				City:        loc.City,
				LatencyStr:  fmt.Sprintf("%d ms", tcpDuration.Milliseconds()),
				TCPDuration: tcpDuration,
			}
			scanMutex.Lock()
			scanResults = append(scanResults, res)
			scanMutex.Unlock()
			broadcastWSMessage("scan_result", res)
		}(ip)
	}
	wg.Wait()

	wasCanceled := scanCtrl.IsCanceled()
	wasPaused := scanCtrl.IsPaused()
	scanMutex.Lock()
	resultsCount := len(scanResults)
	scanMutex.Unlock()

	if wasCanceled {
		broadcastScanFinished(resultsCount, true)
		logf("扫描已取消，保留 %d 个已发现 IP", resultsCount)
		return
	}
	if resultsCount == 0 {
		broadcastWSMessage("error", "扫描完成，但未发现任何有效 IP。请检查网络状态或尝试更换 IP 类型。")
		return
	}
	if wasPaused {
		logf("已暂停时所有 IP 已派发完毕，扫描任务结束")
	}
	broadcastScanFinished(resultsCount, false)
	logf("扫描完成，请选择数据中心查看 IP 并测速")
}

func broadcastScanSummary(paused bool) {
	scanMutex.Lock()
	results := append([]ScanResult(nil), scanResults...)
	scanMutex.Unlock()

	if len(results) == 0 {
		broadcastWSMessage("log", "已暂停扫描，当前还没有扫描到有效 IP，可继续扫描")
		return
	}
	broadcastWSMessage("log", "扫描已暂停，已整理当前已扫描到的 IP")
	broadcastWSMessage("scan_summary", map[string]any{
		"dcList":  buildDCList(results),
		"count":   len(results),
		"paused":  paused,
		"results": results,
	})
}

func broadcastScanFinished(resultsCount int, canceled bool) {
	scanMutex.Lock()
	results := append([]ScanResult(nil), scanResults...)
	scanMutex.Unlock()
	broadcastWSMessage("scan_finished", map[string]any{
		"dcList":   buildDCList(results),
		"count":    resultsCount,
		"paused":   false,
		"canceled": canceled,
		"results":  results,
	})
}

func buildDCList(results []ScanResult) []DataCenterInfo {
	dcMap := make(map[string]*DataCenterInfo)
	for _, res := range results {
		info, ok := dcMap[res.DataCenter]
		if !ok {
			info = &DataCenterInfo{
				DataCenter: res.DataCenter,
				Region:     res.Region,
				City:       res.City,
				MinLatency: 999999,
			}
			dcMap[res.DataCenter] = info
		}
		info.IPCount++
		lat := int(res.TCPDuration.Milliseconds())
		if lat < info.MinLatency {
			info.MinLatency = lat
		}
	}

	dcList := make([]DataCenterInfo, 0, len(dcMap))
	for _, info := range dcMap {
		dcList = append(dcList, *info)
	}
	sort.Slice(dcList, func(i, j int) bool {
		return dcList[i].MinLatency < dcList[j].MinLatency
	})
	return dcList
}

// ----------------------- 测速 -----------------------

var errSpeedSampleComplete = errors.New("speed sample complete")

// newIPBoundHTTPClient 创建固定从 ip:port 出站的 HTTP 客户端
func newIPBoundHTTPClient(ip string, port int) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialer := &net.Dialer{Timeout: 10 * time.Second}
			return dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
		},
		TLSHandshakeTimeout: 10 * time.Second,
		DisableKeepAlives:   true,
		DisableCompression:  true,
	}
	return &http.Client{Transport: transport, Timeout: 20 * time.Second}
}

// peakDownloadSpeed 统计下载速度：500ms 窗口峰值与整体平均取大，
// 小文件几百毫秒下完时也能得到非 0 结果；EOF 正常完成，其他读取错误原样返回。
func peakDownloadSpeed(body io.Reader) (speedMBps float64, totalBytes int64, err error) {
	buf := make([]byte, 32*1024)
	start := time.Now()
	lastTime := start
	var lastBytes int64
	maxBucketSpeed := float64(0)

	for {
		n, readErr := body.Read(buf)
		totalBytes += int64(n)
		now := time.Now()
		if elapsed := now.Sub(lastTime); elapsed >= 500*time.Millisecond {
			speed := float64(totalBytes-lastBytes) / elapsed.Seconds() / 1024 / 1024
			if speed > maxBucketSpeed {
				maxBucketSpeed = speed
			}
			lastBytes = totalBytes
			lastTime = now
		}
		if readErr != nil {
			if readErr != io.EOF {
				err = readErr
			}
			break
		}
	}

	elapsed := time.Since(start)
	overallSpeed := float64(0)
	if elapsed > 0 {
		overallSpeed = float64(totalBytes) / elapsed.Seconds() / 1024 / 1024
	}
	return math.Max(maxBucketSpeed, overallSpeed), totalBytes, err
}

func runSpeedTest(ws *pageConn, ip string, port int, speedURL string) {
	logf("开始对 IP %s 端口 %d 进行测速...", ip, port)

	scheme, host, uri, err := fastSpeedTarget(speedURL, port)
	if err != nil {
		sendWSMessage(ws, "speed_test_result", map[string]string{
			"ip":    ip,
			"speed": "URL解析错误",
		})
		return
	}

	client := newIPBoundHTTPClient(ip, port)
	sampleCtx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	req, err := http.NewRequestWithContext(sampleCtx, "GET", scheme+"://"+fastHostForURL(host)+uri, nil)
	if err != nil {
		sendWSMessage(ws, "speed_test_result", map[string]string{
			"ip":    ip,
			"speed": "URL解析错误",
		})
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := client.Do(req)
	if err != nil {
		sendWSMessage(ws, "speed_test_result", map[string]string{
			"ip":    ip,
			"speed": "连接错误",
		})
		logf("测速失败: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text := fmt.Sprintf("HTTP %d 错误", resp.StatusCode)
		sendWSMessage(ws, "speed_test_result", map[string]string{
			"ip":    ip,
			"speed": text,
		})
		logf("IP %s 测速失败: HTTP %s", ip, resp.Status)
		return
	}

	// 成功响应头之后才开始 5 秒采样，不把建连等待计入采样窗口。
	stopTimer := time.AfterFunc(5*time.Second, func() {
		cancel(errSpeedSampleComplete)
	})
	defer stopTimer.Stop()

	speedValue, totalBytes, readErr := peakDownloadSpeed(resp.Body)
	// 只接受读取错误本身携带的采样标记，不能用 context.Cause 掩盖真实截断。
	if readErr != nil && !errors.Is(readErr, errSpeedSampleComplete) {
		sendWSMessage(ws, "speed_test_result", map[string]string{
			"ip":    ip,
			"speed": "下载错误",
		})
		logf("IP %s 测速失败: 下载错误: %v", ip, readErr)
		return
	}
	if totalBytes == 0 {
		sendWSMessage(ws, "speed_test_result", map[string]string{
			"ip":    ip,
			"speed": "无数据",
		})
		logf("IP %s 测速失败: 响应没有数据", ip)
		return
	}
	speedStr := fmt.Sprintf("%.2f MB/s", speedValue)

	sendWSMessage(ws, "speed_test_result", map[string]string{
		"ip":    ip,
		"speed": speedStr,
	})
	logf("IP %s 测速完成: %s", ip, speedStr)
}

func runResetResources() {
	defer finishTask()

	files := []string{
		dataPath("locations.json"),
		dataPath("ips-v4.txt"),
		dataPath("ips-v6.txt"),
	}

	deleted := make([]string, 0, len(files))
	var deleteErrors []string
	for _, file := range files {
		if err := os.Remove(file); err != nil {
			if !os.IsNotExist(err) {
				deleteErrors = append(deleteErrors, fmt.Sprintf("%s: %v", filepath.Base(file), err))
				logf("删除旧资源 %s 失败: %v", file, err)
			}
			continue
		}
		deleted = append(deleted, filepath.Base(file))
	}

	locationMu.Lock()
	locationMap = nil
	locationMu.Unlock()
	broadcastWSMessage("reset_progress", map[string]any{
		"current": 0,
		"total":   len(resourceSpecs()),
		"text":    "正在重新下载资源文件...",
	})
	logf("正在重新下载 locations、IPv4、IPv6 资源...")

	outcomes := ensureResourceFiles(context.Background(), true, func(current, total int, text string) {
		logf("%s", text)
		broadcastWSMessage("reset_progress", map[string]any{
			"current": current,
			"total":   total,
			"text":    text,
		})
	})
	applyLocationsFromFile()
	broadcastWSMessage("dc_options", buildDCOptions())

	message, downloaded, fallback, failed := resourceOutcomeSummary(outcomes, deleteErrors)
	broadcastWSMessage("reset_resources_done", map[string]any{
		"deleted":    deleted,
		"downloaded": downloaded,
		"fallback":   fallback,
		"failed":     failed,
		"message":    message,
	})
	logf("%s", message)
}

// ----------------------- 子网/IP 工具 -----------------------

func nextRandomIntn(n int) int {
	randomMu.Lock()
	defer randomMu.Unlock()
	return randomGenerator.Intn(n)
}

func getRandomIPv4s(ipList []string) []string {
	randomIPs := make([]string, 0, len(ipList))
	for _, subnet := range ipList {
		_, ipnet, err := net.ParseCIDR(subnet)
		if err != nil || ipnet.IP.To4() == nil {
			continue
		}
		ip := make(net.IP, net.IPv4len)
		copy(ip, ipnet.IP.To4())
		ip[3] = byte(nextRandomIntn(256))
		randomIPs = append(randomIPs, ip.String())
	}
	return randomIPs
}

func getRandomIPv6s(ipList []string) []string {
	randomIPs := make([]string, 0, len(ipList))
	for _, subnet := range ipList {
		_, ipnet, err := net.ParseCIDR(subnet)
		if err != nil || ipnet.IP.To4() != nil || len(ipnet.IP) != net.IPv6len {
			continue
		}
		ip := make(net.IP, net.IPv6len)
		copy(ip, ipnet.IP)
		// /48 前 6 字节固定，后 10 字节随机
		for i := 6; i < net.IPv6len; i++ {
			ip[i] = byte(nextRandomIntn(256))
		}
		randomIPs = append(randomIPs, ip.String())
	}
	return randomIPs
}
