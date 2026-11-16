package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNormalizeDCFilter(t *testing.T) {
	got := normalizeDCFilter([]string{" hkg ", "HKG", "", "sjc", "Sjc", "nrt"})
	want := []string{"HKG", "SJC", "NRT"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("normalizeDCFilter() = %v, 期望 %v", got, want)
	}
	if len(normalizeDCFilter(nil)) != 0 {
		t.Fatal("空输入应返回空列表")
	}
}

func TestSampleSubnets(t *testing.T) {
	list := make([]string, 200)
	for i := range list {
		list[i] = fmt.Sprintf("10.%d.%d.0/24", i/256, i%256)
	}
	got := sampleSubnets(list, fastSampleSize)
	if len(got) != fastSampleSize {
		t.Fatalf("抽样数量 = %d, 期望 %d", len(got), fastSampleSize)
	}
	seen := make(map[string]bool, len(got))
	for _, item := range got {
		if seen[item] {
			t.Fatalf("抽样结果有重复: %s", item)
		}
		seen[item] = true
	}

	small := []string{"a", "b"}
	if got := sampleSubnets(small, fastSampleSize); len(got) != 2 {
		t.Fatalf("小列表抽样数量 = %d, 期望 2", len(got))
	}
}

func TestFastSpeedTarget(t *testing.T) {
	scheme, host, uri, err := fastSpeedTarget("speed.cloudflare.com/__down?bytes=10000000", 443)
	if err != nil {
		t.Fatalf("fastSpeedTarget() error = %v", err)
	}
	if scheme != "https" || host != "speed.cloudflare.com" || uri != "/__down?bytes=10000000" {
		t.Fatalf("fastSpeedTarget() = %q %q %q", scheme, host, uri)
	}

	scheme, _, uri, err = fastSpeedTarget("https://example.com/path", 80)
	if err != nil {
		t.Fatal(err)
	}
	if scheme != "http" || uri != "/path" {
		t.Fatalf("端口 80 应使用 http，实际 = %q %q", scheme, uri)
	}

	if _, _, _, err := fastSpeedTarget("", 443); err == nil {
		t.Fatal("空 URL 应报错")
	}
	if _, _, _, err := fastSpeedTarget("http://", 443); err == nil {
		t.Fatal("无主机 URL 应报错")
	}
}

func TestFastExtractDataCenter(t *testing.T) {
	cases := map[string]string{
		"8a1b2c3d4e5f6789-HKG": "HKG",
		"abc-SJC":              "SJC",
		"":                     "",
		"no-dash":              "dash",
		"nodash":               "",
		"a-b-NRT":              "NRT",
	}
	for input, want := range cases {
		if got := fastExtractDataCenter(input); got != want {
			t.Errorf("fastExtractDataCenter(%q) = %q, 期望 %q", input, got, want)
		}
	}
}

func TestMeasureFastSpeedResponses(t *testing.T) {
	server := newSpeedResponseServer(t)
	ip, port := speedServerAddress(server)
	for _, tc := range []struct {
		path    string
		wantErr string
	}{
		{"/ok", ""}, {"/gzip", ""}, {"/empty", "响应没有数据"},
		{"/truncated", "下载错误"}, {"/broken-chunked", "下载错误"},
		{"/status", "HTTP 503"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			speed, dc, err := measureFastSpeed(context.Background(), ip, port, "http", ip, tc.path)
			if tc.wantErr == "" {
				if err != nil || speed <= 0 || dc != "HKG" {
					t.Fatalf("measureFastSpeed() = %d, %q, %v", speed, dc, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || speed != 0 {
				t.Fatalf("measureFastSpeed() = %d, %q, %v; want zero speed and %q", speed, dc, err, tc.wantErr)
			}
			if tc.wantErr == "下载错误" && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("截断错误被掩盖: %v", err)
			}
		})
	}
}

func TestMeasureFastSpeedExternalCancellation(t *testing.T) {
	for _, cause := range []error{context.Canceled, errors.New("external cancellation")} {
		t.Run(cause.Error(), func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("CF-RAY", "local-HKG")
				w.Header().Set("Content-Length", "1000000000")
				_, _ = w.Write(bytes.Repeat([]byte("x"), 64*1024))
				w.(http.Flusher).Flush()
				// 先让客户端收到成功头和部分数据，再模拟外部取消。
				timer := time.AfterFunc(100*time.Millisecond, func() { cancel(cause) })
				defer timer.Stop()
				<-r.Context().Done()
			}))
			defer server.Close()
			ip, port := speedServerAddress(server)
			speed, dc, err := measureFastSpeed(ctx, ip, port, "http", ip, "/")
			if speed != 0 || dc != "HKG" || !errors.Is(err, cause) || errors.Is(err, errSpeedSampleComplete) {
				t.Fatalf("外部取消不能返回达标速度: %d, %q, %v", speed, dc, err)
			}
		})
	}
}

func TestMeasureFastSpeedAlreadyCanceled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("已取消的测速不应发出请求")
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ip, port := speedServerAddress(server)
	speed, _, err := measureFastSpeed(ctx, ip, port, "http", ip, "/")
	if speed != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("measureFastSpeed() = %d, %v; want canceled and zero speed", speed, err)
	}
}

func TestMeasureFastSpeedSampleComplete(t *testing.T) {
	server, headersAt := newSpeedSamplingServer(t)
	ip, port := speedServerAddress(server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	speed, dc, err := measureFastSpeed(ctx, ip, port, "http", ip, "/")
	assertSpeedSampleDuration(t, headersAt)
	if err != nil || speed <= 0 || dc != "HKG" {
		t.Fatalf("五秒正常采样被误判失败: %d, %q, %v", speed, dc, err)
	}
	if ctx.Err() != nil {
		t.Fatalf("采样结束不应取消父上下文: %v", ctx.Err())
	}
}
