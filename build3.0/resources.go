package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// 资源文件的内置备份：联网下载失败时写回磁盘，保证程序仍可使用。
// 这三份文件与 android/go/resources 保持一致，更新资源时两边都要同步。
//
//go:embed resources/locations.json
var builtinLocations []byte

//go:embed resources/ips-v4.txt
var builtinIPv4 []byte

//go:embed resources/ips-v6.txt
var builtinIPv6 []byte

const (
	resourceDownloadAttempts = 2
	resourceRetryDelay       = time.Second
)

type resourceSpec struct {
	name     string
	url      string
	fallback []byte
	validate func([]byte) error
}

type resourceOutcome struct {
	Name   string
	Status string // local / downloaded / fallback / failed
	Err    error
}

func resourceSpecs() []resourceSpec {
	return []resourceSpec{
		{
			name:     "locations.json",
			url:      locationsURL,
			fallback: builtinLocations,
			validate: validateLocationsData,
		},
		{
			name:     "ips-v4.txt",
			url:      ipsV4URL,
			fallback: builtinIPv4,
			validate: func(body []byte) error {
				_, err := parseSubnets(string(body), 4)
				return err
			},
		},
		{
			name:     "ips-v6.txt",
			url:      ipsV6URL,
			fallback: builtinIPv6,
			validate: func(body []byte) error {
				_, err := parseSubnets(string(body), 6)
				return err
			},
		},
	}
}

func validateLocationsData(body []byte) error {
	var locations []location
	if err := json.Unmarshal(body, &locations); err != nil {
		return fmt.Errorf("解析位置信息失败: %w", err)
	}
	if len(locations) == 0 {
		return fmt.Errorf("位置信息为空")
	}
	return nil
}

func resourceFileValid(spec resourceSpec) bool {
	body, err := os.ReadFile(dataPath(spec.name))
	if err != nil {
		return false
	}
	return spec.validate(body) == nil
}

// ensureResourceFiles 检查三份资源文件。
// 非 force 模式下，本地文件有效就直接复用；缺失或无效时下载。
// force 模式用于“恢复默认”：无论本地文件是否存在都重新下载。
// 每个文件最多尝试 resourceDownloadAttempts 次，仍失败则写回内置备份。
func ensureResourceFiles(ctx context.Context, force bool, progress func(current, total int, text string)) []resourceOutcome {
	specs := resourceSpecs()
	outcomes := make([]resourceOutcome, 0, len(specs))

	for index, spec := range specs {
		outcome := resourceOutcome{Name: spec.name}
		filename := dataPath(spec.name)

		if !force && resourceFileValid(spec) {
			outcome.Status = "local"
			if progress != nil {
				progress(index+1, len(specs), spec.name+" 已存在")
			}
			outcomes = append(outcomes, outcome)
			continue
		}

		downloaded := false
		var lastErr error
		for attempt := 1; attempt <= resourceDownloadAttempts; attempt++ {
			if err := ctx.Err(); err != nil {
				lastErr = err
				break
			}
			if progress != nil {
				progress(index, len(specs), fmt.Sprintf(
					"正在下载 %s（第 %d/%d 次）...", spec.name, attempt, resourceDownloadAttempts))
			}

			body, err := fetchURL(ctx, spec.url)
			if err == nil {
				err = spec.validate(body)
			}
			if err == nil {
				err = saveToFileAtomic(filename, body)
			}
			if err == nil {
				downloaded = true
				lastErr = nil
				break
			}

			lastErr = err
			if attempt < resourceDownloadAttempts {
				if progress != nil {
					progress(index, len(specs), fmt.Sprintf(
						"%s 下载失败：%v，%d 秒后重试...", spec.name, err, int(resourceRetryDelay/time.Second)))
				}
				if !sleepWithContext(ctx, resourceRetryDelay) {
					break
				}
			}
		}

		if downloaded {
			outcome.Status = "downloaded"
			if progress != nil {
				progress(index+1, len(specs), spec.name+" 下载完成")
			}
			outcomes = append(outcomes, outcome)
			continue
		}

		if lastErr == nil {
			lastErr = fmt.Errorf("未知错误")
		}
		if err := saveToFileAtomic(filename, spec.fallback); err != nil {
			outcome.Status = "failed"
			outcome.Err = fmt.Errorf("下载失败（%v），写入内置备份也失败：%w", lastErr, err)
		} else {
			outcome.Status = "fallback"
			outcome.Err = lastErr
		}
		if progress != nil {
			if outcome.Status == "failed" {
				progress(index+1, len(specs), spec.name+" 处理失败："+outcome.Err.Error())
			} else {
				progress(index+1, len(specs), spec.name+" 下载失败，已使用内置备份")
			}
		}
		outcomes = append(outcomes, outcome)
	}

	return outcomes
}

func sleepWithContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// applyLocationsFromFile 只读取本地文件并应用，不再触发下载；供启动检查和重置完成时使用。
func applyLocationsFromFile() bool {
	filename := dataPath("locations.json")
	body, err := os.ReadFile(filename)
	if err != nil {
		logf("读取 %s 失败: %v", filename, err)
		return false
	}
	var locations []location
	if err := json.Unmarshal(body, &locations); err != nil {
		logf("解析 %s 失败: %v", filename, err)
		return false
	}
	if len(locations) == 0 {
		logf("%s 内容为空", filename)
		return false
	}
	applyLocations(locations)
	logf("已加载 %d 个数据中心位置信息", len(locationSnapshot()))
	return true
}

// prepareResourceFilesOnStartup 在监听端口和打开浏览器之前检查资源文件。
// 缺失或无效的文件会先尝试下载，仍失败则写回内置备份。
func prepareResourceFilesOnStartup(ctx context.Context) {
	logf("正在检查资源文件（locations.json、ips-v4.txt、ips-v6.txt）...")
	outcomes := ensureResourceFiles(ctx, false, func(_ int, _ int, text string) {
		logf("%s", text)
	})
	applyLocationsFromFile()

	var fallback, failed []string
	for _, outcome := range outcomes {
		switch outcome.Status {
		case "fallback":
			fallback = append(fallback, outcome.Name)
		case "failed":
			failed = append(failed, outcome.Name)
		}
	}
	if len(failed) > 0 {
		logf("警告: 资源文件处理失败：%s；请检查网络或目录权限", strings.Join(failed, "、"))
	} else if len(fallback) > 0 {
		logf("警告: %s 下载失败，已使用内置备份；可稍后重试", strings.Join(fallback, "、"))
	} else {
		logf("资源文件检查完成")
	}
}

func resourceOutcomeSummary(outcomes []resourceOutcome, deleteErrors []string) (message string, downloaded, fallback, failed []string) {
	for _, outcome := range outcomes {
		switch outcome.Status {
		case "downloaded":
			downloaded = append(downloaded, outcome.Name)
		case "fallback":
			fallback = append(fallback, outcome.Name)
		case "failed":
			failed = append(failed, outcome.Name)
		}
	}

	var parts []string
	switch {
	case len(failed) > 0:
		parts = append(parts, "资源重置未完成："+strings.Join(failed, "、")+" 处理失败")
	case len(fallback) > 0:
		parts = append(parts, "资源已重置，但 "+strings.Join(fallback, "、")+" 下载失败，已使用内置备份，建议稍后重试")
	default:
		parts = append(parts, "资源已重置，三份资源文件均已更新")
	}
	if len(deleteErrors) > 0 {
		parts = append(parts, "删除旧文件时出现问题："+strings.Join(deleteErrors, "；"))
	}
	return strings.Join(parts, "；"), downloaded, fallback, failed
}
