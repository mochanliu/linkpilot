package main

// 基础配置按模块拆页之后的接线测试。
//
// 拆页是"纯字符串"改动：导航项、CFG_MODULES 表、VIEWS 条目、render 分发、
// 以及每页自己的 view 函数分散在五个地方。少写一处的结果不是报错，而是
// "点了没反应"或者"点进去一片空白"——上一轮策略页的故障就是这么来的。
// 所以这里钉三件事：
//
//  1. 导航里的 8 个模块与 CFG_MODULES 一一对应、顺序一致（页内切换条和左侧
//     导航是同一组入口，顺序不同会让人以为自己点错了）；
//  2. 每个模块在 VIEWS、render 分发、view 函数三处都存在；
//  3. 服务配置不再挂在 Overlay 组下，且 NAT 与 服务配置共用同一份服务面
//     （同一个 S.sv、同一台当前设备）——拆成两个 doc 会把对方的行写掉。

import (
	"regexp"
	"strings"
	"testing"
)

var (
	navItemRe    = regexp.MustCompile(`data-view="([^"]+)"[^>]*>([^<]+)</a>`)
	cfgModuleRe  = regexp.MustCompile(`\["(config[^"]*)", "([^"]+)"\]`)
	cfgModulesJS = regexp.MustCompile(`(?s)var CFG_MODULES = \[(.*?)\];`)
)

// navSection 取左侧导航里某一组（.sect 标题到下一个 .sect 之间）的条目。
func navSection(t *testing.T, html, title string) [][]string {
	t.Helper()
	start := strings.Index(html, ">"+title+"</div>")
	if start < 0 {
		t.Fatalf("导航里没有「%s」这一组", title)
	}
	rest := html[start:]
	// 组到下一个分组标题或 nav 结束为止。
	end := len(rest)
	for _, marker := range []string{`<div class="sect"`, "</nav>"} {
		if i := strings.Index(rest[1:], marker); i >= 0 && i+1 < end {
			end = i + 1
		}
	}
	return navItemRe.FindAllStringSubmatch(rest[:end], -1)
}

func TestConsoleNavBasicsIsEightModules(t *testing.T) {
	html := string(consoleHTML)

	nav := navSection(t, html, "基础配置")
	if len(nav) != 8 {
		t.Fatalf("基础配置应有 8 个模块，实际 %d 个: %v", len(nav), nav)
	}

	m := cfgModulesJS.FindStringSubmatch(html)
	if m == nil {
		t.Fatal("console.html 里没有 CFG_MODULES 表")
	}
	mods := cfgModuleRe.FindAllStringSubmatch(m[1], -1)
	if len(mods) != len(nav) {
		t.Fatalf("CFG_MODULES 有 %d 项，导航有 %d 项", len(mods), len(nav))
	}

	// 顺序与文案都要一致：页内切换条是照着这张表画的。
	want := []string{"设备信息", "接口配置", "VRRP", "静态路由", "动态路由", "NAT", "QoS", "服务配置（DHCP / DNS）"}
	for i, item := range nav {
		view, label := item[1], strings.TrimSpace(item[2])
		if view != mods[i][1] {
			t.Errorf("第 %d 项导航指向 %q，CFG_MODULES 是 %q", i+1, view, mods[i][1])
		}
		if label != want[i] {
			t.Errorf("第 %d 项导航文案是 %q，期望 %q", i+1, label, want[i])
		}
		if !regexp.MustCompile(`(?m)^    "?` + regexp.QuoteMeta(view) + `"?: \{`).MatchString(html) {
			t.Errorf("%s 没有 VIEWS 条目：标题栏会是空的", view)
		}
		if !strings.Contains(html, `S.view === "`+view+`"`) {
			t.Errorf("%s 没有 render 分发：点进去会是空白页", view)
		}
	}
}

func TestConsoleServicesMovedOutOfOverlay(t *testing.T) {
	html := string(consoleHTML)

	overlay := navSection(t, html, "Overlay 配置")
	for _, item := range overlay {
		if strings.Contains(item[2], "服务配置") {
			t.Fatalf("服务配置仍挂在 Overlay 组下：%q", item[0])
		}
	}
	if strings.Contains(html, `data-view="services"`) || strings.Contains(html, "\n    services: {") {
		t.Error("旧的 services 视图还在：服务配置已拆成 config-nat 与 config-services 两页")
	}

	// NAT 页与 服务配置页必须走同一个外壳（svcShell）：它们编辑的是同一份
	// ServicesSet，服务端只有一次 PUT。
	nat := funcBody(t, html, "function viewConfigNAT(")
	if !strings.Contains(nat, `svcShell("config-nat")`) {
		t.Error("NAT 页没有走 svcShell：那会拿着一份自己的 doc，保存时把 DHCP/DNS 行写空")
	}
	svc := funcBody(t, html, "function viewConfigServices(")
	if !strings.Contains(svc, `svcShell("config-services")`) {
		t.Error("服务配置页没有走 svcShell")
	}
	// NAT 已按形状 G 正常下发（7daa66d，exp-nat-bare 裁决配置形状问题）：
	// 「暂不下发」机制与说明一并删除，页面不得再引用这个死字段——它来自
	// servicesNatBlocked 时代，留着会把用户引向错误结论。
	if strings.Contains(nat, "nat_pending") || strings.Contains(nat, "暂不下发") {
		t.Error("NAT 页还在显示「暂不下发」：该机制已随形状 G 下发落地而删除（7daa66d）")
	}
}

// 监控页趋势图的设备选择必须是搜索选择器，而不是裸下拉：设备一多，
// 下拉里翻不到要看的设备；且 5s 轮询会在下拉展开时整页重绘，把没点完的
// 选择整个拆掉（owner 实测"换设备图不变"的体感就是这么来的——另一并因素
// 是图上不写当前设备是谁，几台盒子曲线几乎一样）。冻结护栏与 mousedown
// 选中是这条体验的两半，缺一半都会退化回旧问题。
func TestConsoleMonitorHistPicker(t *testing.T) {
	html := string(consoleHTML)
	mon := funcBody(t, html, "function viewMonitor(")
	if !strings.Contains(mon, `id="hist-q"`) {
		t.Error("监控页没有设备搜索选择器（hist-q）")
	}
	if strings.Contains(mon, `id="hist-node"`) {
		t.Error("监控页还在用旧的裸下拉 hist-node")
	}
	if !strings.Contains(mon, "histCharts(S.history, histSel, nodes, S.histT || \"\")") {
		t.Error("趋势图没有把设备列表和隧道选择传给 histCharts：图例反查不出对端序列号，隧道也筛不掉")
	}
	if !strings.Contains(mon, `id="hist-tq"`) || !strings.Contains(mon, "hist-tpick") {
		t.Error("监控页没有隧道搜索选择器（hist-tq / hist-tpick）：单设备上千条隧道时图没法看")
	}
	r := funcBody(t, html, "function render(")
	if !strings.Contains(r, "S.freeze") {
		t.Error("render() 没有冻结护栏：选择器展开时会被 5s 轮询整页重绘拆掉")
	}
	bd := funcBody(t, html, "function bind(")
	if !strings.Contains(bd, "histPickList()") || !strings.Contains(bd, "data-hist-node") {
		t.Error("bind() 没有接设备选择器事件：搜索与选中都不会有反应")
	}
	if !strings.Contains(bd, "histTPickList()") || !strings.Contains(bd, "data-hist-tunnel") {
		t.Error("bind() 没有接隧道选择器事件：选隧道不会有反应")
	}
	if !strings.Contains(bd, "S.histT = \"\"") {
		t.Error("bind() 换设备时没有清掉隧道选择：选中的键多半不在新设备上")
	}
	if !strings.Contains(bd, `histHold("hist-pick")`) || !strings.Contains(bd, `addEventListener("mousedown"`) {
		t.Error("bind() 没有在 mousedown 就压冻结：5s 重绘落在 mousedown 和 focus 之间会吃掉点击")
	}
	// render 的焦点护栏（焦点在 #content 输入控件上整帧跳过）会吞掉选中后
	// 的那一帧重绘：条目 mousedown 的 preventDefault 让输入框一直带着焦点，
	// 状态变了画面不动，要点空白处失焦后才生效。两个选中回调必须先 blur。
	if !strings.Contains(bd, "histBlurForRender") || strings.Count(bd, "histBlurForRender();") < 2 {
		t.Error("bind() 选中回调没有先 blur 再 render：焦点护栏会吞掉选中那一帧，点了条目要等下一拍轮询才生效")
	}
	if !strings.Contains(r, "S.freezeAt") {
		t.Error("render() 没有 freezeAt 过期安全阀：blur 丢了冻结会卡死整页")
	}
	lh := funcBody(t, html, "function loadHistory(")
	if !strings.Contains(lh, "S.metrics.nodes") || !strings.Contains(lh, `"&node="`) {
		t.Error("loadHistory 没有按台拉取：默认全量响应 18MB，WAN 上卡住交互")
	}
	if !strings.Contains(html, "S.histLoading") || !strings.Contains(html, "正在拉取过去 24 小时的采样") {
		t.Error("没有拉取中提示：换设备后空图看起来像点了没反应")
	}
}

// 基础配置组共用一台当前设备：服务面必须跟随 S.cfg，否则在 NAT 页换设备之后，
// 接口配置页显示的还是上一台。
func TestConsoleServicesFollowSelectedDevice(t *testing.T) {
	html := string(consoleHTML)
	body := funcBody(t, html, "function ensureServices(")
	if !strings.Contains(body, "svFollowCfg()") {
		t.Fatal("ensureServices 没有跟随 S.cfg 的当前设备")
	}
	follow := funcBody(t, html, "function svFollowCfg(")
	if !strings.Contains(follow, "cs.nodeID") {
		t.Error("svFollowCfg 没有用 S.cfg 的 nodeID")
	}
	if !strings.Contains(follow, "o.doc = null") {
		t.Error("换了设备却没有丢掉上一台的 doc")
	}
	// 反向：在 NAT 页换设备要连那 5 页的草稿一起换。
	change := funcBody(t, html, "function svChange(")
	if !strings.Contains(change, "selectCfgTarget(") {
		t.Error("服务页换设备没有同步到基础配置草稿（selectCfgTarget）")
	}
}

// 每个模块只画自己那一段：拆页之后又把面板塞回设备信息页，等于拆了个寂寞。
func TestConsoleModulePagesOwnTheirPanels(t *testing.T) {
	html := string(consoleHTML)
	cases := []struct {
		fn     string
		panels []string
	}{
		{"function viewConfig(", []string{"cfgDevicePanel(", "cfgDeliveryPanel(", "cfgChecksPanel(", "cfgArtifactsPanel("}},
		{"function viewConfigIfaces(", []string{"cfgVPNPanel(", "cfgIfacePanel("}},
		{"function viewConfigVrrp(", []string{"cfgVRRPPanel("}},
		{"function viewConfigStatic(", []string{"cfgStaticPanel("}},
		{"function viewConfigDynamic(", []string{"cfgDynamicPanel("}},
		{"function viewConfigQos(", []string{"cfgQosPanel("}},
	}
	for _, c := range cases {
		body := funcBody(t, html, c.fn)
		for _, p := range c.panels {
			if !strings.Contains(body, p) {
				t.Errorf("%s 没有画 %s", c.fn, p)
			}
		}
		// 设备信息页是唯一带主机名的页：其余页出现 hostname 输入就是重复编辑入口。
		if c.fn != "function viewConfig(" && strings.Contains(body, "cfgDevicePanel(") {
			t.Errorf("%s 又把设备信息面板塞回来了", c.fn)
		}
	}

	// hostname 的输入框与它的保存路径必须成对：一个 id 写错，页面上就是一个
	// 永远存不进去的输入框。
	dev := funcBody(t, html, "function cfgDevicePanel(")
	if !strings.Contains(dev, `id="cfg-hostname"`) || !strings.Contains(dev, `data-cfg="hostname"`) {
		t.Error("设备信息页没有可编辑的 hostname 输入框")
	}
	if !strings.Contains(html, `if (p === "hostname") { cfg.hostname = val; return; }`) {
		t.Error("hostname 输入没有接进草稿（cfgSetPath）")
	}
}

// 隧道表格的接线：下载按钮、上传框、「以文件为准」、以及预览里那两条
// 「确认 / 取消」。少接一处的结果不是报错，而是按钮点了没反应——所以这里
// 按「点了会发生什么」逐条钉住。
func TestConsoleTunnelTableWiring(t *testing.T) {
	html := string(consoleHTML)

	view := funcBody(t, html, "function viewUnderlayTunnels(")
	if !strings.Contains(view, "undCSVPanel(doc, u)") {
		t.Fatal("隧道页没有画表格面板（undCSVPanel）")
	}

	panel := funcBody(t, html, "function undCSVPanel(")
	for _, want := range []string{
		`data-act="und-csv-download"`,
		`data-act="und-csv-pick"`,
		`id="und-csv-file"`,
		`id="und-csv-prune"`,
		"undCSVLegend(doc)",
		"undCSVResult(u)",
	} {
		if !strings.Contains(panel, want) {
			t.Errorf("表格面板缺少 %s", want)
		}
	}
	// 列说明由后端给（doc.csv.columns），页面自己再抄一份就会和表头对不上。
	legend := funcBody(t, html, "function undCSVLegend(")
	if !strings.Contains(legend, "doc.csv") || !strings.Contains(legend, ".hint") {
		t.Error("表格格式说明没有从后端拿列定义（doc.csv.columns）")
	}

	// 上传：读字节、问控制器、先预览。
	loaded := funcBody(t, html, "function undCSVLoaded(")
	if !strings.Contains(loaded, "readAsArrayBuffer") {
		t.Error("上传按文本读文件了：GBK 会被浏览器先糟蹋一遍，错误就指错了地方")
	}
	post := funcBody(t, html, "function undCSVPost(")
	for _, want := range []string{"/api/underlay/tunnels/import", "undCSVB64(", "prune:", "dry:", "Content-Type"} {
		if !strings.Contains(post, want) {
			t.Errorf("上传请求缺少 %s", want)
		}
	}
	// 错误也带 plan：行错误要能一次看全，而不是只留一句「失败」。
	if !strings.Contains(post, "o.plan") {
		t.Error("上传没有接住返回的计划：行错误会看不到")
	}
	// 同一个文件改完再选一次也要触发 change。
	if !strings.Contains(loaded, "input.value") {
		t.Error("选完文件没有清空 input：改完再选同一个文件不会触发 change")
	}

	download := funcBody(t, html, "function undCSVDownload(")
	if !strings.Contains(download, "/api/underlay/tunnels/csv") || !strings.Contains(download, "download") {
		t.Error("下载按钮没有走 /api/underlay/tunnels/csv 并保存文件")
	}

	// 预览上的两个动作 + 勾选「以文件为准」要重算预览。
	bd := funcBody(t, html, "function bind(")
	if !strings.Contains(bd, "undCSVLoaded(ucf)") {
		t.Error("bind() 没有接文件选择：选了文件没反应")
	}
	if !strings.Contains(bd, "undCSVPost(true)") {
		t.Error("bind() 改了「以文件为准」却没有重算预览：删除条数会停在旧口径")
	}
	// 预览上的两个动作也在这条链上：按钮没有分发就是死按钮。
	for _, want := range []string{"undCSVApply()", "undCSVCancel()", "undCSVPick()", "undCSVDownload()"} {
		if !strings.Contains(bd, want) {
			t.Errorf("动作分发里没有 %s：按钮点了没反应", want)
		}
	}
	// 上传结果是异步回来的，此时文件输入框往往还带着焦点；render 的焦点护栏
	// 会整帧跳过，预览就不出现——必须先失焦。
	if !strings.Contains(post, "blurContent()") {
		t.Error("上传回调没有先失焦：焦点护栏会吞掉预览那一帧")
	}
}

// TestConsoleLedgerRetentionWiring 钉住「台账与告警」页上的保留天数：
// 输入框要有、保存动作要分发、请求体要带 retention_days，缩短期要先问一句。
// 少任何一处，用户看到的是"填了数字、点了保存、什么也没发生"。
func TestConsoleLedgerRetentionWiring(t *testing.T) {
	html := string(consoleHTML)
	ops := funcBody(t, html, "function viewOps()")
	for _, want := range []string{`id="ops-keep"`, `data-act="retention-save"`, "rt.min_days", "rt.max_days"} {
		if !strings.Contains(ops, want) {
			t.Errorf("台账与告警页缺少 %s", want)
		}
	}
	// 动作分发是一条 else-if 链：取本分支到自己结束、下一个分支开始为止。
	i := strings.Index(html, `else if (act === "retention-save") {`)
	if i < 0 {
		t.Fatal("动作分发里没有 retention-save：按钮点了没反应")
	}
	rest := html[i:]
	if j := strings.Index(rest, `else if (act === "diag-refresh")`); j > 0 {
		rest = rest[:j]
	}
	for _, want := range []string{"retention_days", "confirm(", "webhook_url", "loss_permille"} {
		if !strings.Contains(rest, want) {
			t.Errorf("保存保留天数的分支缺少 %s", want)
		}
	}
	// 只改告警设置的老路径不能把页面上已经读到的保留期弄丢（否则一保存
	// webhook，页面上那个数字就变回默认值，看起来像被重置了）。
	if !strings.Contains(funcBody(t, html, `else if (act === "alert-save") {`), "res.retention") {
		t.Error("保存告警设置后没有把 retention 一起带回 S.alerts")
	}
}

// TestConsoleClusterPageWiring 钉住「控制器集群」页的三种玩家动作：
// 开启集群（单机 → 第 1 台）、加人（第 2、3 台）、替换一台、下线一台。
//
// 这一页的价值全在"点了会发生什么"上：按钮没有分发、请求少带一个字段、
// 少一个 confirm，症状都不是报错，而是"点了没反应"或者"点了，集群少了一票"。
func TestConsoleClusterPageWiring(t *testing.T) {
	html := string(consoleHTML)

	// 入口只给运营账号：成员地址属于运营侧信息，租户不该看到。
	if !strings.Contains(html, `data-view="cluster" data-roles="operator"`) {
		t.Error("导航里没有「控制器集群」（或没有按运营角色隐藏）")
	}
	if !strings.Contains(html, `S.view === "cluster") c = viewCluster(st)`) {
		t.Error("render 没有把 cluster 这一页接到 viewCluster")
	}

	view := funcBody(t, html, "function viewCluster(st)")
	// 单机：开启集群就等于填本机地址 + 一个明确的按钮。
	for _, want := range []string{`id="cl-init-endpoint"`, `id="cl-init-console"`,
		`id="cl-init-name"`, `data-act="cluster-init"`, "cv.mode"} {
		if !strings.Contains(view, want) {
			t.Errorf("开启集群面板缺少 %s", want)
		}
	}
	// 集群：概览要有"有多数派"，成员表每一行要能下线/替换，加入码要能复制。
	for _, want := range []string{"cv.quorum", "data-act=\"cluster-remove\"",
		"data-act=\"cluster-replace\"", `id="cl-endpoint"`, `id="cl-console"`,
		`data-act="cluster-token"`, `data-act="cluster-copy"`, "S.clusterTok", ".command",
		"clusterRoleTag(m)", "behind"} {
		if !strings.Contains(view, want) {
			t.Errorf("集群视图缺少 %s", want)
		}
	}
	// 加入码只显示一次：界面必须说清楚，否则用户关掉页面再来要，只能重新生成。
	if !strings.Contains(view, "只显示这一次") {
		t.Error("加入码面板没有提醒「只显示这一次」")
	}

	// 动作分发
	init := actBranch(t, html, "cluster-init")
	for _, want := range []string{`"/api/cluster"`, `action: "init"`, "endpoint", "confirm("} {
		if !strings.Contains(init, want) {
			t.Errorf("开启集群缺少 %s", want)
		}
	}
	tok := actBranch(t, html, "cluster-token")
	for _, want := range []string{`action: "token"`, "replace_id", "kind", "S.clusterReplace"} {
		if !strings.Contains(tok, want) {
			t.Errorf("生成加入码缺少 %s", want)
		}
	}
	// 替换是"同一张表单换个目标"：地址照填，只是多带一个 replace_id。
	if !strings.Contains(tok, `? "replace" : "add"`) || !strings.Contains(tok, "replace_id: rep") {
		t.Error("加入码没有区分 add / replace：替换会退化成「又多一台」")
	}
	rep := actBranch(t, html, "cluster-replace")
	if !strings.Contains(rep, `data-id`) || !strings.Contains(rep, "S.clusterReplace") {
		t.Error("「替换」按钮没有记住目标成员")
	}
	if !strings.Contains(actBranch(t, html, "cluster-replace-clear"), `S.clusterReplace = ""`) {
		t.Error("「取消替换」没有清掉目标")
	}
	rm := actBranch(t, html, "cluster-remove")
	for _, want := range []string{`action: "remove"`, "confirm(", "id: mid"} {
		if !strings.Contains(rm, want) {
			t.Errorf("下线缺少 %s", want)
		}
	}
	// 下线会消耗一票：必须先把"剩下的还占多数吗"讲清楚，而不是只问一句确定。
	if !strings.Contains(rm, "多数派") {
		t.Error("下线的确认框没有说明它会从多数派里拿走一票")
	}

	// 提交失败的红条：这是"客户以为发布成功了"的唯一出口。
	if !strings.Contains(html, "st.cluster.degraded") || !strings.Contains(html, `class="banner danger"`) {
		t.Error("提交失败时页面没有红条（改动没落盘却看不出）")
	}
}

// actBranch 取一条 `else if (act === "name")` 分支，到下一个分支为止。
func actBranch(t *testing.T, html, name string) string {
	t.Helper()
	marker := `else if (act === "` + name + `") {`
	i := strings.Index(html, marker)
	if i < 0 {
		t.Fatalf("动作分发里没有 %s：按钮点了没反应", name)
	}
	rest := html[i+len(marker):]
	if j := strings.Index(rest, "\n        else "); j > 0 {
		rest = rest[:j]
	}
	return rest
}

// funcBody 取一个函数体：从签名到下一个顶格两空格的 "}" 行。
func funcBody(t *testing.T, html, sig string) string {
	t.Helper()
	i := strings.Index(html, sig)
	if i < 0 {
		t.Fatalf("console.html 里没有 %s", sig)
	}
	rest := html[i:]
	end := strings.Index(rest, "\n  }\n")
	if end < 0 {
		t.Fatalf("找不到 %s 的函数结尾", sig)
	}
	return rest[:end]
}
