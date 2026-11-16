package cfdata

// 快速优选：抽样 + RTT 筛选 + 顺序测速 + 达标早停。
// 逻辑参考 yuanma/cfip，但复用 build3.0 的端口、TLS、测速 URL 和任务状态。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	fastSampleSize      = 100
	fastRTTConcurrency  = 50
	fastTopSpeedTargets = 10
	fastHistoryLimit    = 20
	fastMaxTargetMbps   = 10000
	fastHistoryFile     = "fast_history.json"
)

type fastRTTResult struct {
	IP         string
	LatencyMs  int
	DataCenter string
	City       string
	Region     string
}

type fastHistoryEntry struct {
	Round      int    `json:"round"`
	IP         string `json:"ip"`
	DataCenter string `json:"dataCenter"`
	City       string `json:"city"`
	Region     string `json:"region"`
	LatencyMs  int    `json:"latencyMs"`
	SpeedKB    int    `json:"speedKb"`
	RealMbps   int    `json:"realMbps"`
	TargetMbps int    `json:"targetMbps"`
	TargetKB   int    `json:"targetKb"`
	Met        bool   `json:"met"`
	Tested     int    `json:"tested"`
	Valid      int    `json:"valid"`
	ElapsedSec int    `json:"elapsedSec"`
	Timestamp  int64  `json:"timestamp"`
}

type fastResult struct {
	IP         string `json:"ip"`
	DataCenter string `json:"dataCenter"`
	City       string `json:"city"`
	Region     string `json:"region"`
	LatencyMs  int    `json:"latencyMs"`
	SpeedKB    int    `json:"speedKb"`
	RealMbps   int    `json:"realMbps"`
	TargetMbps int    `json:"targetMbps"`
	Round      int    `json:"round"`
	ElapsedSec int    `json:"elapsedSec"`
}

type fastSnapshot struct {
	Running    bool               `json:"running"`
	Round      int                `json:"round"`
	Phase      string             `json:"phase"`
	Current    int                `json:"current"`
	Total      int                `json:"total"`
	CurrentIP  string             `json:"currentIp"`
	TargetMbps int                `json:"targetMbps"`
	DCFilter   []string           `json:"dcFilter"`
	History    []fastHistoryEntry `json:"history"`
	Result     *fastResult        `json:"result"`
	Message    string             `json:"message"`
}

type dcOption struct {
	Code   string `json:"code"`
	City   string `json:"city"`
	Region string `json:"region"`
	Cca2   string `json:"cca2"`
}

var (
	fastMutex  sync.Mutex
	fastInfo   fastSnapshot
	fastCancel context.CancelFunc
)

func copyStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}

func fastSnapshotCopy() fastSnapshot {
	fastMutex.Lock()
	defer fastMutex.Unlock()
	snapshot := fastInfo
	snapshot.DCFilter = copyStrings(fastInfo.DCFilter)
	snapshot.History = append([]fastHistoryEntry(nil), fastInfo.History...)
	if fastInfo.Result != nil {
		result := *fastInfo.Result
		snapshot.Result = &result
	}
	return snapshot
}

func buildDCOptions() []dcOption {
	locationMu.RLock()
	defer locationMu.RUnlock()
	options := make([]dcOption, 0, len(locationMap))
	for code, loc := range locationMap {
		if code == "" {
			continue
		}
		options = append(options, dcOption{Code: code, City: loc.City, Region: loc.Region, Cca2: loc.Cca2})
	}
	sort.Slice(options, func(i, j int) bool {
		return options[i].Code < options[j].Code
	})
	return options
}

func lookupLocationInfo(code string) (city, region string) {
	locationMu.RLock()
	defer locationMu.RUnlock()
	loc := locationMap[code]
	return loc.City, loc.Region
}

// loadFastHistory 在启动时恢复上次保存的历史优选记录；文件损坏或不存在时静默忽略。
func loadFastHistory() {
	body, err := os.ReadFile(dataPath(fastHistoryFile))
	if err != nil {
		return
	}
	var history []fastHistoryEntry
	if err := json.Unmarshal(body, &history); err != nil {
		logf("警告: 历史优选记录格式无效，已忽略")
		return
	}
	if len(history) > fastHistoryLimit {
		history = history[len(history)-fastHistoryLimit:]
	}
	fastMutex.Lock()
	fastInfo.History = append([]fastHistoryEntry(nil), history...)
	fastMutex.Unlock()
	if len(history) > 0 {
		logf("已加载 %d 条历史优选 IP 记录", len(history))
	}
}

func saveFastHistory(history []fastHistoryEntry) {
	body, err := json.Marshal(history)
	if err != nil {
		return
	}
	if err := saveToFileAtomic(dataPath(fastHistoryFile), body); err != nil {
		logf("警告: 保存历史优选记录失败: %v", err)
	}
}

func clearFastHistory() {
	fastMutex.Lock()
	fastInfo.History = nil
	fastMutex.Unlock()
	if err := os.Remove(dataPath(fastHistoryFile)); err != nil && !os.IsNotExist(err) {
		logf("警告: 删除历史优选记录失败: %v", err)
	}
}

func beginFastState(targetMbps int, dcFilter []string, cancel context.CancelFunc) {
	fastMutex.Lock()
	fastInfo = fastSnapshot{
		Running:    true,
		Phase:      "starting",
		TargetMbps: targetMbps,
		DCFilter:   copyStrings(dcFilter),
		History:    append([]fastHistoryEntry(nil), fastInfo.History...),
		Message:    "正在初始化...",
	}
	fastCancel = cancel
	fastMutex.Unlock()
}

func setFastProgress(round int, phase string, current, total int, currentIP string) {
	fastMutex.Lock()
	if !fastInfo.Running {
		fastMutex.Unlock()
		return
	}
	fastInfo.Round = round
	fastInfo.Phase = phase
	fastInfo.Current = current
	fastInfo.Total = total
	fastInfo.CurrentIP = currentIP
	fastMutex.Unlock()
	broadcastWSMessage("fast_progress", map[string]any{
		"round":     round,
		"phase":     phase,
		"current":   current,
		"total":     total,
		"currentIp": currentIP,
	})
}

func appendFastBatch(entry fastHistoryEntry) {
	fastMutex.Lock()
	fastInfo.History = append(fastInfo.History, entry)
	if len(fastInfo.History) > fastHistoryLimit {
		fastInfo.History = append([]fastHistoryEntry(nil), fastInfo.History[len(fastInfo.History)-fastHistoryLimit:]...)
	}
	history := append([]fastHistoryEntry(nil), fastInfo.History...)
	fastMutex.Unlock()
	saveFastHistory(history)
	broadcastWSMessage("fast_batch", entry)
}

func stopFastScan() bool {
	fastMutex.Lock()
	running := fastInfo.Running
	cancel := fastCancel
	fastMutex.Unlock()
	if !running || cancel == nil {
		return false
	}
	cancel()
	return true
}

func sampleSubnets(list []string, n int) []string {
	shuffled := make([]string, len(list))
	copy(shuffled, list)
	randomMu.Lock()
	randomGenerator.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})
	randomMu.Unlock()
	if n > len(shuffled) {
		n = len(shuffled)
	}
	return shuffled[:n]
}

func fastHostForURL(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func fastExtractDataCenter(cfRay string) string {
	parts := strings.Split(strings.TrimSpace(cfRay), "-")
	if len(parts) < 2 {
		return ""
	}
	return strings.TrimSpace(parts[len(parts)-1])
}

// fastTestRTT 做 3 次 TCP 探测，全部成功才返回平均值；
// TLS/HTTP 阶段使用 3 秒超时，避免高延迟 IP 被 1 秒总 deadline 误杀。
func fastTestRTT(ctx context.Context, ip string, port int, scheme, host string) (int, string, bool) {
	var totalMs int
	var dataCenter string

	for i := 0; i < 3; i++ {
		if ctx.Err() != nil {
			return 0, "", false
		}

		tcpDuration, dc, err := probeCloudflareIP(ctx, ip, port, scheme, host, true)
		if err != nil {
			return 0, "", false
		}
		dataCenter = dc
		totalMs += int(tcpDuration.Milliseconds())
	}

	return totalMs / 3, dataCenter, true
}

func runFastRTT(ctx context.Context, round int, ips []string, concurrency, port int, scheme, host string, allowed map[string]bool) []fastRTTResult {
	if len(ips) == 0 {
		return nil
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if len(ips) < concurrency {
		concurrency = len(ips)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var count int
	results := make([]fastRTTResult, 0, len(ips))
	sem := make(chan struct{}, concurrency)
	total := len(ips)

	for _, ip := range ips {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(ip string) {
			defer func() {
				<-sem
				wg.Done()
				mu.Lock()
				count++
				current := count
				mu.Unlock()
				if current%10 == 0 || current == total {
					setFastProgress(round, "rtt", current, total, ip)
				}
			}()

			if ctx.Err() != nil {
				return
			}
			latency, dc, ok := fastTestRTT(ctx, ip, port, scheme, host)
			if !ok || dc == "" {
				return
			}
			if len(allowed) > 0 && !allowed[dc] {
				return
			}
			city, region := lookupLocationInfo(dc)
			mu.Lock()
			results = append(results, fastRTTResult{
				IP:         ip,
				LatencyMs:  latency,
				DataCenter: dc,
				City:       city,
				Region:     region,
			})
			mu.Unlock()
		}(ip)
	}
	wg.Wait()

	if ctx.Err() != nil {
		return nil
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].LatencyMs < results[j].LatencyMs
	})
	if len(results) > fastTopSpeedTargets {
		results = results[:fastTopSpeedTargets]
	}
	return results
}

func fastSpeedTarget(rawURL string, port int) (scheme, host, uri string, err error) {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return "", "", "", fmt.Errorf("缺少测速 URL")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return "", "", "", fmt.Errorf("测速 URL 无法解析")
	}
	uri = parsed.RequestURI()
	if uri == "" {
		uri = "/"
	}
	return schemeForPort(port), parsed.Hostname(), uri, nil
}

// measureFastSpeed 用所选端口/测速 URL 下载一次，返回峰值速度(kB/s)和
// CF-RAY 数据中心。统计口径与完整模式的测速一致。
func measureFastSpeed(ctx context.Context, ip string, port int, scheme, host, uri string) (int, string, error) {
	client := newIPBoundHTTPClient(ip, port)
	sampleCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	fullURL := scheme + "://" + fastHostForURL(host) + uri
	req, err := http.NewRequestWithContext(sampleCtx, "GET", fullURL, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, "", fmt.Errorf("HTTP %s", resp.Status)
	}

	dataCenter := fastExtractDataCenter(resp.Header.Get("CF-RAY"))
	// 成功响应头之后才开始 5 秒采样，不把建连等待计入采样窗口。
	stopTimer := time.AfterFunc(5*time.Second, func() {
		cancel(errSpeedSampleComplete)
	})
	defer stopTimer.Stop()

	speedValue, totalBytes, readErr := peakDownloadSpeed(resp.Body)
	// 只接受读取错误本身携带的采样标记，不能用 context.Cause 掩盖真实截断。
	if readErr != nil && !errors.Is(readErr, errSpeedSampleComplete) {
		return 0, dataCenter, fmt.Errorf("下载错误: %w", readErr)
	}
	if totalBytes == 0 {
		return 0, dataCenter, fmt.Errorf("响应没有数据")
	}
	return int(math.Round(speedValue * 1024)), dataCenter, nil
}

func runFastScanTask(ctx context.Context, ipType, port int, speedURL string, targetMbps int, dcFilter []string) {
	startTime := time.Now()
	targetKB := targetMbps * 128
	finishMessage := ""

	defer func() {
		fastMutex.Lock()
		fastInfo.Running = false
		if finishMessage != "" {
			fastInfo.Message = finishMessage
		}
		if fastInfo.Message == "" {
			fastInfo.Message = "快速优选已结束"
		}
		fastCancel = nil
		reason := fastInfo.Message
		hasResult := fastInfo.Result != nil
		fastMutex.Unlock()

		// 先发结束消息再释放任务锁，保证新任务不会抢在旧结束消息之前启动
		broadcastWSMessage("fast_finished", map[string]any{
			"reason":    reason,
			"hasResult": hasResult,
		})
		finishTask()
		broadcastStateSync()
	}()

	allowed := make(map[string]bool, len(dcFilter))
	for _, code := range dcFilter {
		code = strings.TrimSpace(strings.ToUpper(code))
		if code != "" {
			allowed[code] = true
		}
	}

	scheme, host, uri, err := fastSpeedTarget(speedURL, port)
	if err != nil {
		finishMessage = "快速优选失败: " + err.Error()
		logf("%s", finishMessage)
		return
	}

	logf("开始快速优选（目标 %d Mbps，IP%s，%s 端口 %d，每轮 %d 个子网）...",
		targetMbps, map[bool]string{true: "v6", false: "v4"}[ipType == 6], strings.ToUpper(scheme), port, fastSampleSize)

	loadLocations(ctx)
	if ctx.Err() != nil {
		finishMessage = "已停止快速优选"
		return
	}

	subnets, err := loadSubnets(ctx, ipType)
	if err != nil {
		if ctx.Err() != nil {
			finishMessage = "已停止快速优选"
			return
		}
		finishMessage = "快速优选失败: " + err.Error()
		logf("%s", finishMessage)
		return
	}
	if len(subnets) == 0 {
		finishMessage = "快速优选失败: IP 列表为空"
		logf("%s", finishMessage)
		return
	}

	round := 0
	for {
		if ctx.Err() != nil {
			finishMessage = "已停止快速优选"
			return
		}
		round++

		sampled := sampleSubnets(subnets, fastSampleSize)
		var ips []string
		if ipType == 6 {
			ips = getRandomIPv6s(sampled)
		} else {
			ips = getRandomIPv4s(sampled)
		}
		if len(ips) == 0 {
			logf("第 %d 轮没有生成可用 IP，继续下一轮", round)
			continue
		}

		setFastProgress(round, "rtt", 0, len(ips), "")
		candidates := runFastRTT(ctx, round, ips, fastRTTConcurrency, port, scheme, host, allowed)
		if ctx.Err() != nil {
			finishMessage = "已停止快速优选"
			return
		}
		if len(candidates) == 0 {
			logf("第 %d 轮没有符合条件的数据中心 IP，继续下一轮", round)
			continue
		}

		logf("第 %d 轮 RTT 完成：保留延迟最低的 %d 个进入测速", round, len(candidates))
		setFastProgress(round, "speed", 0, len(candidates), "")

		var best *fastHistoryEntry
		tested := 0
		for i, candidate := range candidates {
			if ctx.Err() != nil {
				finishMessage = "已停止快速优选"
				return
			}
			setFastProgress(round, "speed", i+1, len(candidates), candidate.IP)
			speedKB, speedDC, err := measureFastSpeed(ctx, candidate.IP, port, scheme, host, uri)
			if ctx.Err() != nil {
				finishMessage = "已停止快速优选"
				return
			}
			tested++
			if err != nil {
				logf("第 %d 轮测速 %s 失败: %v", round, candidate.IP, err)
				continue
			}
			if speedKB <= 0 {
				logf("第 %d 轮测速 %s 没有测到有效速度", round, candidate.IP)
				continue
			}
			if speedDC != "" && len(allowed) > 0 && !allowed[speedDC] {
				logf("第 %d 轮 %s 的数据中心 %s 不在筛选范围内，跳过", round, candidate.IP, speedDC)
				continue
			}

			entry := fastHistoryEntry{
				Round:      round,
				IP:         candidate.IP,
				DataCenter: candidate.DataCenter,
				City:       candidate.City,
				Region:     candidate.Region,
				LatencyMs:  candidate.LatencyMs,
				SpeedKB:    speedKB,
				RealMbps:   speedKB / 128,
				TargetMbps: targetMbps,
				TargetKB:   targetKB,
				Tested:     tested,
				Valid:      len(candidates),
				Timestamp:  time.Now().Unix(),
			}
			if best == nil || entry.SpeedKB > best.SpeedKB {
				copyEntry := entry
				best = &copyEntry
			}

			if speedKB >= targetKB {
				entry.Met = true
				entry.ElapsedSec = int(time.Since(startTime).Seconds())
				result := fastResult{
					IP:         candidate.IP,
					DataCenter: candidate.DataCenter,
					City:       candidate.City,
					Region:     candidate.Region,
					LatencyMs:  candidate.LatencyMs,
					SpeedKB:    speedKB,
					RealMbps:   entry.RealMbps,
					TargetMbps: targetMbps,
					Round:      round,
					ElapsedSec: entry.ElapsedSec,
				}
				fastMutex.Lock()
				fastInfo.Result = &result
				fastInfo.Message = "已找到达标 IP"
				fastMutex.Unlock()
				appendFastBatch(entry)
				broadcastWSMessage("fast_result", result)
				logf("第 %d 轮找到达标 IP：%s，%d kB/s（约 %d Mbps），延迟 %d ms，数据中心 %s",
					round, candidate.IP, speedKB, entry.RealMbps, candidate.LatencyMs, candidate.DataCenter)
				finishMessage = "已找到达标 IP"
				return
			}
		}

		if best == nil {
			fallback := candidates[0]
			best = &fastHistoryEntry{
				Round:      round,
				IP:         fallback.IP,
				DataCenter: fallback.DataCenter,
				City:       fallback.City,
				Region:     fallback.Region,
				LatencyMs:  fallback.LatencyMs,
				TargetMbps: targetMbps,
				TargetKB:   targetKB,
				Tested:     tested,
				Valid:      len(candidates),
				Timestamp:  time.Now().Unix(),
			}
		}
		best.Met = false
		best.Tested = tested
		best.ElapsedSec = int(time.Since(startTime).Seconds())
		appendFastBatch(*best)
		logf("第 %d 轮未达到 %d Mbps，本批最佳 %s（%s，%d ms，%d kB/s），继续换批",
			round, targetMbps, best.IP, best.DataCenter, best.LatencyMs, best.SpeedKB)
	}
}

func normalizeDCFilter(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		code := strings.ToUpper(strings.TrimSpace(value))
		if code == "" || seen[code] {
			continue
		}
		seen[code] = true
		result = append(result, code)
	}
	return result
}

func fastStateSnapshot() fastSnapshot {
	return fastSnapshotCopy()
}
