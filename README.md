# cfdata 3.0

本机跑一个小服务（HTTP + WebSocket），网页上扫描 Cloudflare Anycast IP、按数据中心汇总、逐个测速。
3.0 在原有全量扫描之外新增**快速优选（快速模式）**：抽样 → 筛延迟 → 逐个测速 → 达到目标带宽立即返回。

产物：`..\build\CF3.0.exe`（约 8 MB，单文件，页面已内嵌）

## 三份代码的关系

| 位置 | 角色 | 产物 |
| --- | --- | --- |
| `build3.0/`（本目录） | **当前主线**，模块 `cfdata3` | `..\build\CF3.0.exe` |
| `..\cfdata.go` + `..\index.html` | 原版（1.x），单文件、模块 `cfdata`，仅作历史参考，**不再维护** | `..\build\cfdata-windows-amd64.exe` |
| `..\android\go\` | 由本目录源码逐文件移植的安卓版（`package cfdata` + gomobile 导出） | `..\build\CFScan-Android-<versionName>.apk` |

安卓版与本目录的差异集中在进程入口和生命周期，扫描/快速优选/测速/协议逻辑完全同源；
详见文末 [Android 版](#android-版)。相对原版的改动见 [相对原版做了什么](#相对原版做了什么)。

## 两种模式

页面顶部在「快速模式 / 完整模式」之间切换，默认快速模式，选择记在 `localStorage`（`cfdata_mode`）。
切换不会中断已运行的任务：完整扫描仍在跑时切到快速模式，开始按钮会被任务锁挡住，日志提示先取消或等它跑完。

### 快速模式（3.0 新增）

一轮的流程（数字全部来自 `fast.go` 的常量）：

1. 从 IP 列表**随机抽 100 个子网**（`fastSampleSize`），每个子网生成 1 个 IP；
2. **50 并发**（`fastRTTConcurrency`）对每个 IP 做 **3 次探测，3 次全部成功才保留**，延迟取 3 次平均；
   数据中心不在筛选范围内的 IP 直接丢弃；
3. 按平均延迟升序**保留前 10 个**（`fastTopSpeedTargets`）进入测速；
4. 逐个测速，**一旦实测带宽 ≥ 目标带宽就立即返回这一个 IP 并结束**；
5. 10 个都没达标 → 记录本批最佳 IP（数据中心、延迟、实测速度、用时），**换一批继续**，直到达标或点停止；
6. 批次历史最多保留最近 **20 批**（`fastHistoryLimit`），页面按新→旧展示，每次点开始会清空上一轮的历史和结果。

其他默认值：目标带宽 **50 Mbps**（1–10000，`fastMaxTargetMbps` 兜底）、IPv4、443 端口、
测速 URL `speed.cloudflare.com/__down?bytes=10000000`；数据中心默认**全部**，可按大区/国家分组多选。

带宽换算：1 Mbps = 128 kB/s，所以达标线 = `targetMbps * 128` kB/s，回显的 Mbps = `kB/s ÷ 128`。

### 完整模式

原版流程：全量扫描（可暂停/继续/取消）→ 数据中心汇总 → 点「查看 IP」列出该数据中心全部 IP（按延迟排序）
→ 单 IP 测速，或「一键测速」自动跑延迟最低的 3 个 IP。

## 编译

双击 **`build.bat`**，产物固定写到 `..\build\CF3.0.exe`。脚本行为：

- 用本机 Go（`D:\ruanjian\Go\bin\go.exe`，找不到时回退 `PATH` 里的 `go`）；
- `GOCACHE` → 本目录 `.gocache`，`GOMODCACHE` → `..\.gomodcache`；
- `GOPROXY=off`、`GOSUMDB=off`、`GOTOOLCHAIN=local`、`CGO_ENABLED=0`、`GOOS=windows GOARCH=amd64`，全程离线
  （`go.mod` 里用 `replace` 指向 `../.gomodcache/github.com/gorilla/websocket@v1.5.3`）；
- 编译前用 PowerShell 试探输出文件是否被占用，exe 还在运行时直接提示，不会报 `Access is denied`；
- `go build -trimpath -ldflags "-s -w"`。

命令行等价写法：

```powershell
$src = 'D:\app\Cloudflare\cfdata-web'
cd "$src\build3.0"
$env:GOCACHE="$src\build3.0\.gocache"; $env:GOMODCACHE="$src\.gomodcache"
$env:GOTOOLCHAIN='local'; $env:GOPROXY='off'; $env:GOSUMDB='off'; $env:CGO_ENABLED='0'
D:\ruanjian\Go\bin\go.exe build -trimpath -ldflags "-s -w" -o "$src\build\CF3.0.exe" .
```

> `index.html` 通过 `//go:embed` 嵌进 exe，**改完页面必须重新编译并重启 exe** 才看得到变化。

## 运行参数

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-port` | 8080 | 网页服务端口（1–65535，非法值直接退出码 2） |
| `-host` | `127.0.0.1` | 监听地址；局域网访问设 `0.0.0.0` 或网卡 IP，启动时会打印无鉴权提醒 |
| `-data-dir` | 空（当前目录） | `locations.json`、`ips-v4.txt`、`ips-v6.txt` 所在目录 |
| `-open-browser` | true | 监听就绪后自动打开默认浏览器（延迟 900 ms，避免打开过早报「无法访问」） |
| `-auto-exit` | true | 所有网页关闭后自动退出程序 |
| `-idle-timeout` | 10s | 最后一次页面活动后多久退出，最小 3s |

环境变量优先级高于参数：`CFD_NO_BROWSER=1` 关闭自动开浏览器，`CFD_NO_AUTO_EXIT=1` 关闭自动退出
（空/`0`/`false`/`no`/`off` 视为关闭，见 `envflag.go`）。

## 页面功能

- **快速优选卡片**：进度条 + 状态行（`第 N 轮 · RTT 测试 x/y`、`第 N 轮 · 测速 x/y · 当前IP`）、
  达标 IP 卡片（IP/数据中心/延迟/实测速度/目标带宽/用时，带轮次徽章）、每批最快 IP 表格。
- **数据中心筛选弹窗**：`get_dc_options` 首次打开时拉全量列表，按大区（亚洲/欧洲/美洲/非洲/大洋洲/其他）
  → 国家/地区 → 数据中心三级折叠展示，支持与搜索（可命中城市、IATA、国家中文名）、组级三态勾选、
  全选/清空；全选用 `[]`（= 不筛选）保存。任务运行中不允许改筛选。
- **完整扫描**：并发数（默认 100，超出 1–500 会被静默夹取并回写输入框）、13 个常用端口下拉、
  扫描结果表（延迟 <100 ms 绿 / <200 ms 黄 / 其余红，按扫描顺序最新在上、最多 500 条）、
  数据中心汇总表（IP 数量 + 最低延迟，按延迟排序）。
  扫描进行中点「查看 IP」会被拒绝并提示先暂停或等扫描结束。
- **测速**：扫描表/详细表都可直接点 IP 单元格复制（`navigator.clipboard`，非安全上下文回退 `execCommand`）。
  详细测试表每行有「测速」按钮，出结果后变「重测」；「一键测速」串行跑延迟最低的 3 个 IP（每 25 s 无回执算超时）。
- **运行日志**：最多 500 行，可折叠（折叠状态持久化），出现「失败/错误/超时/断开」等关键词时自动展开。
  所有校验提示、服务端 notice 都进日志；toast 只用于复制成功、主题切换、一键测速起止。
  启动、断线重连、切换网络或手动重测 IPv6 时会顺带请求 `myip.ipip.net`，
  在日志中输出当前 IPv4 出口与运营商（如 `当前 IP：120.10.223.236  来自于：中国 河北 沧州 联通`）。
- **主题**：自动 → 日间 → 夜间 三态循环，「自动」跟随系统 `prefers-color-scheme`。
- **恢复默认设置**：清空测速 URL 设置并发 `reset_resources`，删除三个资源文件后**立即重新下载**；
  每个文件失败自动重试一次，仍失败则写回内置备份，进度和失败原因会显示在进度条与日志中。
- **关闭程序**：发 `shutdown` 让服务端退出；服务端回 `shutting_down` 后页面显示「可以关掉此页面」。
- **设置持久化**（localStorage）：`cfdata_mode`、`cfdata_quick_target`、`cfdata_quick_ipv4`、
  `cfdata_quick_port`、`cfdata_quick_dc_filter`、`cfdata_speed_url`（两种模式共用）、
  `cfdata_log_open`、`cfdata_theme_mode`。任务状态**不存在前端**，刷新后由 `state_sync` 从服务端恢复。
- **断线重连**：指数退避 1s → 2s → 4s → 8s 封顶；重连成功后立即取快照，按钮状态跟着 `taskRunning/taskKind` 重算。

## 运行条件与数据文件

**启动时**检查三份资源文件：缺失或无效的先联网下载，完成后再监听端口并打开浏览器；
下载失败会自动重试一次，仍失败则写回内置备份（`resources/`，已嵌入 exe），程序照常启动。
运行扫描时优先使用本地文件：

| 文件 | 来源 |
| --- | --- |
| `locations.json` | `https://www.baipiao.eu.org/cloudflare/locations` |
| `ips-v4.txt` | `https://www.baipiao.eu.org/cloudflare/ips-v4` |
| `ips-v6.txt` | `https://www.baipiao.eu.org/cloudflare/ips-v6` |

- 下载校验：HTTP 200、响应非空、上限 20 MB、30 s 超时；**先校验再原子写入**（临时文件 + fsync + rename），
  中途失败不会留下半截文件。
- 启动检查和重置每份文件最多尝试 2 次（间隔 1 s）；仍失败时写回 `resources/` 里的内置备份。
- 本地文件损坏（如出现非法 CIDR）会打日志并自动重新下载，不会静默用坏数据。
- `parseSubnets` 只接受 **IPv4 `/24`** 与 **IPv6 `/48`**，自动规范化 + 去重，报错带行号。
- 随机 IP：IPv4 固定前 3 字节、末字节 0–255 随机；IPv6 固定前 6 字节、后 10 字节随机。
- IPv6 需要当前网络真的有 IPv6 出口，否则 RTT 阶段全军覆没（表现为一直换批）。

## 探测与测速口径

- **端口决定协议**：443/2053/2083/2087/2096/8443 走 HTTPS（TLS + SNI），其余走明文 HTTP。
  扫描、快速优选的 RTT、测速用同一个端口，因此延迟和速度是针对同一端口测出来的。
- **一次探测 = 一次 TCP 握手 + 一次 HTTP(S) 请求**，必须从响应头拿到 `CF-RAY` 才算有效 IP；
  延迟只取 TCP 握手耗时。TCP 连接超时 1 s，TLS/HTTP 阶段超时 3 s，避免高延迟 IP 被总超时误杀。
- 完整扫描按标准 TLS 校验；快速优选的 RTT 探测跳过证书校验（只是拿 `CF-RAY` 判数据中心）。
- **取消/停止是即时的**：探测用 `context`，取消时立刻关闭连接，不会卡在 `ReadResponse` 上等超时。
- **测速统计**：只接受 HTTP 2xx 响应；非 2xx 或空响应会作为错误显示，不会算出速度。
  500 ms 窗口的峰值速率与整体平均**取大**，小文件几百毫秒下完也能得到非 0 结果；
  下载 5 s 后主动关闭响应体，避免慢连接把统计拖死。请求强制从 `ip:port` 出站、禁用 keep-alive，
  客户端整体 20 s 超时。

## 消息协议

客户端 → 服务端（单条消息上限 1 MB）：

| type | data | 说明 |
| --- | --- | --- |
| `start_task` | `ipType, threads, port, speedUrl` | 开始完整扫描；端口同时用于探测和测速 |
| `start_fast_task` | `ipType, port, speedUrl, targetMbps, dcFilter` | 开始快速优选；`dcFilter` 为空数组表示全部数据中心 |
| `stop_fast_task` | `{}` | 停止快速优选 |
| `get_dc_options` | `{}` | 取数据中心筛选列表 |
| `pause_task` / `resume_task` | `{}` | 暂停 / 继续扫描 |
| `stop_scan_task` | `{}` | 取消扫描，保留已发现的 IP |
| `reset_resources` | `{}` | 删除资源文件并立即重新下载（失败重试后回退内置备份） |
| `start_speed_test` | `ip, port, speedUrl` | 单 IP 测速 |
| `shutdown` | `{}` | 关闭程序（`heartbeat` 仍兼容，已不需要） |

服务端 → 客户端：

| type | 说明 |
| --- | --- |
| `state_sync` | 完整快照：任务状态、扫描进度与全部结果、dcList、快速模式快照（轮次/阶段/历史/结果） |
| `scan_state` | 扫描 running/paused 确认，前端据此更新按钮 |
| `scan_progress` | 扫描进度；准备阶段用 `{phase:"..."}` 文案，扫描阶段用 `{current,total,matched}`（`matched` 为已命中的有效 IP 数） |
| `scan_result` | 单条 IP 结果 |
| `scan_summary` | 暂停时的全量汇总（含 `results` 与 `dcList`） |
| `scan_finished` | 扫描结束（含 `canceled` 标记） |
| `fast_progress` | 快速模式轮次 / 阶段（`rtt`、`speed`）/ 当前进度 / 当前 IP |
| `fast_batch` | 某一批的最佳 IP |
| `fast_result` | 达标 IP 结果 |
| `fast_finished` | 快速模式结束（`reason` + `hasResult`） |
| `dc_options` | 数据中心筛选列表（code/city/region/cca2，按 code 排序） |
| `reset_progress` | 资源重置进度（`current` / `total` / `text`） |
| `reset_resources_done` | 资源重置完成（`deleted` / `downloaded` / `fallback` / `failed` / `message`） |
| `speed_test_result` | 单 IP 测速结果（`ip` + `speed`，失败时 `speed` 是错误文案） |
| `log` / `notice` / `error` | 日志 / 提示 / 错误 |
| `shutting_down` | 服务端即将退出 |

所有输入都在服务端复核（IP 类型、端口、并发 1–500、目标带宽 ≤10000、测速 URL 能否解析、IP 字面量），
不信任前端；任一条校验失败只回 `notice`，不会启动任务。

## 任务与连接模型

- 全局任务锁：扫描 / 快速优选 / 资源重置三者互斥，`taskKind` 记录当前是哪种。
  新任务必须等旧任务释放锁，`fast_finished` 会**先广播再释放锁**，避免新任务抢在结束消息之前启动。
- 扫描支持暂停（条件变量挂住派发循环）、继续、取消；在途请求会跑完，全部落地后再补一份 `scan_summary`，
  避免暂停瞬间看到的 dcList 漏掉刚返回的 IP。
- 结果不绑定某个 WebSocket 连接，进度/日志/结果广播给所有页面；多开页面看到同一份状态。
- 每个连接独立写锁 + 单次写入 5 s 超时，慢客户端卡不住广播和其他页面。
- 存活检测是**服务端 ping / 浏览器 pong**（5 s 一次），页面切后台被浏览器限流也不会误退出；
  读超时 = `-idle-timeout` + 两个 ping 周期。连接断开时保留「断开时刻」而不是删记录，
  所以刷新页面、短暂断网不会触发自动退出。
- 看门狗 2 s 一拍；即使关了自动退出也会跑，只负责清理过期记录，避免 map 泄漏。
- HTTP 侧：`/` 只服务根路径（`favicon.ico` 等返回 404），`ReadHeaderTimeout` 5 s、`IdleTimeout` 60 s。

## 安全

- 默认只监听 `127.0.0.1`。用 `-host` 开局域网时启动打印提醒（程序本身没有鉴权）。
- WebSocket 握手校验 `Origin` 与 `Host`：回环监听时两者都必须是回环地址（抵御 DNS rebinding）；
  局域网监听时只接受 IP 字面量或 `localhost`，且 `Origin` 必须与 `Host` 一致；
  没有 `Origin` 头的非浏览器客户端放行。
- 前端所有动态内容用 `createElement` + `textContent` 渲染，**没有 `innerHTML` 和内联 `onclick`**，
  IP、城市、地区等字段不构成 XSS 注入面。

## 相对原版做了什么

原版（`..\cfdata.go`）能跑，但有三类问题，3.0 全部处理掉了：

- **状态绑连接**：任务状态和结果只发给发起它的那一条 WebSocket，`CheckOrigin` 直接返回 `true`，
  刷新页面就看不到正在跑的任务、按钮点不动。→ 3.0 引入 `state_sync` 全量快照 + 广播 + Origin/Host 校验。
- **靠前端心跳决定退出**：页面 `setInterval` 心跳被浏览器后台限流，标签一切换程序就自己退了。
  → 3.0 改为服务端 ping/pong，并区分「断开」与「从未连接」。
- **不校验输入**：资源文件写坏就留半截、IP 列表不校验前缀、端口/并发/URL 前端说了算、
  前端用 `innerHTML` 拼 IP 与地区。→ 3.0 全部在服务端复核 + 原子写入 + CIDR 校验 + 去 innerHTML。
- **测速口径**：小文件几百毫秒下完时统计不出速度。→ 3.0 改成 500 ms 窗口峰值与整体平均取大。

顺带删掉的：原版的「IPv4 地址库精简」（`compact_ipv4`，多轮扫子网把无响应网段从文件里剔掉）。
3.0 的快速模式用「抽样 + 换批」达到同样目的，且不会改写资源文件。
原版的详细测试阶段（`start_test` + `delay` 参数重测一遍延迟）也没保留——
选数据中心后直接复用扫描阶段记录的 IP 与延迟，不再重复跑一轮。

## 文件说明

| 文件 | 说明 |
| --- | --- |
| `cfdata.go` | 入口与参数、HTTP/WS、任务状态机、扫描、测速、资源下载与校验、Origin 校验 |
| `resources.go` / `resources/` | 三份资源的内置备份；启动检查、重置下载、校验、重试与回退逻辑 |
| `fast.go` | 快速模式：抽样、3 次 RTT 筛选、逐个测速与早停、批次历史、DC 过滤 |
| `ipinfo.go` | 启动 / 切网时经 `myip.ipip.net` 查询当前 IPv4 出口与运营商并写入日志（固定 IPv4 出站、30 s 缓存、结果去重） |
| `lifecycle.go` | 页面跟踪、服务端 ping/pong、读超时、自动退出看门狗、退出通知 |
| `browser.go` / `browser_windows.go` / `browser_other.go` | 自动打开默认浏览器（Windows 优先读注册表拿默认浏览器命令，再退到 `rundll32 url.dll` / `explorer`） |
| `procsys_windows.go` / `procsys_other.go` | 子进程隐藏控制台窗口 |
| `envflag.go` | 环境变量开关解析 |
| `index.html` | 页面（`//go:embed` 进 exe） |
| `go.mod` / `go.sum` | 模块 `cfdata3`，唯一依赖 gorilla/websocket v1.5.3，`replace` 指向本地缓存 |
| `cfdata_test.go` | CIDR 校验与报错行号、随机 IP 落在段内、端口→协议映射、测速 URL 取 Host、Origin/Host 校验、原子写入、下载大小限制、dcList 排序 |
| `fast_test.go` | DC 过滤归一化、抽样数量与去重、`CF-RAY` 解析、测速目标拆解 |
| `browser_windows_test.go` | 注册表浏览器命令解析与 UserChoice 读取（仅 Windows） |
| `build.bat` | 一键编译到 `..\build\CF3.0.exe`（保持纯 ASCII，cmd 按 GBK 读 .bat） |
| `check_html.go` | 一次性页面结构检查，`go run check_html.go index.html`（带 `//go:build ignore`） |
| `check_js.mjs` | 一次性脚本语法与协议断言，`node check_js.mjs index.html` |
| `wsclient.go` | 一次性 WebSocket 测试客户端，`go run wsclient.go <url> heartbeat|nobeat|shutdown|resume|trace <秒数>` |

## Android 版

源码在 `..\android\`，`go/` 下每个文件都从本目录对应文件移植，扫描与协议逻辑同源：

| 项目 | 值 |
| --- | --- |
| Go 模块 | `cfdatamobile`，`package cfdata`，导出 `Start(baseDir) (string, error)` / `Stop()` |
| 打包 | `gomobile bind -target=android/arm64 -androidapi 26 -javapkg=com.cfdata.mobile` → `app/libs/cfdata.aar` |
| 壳 | `MainActivity.java`：起服务拿 URL → WebView 加载（`localhost` HTTP，`usesCleartextTraffic`） |
| 端口 | 固定 `127.0.0.1:18080`；被占用时退回随机端口并打日志 |
| 资源目录 | `getFilesDir()/cfdata`，内置 `resources/{locations.json,ips-v4.txt,ips-v6.txt}` 首启直接写入，首屏不依赖联网下载；重置时先联网更新，失败回退内置备份 |
| 生命周期 | 不做「页面全关就退出」：无空闲看门狗、不设读超时；`shutdown` 只停本地服务。Android 12+ 按返回/Home 会把任务移到后台，真正结束进程是上滑移除任务，页面隐藏「关闭程序」按钮 |
| 主题 | `CFScanNative` JS 桥（`isSystemDark` / `setPageDark`）同步状态栏与背景色，系统深浅色变化回调 `CFScanSystemThemeChanged` |
| 版本 | 唯一来源是 `app\build.gradle` 的 `versionName` / `versionCode`（当前 `3.5` / 10）；`build-apk.bat` 启动时读出来拼成输出文件名，改版本只需要改 build.gradle |
| 兼容 | minSdk 29、targetSdk 36、compileSdk 36，仅打包 `arm64-v8a`；debug 与 release 都用 debug 签名（个人侧载，不配 keystore） |

为什么端口固定：WebView 的 localStorage 按 origin（含端口）隔离，端口每次变化会让目标带宽、
测速 URL 等设置记不住。

一键打包（工具链在 `D:\Android`：JDK 21 / SDK 36 / NDK 28.2 / Gradle 8.13，需要联网拉依赖）：

```bat
D:\app\Cloudflare\cfdata-web\android\build-apk.bat
adb install -r D:\app\Cloudflare\cfdata-web\build\CFScan-Android-3.5.apk
```

改 `android/go/index.html` 后同样要重新出 APK（页面是 embed 进 aar 的）。

## 自测

```powershell
# Go 单元测试 + 静态检查
D:\ruanjian\Go\bin\go.exe vet ./...
D:\ruanjian\Go\bin\go.exe test ./...

# 页面结构与前端脚本（需要 Node）
go run check_html.go index.html
node check_js.mjs index.html
```

`cfdata_test.go` / `fast_test.go` 覆盖纯函数（解析、校验、排序、快照），不联网、不真实探测 IP。
