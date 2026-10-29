# topo-scale-mock 规模预演发生器

拓扑页的 mock 数据发生器。它**不进控制器、不写库、不改页面代码**：注入到已登录的标签页内存里，
只劫持 `window.fetch` 对 `/api/underlay/topology` 这一个路径的响应，其它请求照走真实控制器。
用途是量"上千台 CPE 时这版 SVG 画法撑不撑得住"——实测数字与结论在
[`docs/topology-map-plan.md` 第 10 节](../../../docs/topology-map-plan.md)。

留进仓库的原因很直白：那节里的数字如果只存在于一次对话里，改完渲染就没法复跑同一套量测，
"裁剪之后从 202 ms 掉到多少"就只能靠说。

## 怎么注入

浏览器控制台粘贴整个文件（它是个箭头函数表达式，需要在最外面加括号或直接当 expression 求值），
或者用 MCP `evaluate_script` 把文件内容当脚本丢进去。返回 `"generator installed: 52 cities"` 才算装上。

```js
window.__mock118(1000)   // 造 1000 台 + 覆盖 fetch，返回自检结果
```

装完之后**在页面上点进「监控与管理 › 拓扑图」**，页面自己就会去取这份 mock 文档。
换规模直接再调一次 `__mock118(300)`。想回到真实数据：刷新页面（fetch 覆盖只活在内存里）。

## 自检输出必须全对上

`__mock118` 返回里 `check` 那三项是这套量测的可信度来源：

```
{ nodes, bundles, sites, unplaced, jsonKB, genMs, parseMs,
  check: { placed, sumSiteDevices, danglingSiteRefs, unplacedMatches } }
```

`placed == sumSiteDevices`、`danglingSiteRefs == 0`、`unplacedMatches == true`。
第一版发生器这里就是错的（同城第二台之后的设备拿不到 `site_id`），于是 300 台里虚报 101 台未定位、
站点台数虚高——量到的会是 mock 的 bug 而不是页面的性能。加自检之后重跑，第 10 节的数字全部来自修完之后。

## 种子与形态

种子固定（mulberry32 `20261029`），同一 `N` 每次生成完全一样的文档，所以两次量测可以直接比。
52 个真实城市坐标、站内 jitter ±0.01°，形态按 1000 台那份文档实测比例：online 906 / stale 45 /
pending 49、未定位 48、symmetric_nat 80、站点 454 个单台 + 57 个两台 + 42 个三台 + 44 个四台以上、
1,424 组隧道里 183 组走中转（12.9%）、8 组路径模板失效、239 组 `state≠up`。
连接形态是 hub-spoke 全量 + 约三成挂第二个 hub + N/6 条随机 spoke-spoke。

## 换了拓扑接口之后

`__gen118` 产出的字段名是照 `consoleUnderlayTopologyView`（`underlay_view.go`）手抄的：
`nodes[]` / `bundles[]` / `sites[]` / `unplaced_nodes`，节点上带 `site_id`、`has_location`、
`tunnel_delivery`、`ifaces` 等。**接口加字段这里不会报错，只会让页面上对应的东西安静地消失**
（渲染读的是 `undefined`）。所以动过 `/api/underlay/topology` 的形状，先在这儿补一个字段，
再回拓扑页确认它真的画出来了。

## 量帧延迟的姿势（第 10 节用的就是这套）

- 帧延迟 = 同步 `svg.dispatchEvent(new WheelEvent('wheel', {deltaY…}))` 前后的
  `performance.now()` 差，覆盖 `undMapZoomTo → undMapDraw` 一次完整重绘。滚轮与拖动走同一个函数。
- 首屏 = `document.querySelector('#nav a[data-view="underlay-topology"]').click()` 这一次同步调用的耗时。
- 每档数字从页面自身读回（`#tp-zoom`、`#tp-level`、`.usage`、`querySelectorAll` 计数），不推算。
- **这些都是下界**：这个 webview `visibilityState=hidden` 拿不到视口，量不到浏览器 paint。
  要看绝对手感，得把面板切到前台用真实屏幕量；要比较两个改动的优劣，用这里的相对斜率就够。
