// 一次性校验脚本：抽取 index.html 里的脚本做语法检查。
// 运行：node check_js.mjs index.html
import { readFileSync } from 'node:fs';

const file = process.argv[2];
if (!file) {
  console.error('用法: node check_js.mjs index.html');
  process.exit(2);
}
const html = readFileSync(file, 'utf8');

const blocks = [...html.matchAll(/<script[^>]*>([\s\S]*?)<\/script>/gi)];
if (blocks.length === 0) {
  console.log('没有找到 script 块');
  process.exit(1);
}

let failed = false;
blocks.forEach((m, i) => {
  const code = m[1];
  try {
    // 只编译不执行，能抓出语法错误
    new Function(code);
    console.log(`script 块 #${i + 1}: 语法正确（${code.length} 字符）`);
  } catch (e) {
    failed = true;
    console.log(`script 块 #${i + 1}: 语法错误 -> ${e.message}`);
  }
});

const js = blocks.map((m) => m[1]).join('\n');

// 关键逻辑断言（build2.0 协议）
const checks = [
  ['主题三档：自动/日间/夜间', ((/id="themeAuto"/.test(html) && /data-theme="auto"/.test(html)) || (/id="themeCycleBtn"/.test(html) && /data-theme-mode="auto"/.test(html) && /function\s+cycleThemeMode/.test(js))) && /function\s+setThemeMode/.test(js)],
  ['默认自动模式', /let mode = 'auto'/.test(js)],
  ['自动模式跟随系统', /prefers-color-scheme: dark/.test(js) && /addEventListener\('change'/.test(js)],
  ['不再由前端发心跳', !/type:\s*'heartbeat'/.test(js) && !/startHeartbeat/.test(js)],
  ['接收完整状态同步', /case 'state_sync'/.test(js) && /function\s+applyStateSync/.test(js)],
  ['扫描状态由服务端确认', /case 'scan_state'/.test(js) && /function\s+applyScanState/.test(js)],
  ['扫描汇总与完成分开处理', /case 'scan_summary'/.test(js) && /case 'scan_finished'/.test(js)],
  ['暂停不做本地乐观切换', /pausePending/.test(js) && /sendWS\('pause_task'\)/.test(js)],
  ['发送前检查连接', /function\s+sendWS/.test(js) && /readyState !== WebSocket\.OPEN/.test(js)],
  ['重连退避上限', /RECONNECT_MAX/.test(js) && /Math\.min\(reconnectDelay \* 2, RECONNECT_MAX\)/.test(js)],
  ['日志有上限', /MAX_LOG_LINES/.test(js) && /childElementCount > MAX_LOG_LINES/.test(js)],
  ['扫描表格有上限', /MAX_SCAN_ROWS/.test(js) && /scanData\.length - MAX_SCAN_ROWS/.test(js)],
  ['动态行用 DOM + textContent 渲染', /function\s+buildScanRow/.test(js) && /textContent/.test(js)],
  ['不再使用 innerHTML 渲染', !/\.innerHTML\s*=/.test(js)],
  ['不再使用内联事件', !/\bonclick=/.test(html)],
  ['选择数据中心直接使用扫描结果', /function\s+selectDC/.test(js) && /function\s+rowsForDC/.test(js) && /LatencyMs/.test(js) && !/sendWS\('start_test'/.test(js)],
  ['扫描与测速共用端口', /扫描\/测速端口/.test(html) && /port: port/.test(js)],
  ['详细测试表使用扫描延迟', /延迟来自扫描阶段/.test(html) && /<th>延迟<\/th>/.test(html)],
  ['测速结果单独一列', /<th>速度<\/th>/.test(html) && /speed-result-cell/.test(html) && /speed-result-cell/.test(js)],
  ['一键测速按钮', /id="btnAutoSpeed"/.test(html) && /function\s+handleAutoSpeedClick/.test(js)],
  ['详细测试标题显示当前数据中心', /id="testSectionTitle"/.test(html) && /testTitle\.textContent = selectedDC \? selectedDC \+ ' 数据中心'/.test(js)],
  ['一键测速与开始扫描同蓝', /#btnAutoSpeed[\s\S]*?linear-gradient\(135deg, var\(--primary-color\), var\(--primary-hover\)\)/.test(html)],
  ['一键测速最多 3 个且串行', /AUTO_SPEED_COUNT\s*=\s*3/.test(js) && /function\s+startNextAutoStep/.test(js)],
  ['默认快速模式', /class="mode-pill active"[^>]*id="modeQuick"/.test(html) && /let mode = 'quick'/.test(js)],
  ['快速/完整模式切换', /id="modeQuick"/.test(html) && /id="modeFull"/.test(html) && /function\s+setMode/.test(js)],
  ['快速模式默认目标 50', /id="qTargetMbps"[^>]*value="50"/.test(html)],
  ['快速模式每批历史', /id="qHistoryTable"/.test(html) && /fast_batch/.test(js) && /FAST_HISTORY_LIMIT/.test(js)],
  ['快速模式 DC 多选筛选', /id="dcFilterModal"/.test(html) && /get_dc_options/.test(js) && /dcFilter: dcFilter\.slice\(\)/.test(js)],
  ['快速模式按大区/国家筛选', /DC_REGION_GROUPS/.test(js) && /dc-group-header/.test(js) && /countryLabel/.test(js)],
  ['复选框不继承文本框尺寸与聚焦阴影', !/select(?::focus)?,\s*input(?::focus)?\s*\{/.test(html) && /input:not\(\[type="checkbox"\]\):focus/.test(html)],
  ['筛选说明保持国家级选择', /勾选国家\/地区即选择其全部数据中心/.test(html) && !/具体数据中心可选择/.test(html)],
  ['快速模式可停止', /sendWS\('stop_fast_task'\)/.test(js)],
  ['快速模式状态恢复', /case 'fast_progress'/.test(js) && /case 'fast_finished'/.test(js) && /applyFastSnapshot/.test(js)],
  ['圆形开始/重置/关闭按钮', /id="qStart"[^>]*round-btn/.test(html) && /id="btnStart"[^>]*round-btn/.test(html) && /id="btnRestoreDefaults"[^>]*round-btn/.test(html) && /id="btnShutdown"[^>]*header-close/.test(html)],
  ['精简 IPv4 功能已移除', !/btnCompactIPv4/.test(html) && !/compact_ipv4/.test(js)],
  ['完整模式介绍', /完整扫描/.test(html)],
  ['测速结果可重测', /speedResults\.has\(ip\) \? '重测' : '测速'/.test(js)],
  ['关闭按钮', /id="btnShutdown"/.test(html) && /function\s+shutdownApp/.test(js)],
  ['发送关闭指令', /sendWS\('shutdown'\)/.test(js)],
  ['收到关闭通知后停止重连', /case 'shutting_down'/.test(js) && /if \(serverClosed\) return;/.test(js)],
  ['尝试自动关闭页面', /window\.close\(\)/.test(js)],
  ['确认框保留换行', /white-space:\s*pre-line/.test(html)],
];

let bad = 0;
for (const [name, ok] of checks) {
  if (!ok) bad++;
  console.log(`${ok ? '通过' : '未通过'}  ${name}`);
}

// 待办标记不该留在代码里
const leftover = js.match(/TODO|FIXME|XXX/g);
if (leftover) console.log('注意：脚本里还有待办标记', leftover);

console.log(bad === 0 && !failed ? '全部检查通过' : `有 ${bad} 项未通过`);
process.exit(bad === 0 && !failed ? 0 : 1);
