package agent

// G-2：sr-fib 的隧道表由本进程（sr-agent）派生并推送。
//
// 为什么在这台盒子上、而不是在控制器：sr-fib 要的每一行是
// {next_hop, interface, color}，这三个键分属三个地方 —— next_hop 与 color 只有
// 控制器知道（wire.TunnelSpec.PeerOverlay / .Color），interface 只有这台盒子自己
// 知道（VPP 的口名，重启一次就换）。而这两半今天都已经在 sr-agent 这一个进程里：
// 隧道段是它装的（TunnelSync），route.json 是 sr-apply 落的、它能读。所以派生器
// 是 sr-agent 里的一个组件，不是新进程、不是新 daemon。
//
// 四条纪律（docs/g2-tunnel-table-derivation-plan.md §4）：
//
//  1. 口名只认读回：拿 Link 的 sw_if_index 去 VPP 的接口表里查 Name，不拼
//     "wg<instance>"（那是观察，不是契约）。查不到就把这一行排除并说明原因 ——
//     绝不能推一条接口名为空的行，sr-fib 会因为空接口名把整份配置拒掉。
//  2. 整份替换，所以先读再写：PUT /config 是整份替换，escape_all_colors 不是
//     本组件的职责，必须原样带回去（今天 lab 脚本也是这么做的）。
//  3. 颜色缺了就整轮拒绝：老控制器不发 color 时设备只有 0。按 0 装进去的后果是
//     主备关系静默消失（所有路径同优先级），比装不出来难查得多。
//  4. 不落盘：这张表每一行只在那一刻成立（口名会被 VPP 重启换掉，下一跳会被
//     控制台删掉）。盘上存一份，开机就得在"用盘上那份"和"重新派生"之间选，
//     而选前者正是"基线是假的"那类缺陷的源头。
//
// 触发：每拍心跳调一次 MaybeRun（TunnelSync.Kick 之后）。不是独立定时器 ——
// 它借用的是心跳这个既有节拍；而每轮都先 GET 再比，所以 sr-fib 自己重启把表
// 丢回 fib.json 的空表时，下一拍就能发现差异并重推，这个洞不需要另设检测。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// fibTableTimeout bounds one derivation round (one GET + at most one PUT against
// loopback). A wedged sr-fib must never hold the heartbeat's goroutine for
// longer than this, which is why the round runs on its own goroutine.
const fibTableTimeout = 15 * time.Second

// FibTableSync derives the sr-fib tunnel table and pushes it when it differs
// from what sr-fib currently runs.
type FibTableSync struct {
	ts *TunnelSync

	routeConfPath string // 这台盒子的 route.json：vrfs 的唯一来源
	fibConfPath   string // 这台盒子的 fib.json：sr-fib 的 status_addr（推送地址）

	hc   *http.Client
	logf func(string, ...any)

	// 测试缝：真实现读盘，测试直接给答案。
	readVRFs func() ([]int, error)
	readFB   func() ([]FibFallback, error)
	baseURL  func() (string, error)

	mu       sync.Mutex
	busy     bool
	lastNote string // 只在 mu 内读写：同一句原因不每拍刷一行
}

// NewFibTableSync wires the deriver. logf may be nil.
func NewFibTableSync(ts *TunnelSync, routeConfPath, fibConfPath string, logf func(string, ...any)) *FibTableSync {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	f := &FibTableSync{
		ts:            ts,
		routeConfPath: routeConfPath,
		fibConfPath:   fibConfPath,
		hc:            &http.Client{Timeout: 5 * time.Second},
		logf:          logf,
	}
	f.readVRFs = func() ([]int, error) { return readRouteVRFs(routeConfPath) }
	f.readFB = func() ([]FibFallback, error) { return nil, nil }
	f.baseURL = func() (string, error) { return fibStatusBase(fibConfPath) }
	return f
}

// SetFallbacks installs the source of the per-(VPN, colour) fallback switches.
// sr-agent wires it to the installed bundle's 7th segment; a nil source means
// "this box has no policy segment", which derives to an empty list.
func (f *FibTableSync) SetFallbacks(fn func() ([]FibFallback, error)) {
	if fn != nil {
		f.readFB = fn
	}
}

// MaybeRun starts one derivation round in the background unless one is already
// running. Coalescing (not queueing) is right here for the same reason the
// reconcile uses a capacity-1 wake: the work is "reach the current state".
func (f *FibTableSync) MaybeRun(ctx context.Context) {
	f.mu.Lock()
	if f.busy {
		f.mu.Unlock()
		return
	}
	f.busy = true
	f.mu.Unlock()

	go func() {
		defer func() {
			f.mu.Lock()
			f.busy = false
			f.mu.Unlock()
		}()
		rctx, cancel := context.WithTimeout(ctx, fibTableTimeout)
		defer cancel()
		_ = f.Sync(rctx)
	}()
}

// fibRowDoc is one row of sr-fib's tunnel table (cmd/sr-fib: tunnelConfig).
type fibRowDoc struct {
	NextHop   string `json:"next_hop"`
	Interface string `json:"interface"`
	Color     uint8  `json:"color,omitempty"`
	// Weight 是这条隧道的分担权重（VPP fib_path.weight，u8）。omitempty：
	// 1（默认等价）不写，与老配置字节同形 —— diff 不翻行。动态档位（task 56）
	// 也写这一格：sr-fib 是纯对账者，它只需要"现在用多少"，不需要知道是
	// 配置的还是算出来的。
	Weight uint8 `json:"weight,omitempty"`
}

// fibConfigDoc is the GET/PUT /config body of sr-fib. Whole-replacement: the
// escape switch rides along untouched because this component does not own it.
type fibConfigDoc struct {
	VRFs            []int            `json:"vrfs"`
	Tunnels         []fibRowDoc      `json:"tunnels"`
	EscapeAllColors bool             `json:"escape_all_colors"`
	Fallbacks       []fibFallbackDoc `json:"fallbacks,omitempty"`
}

// fibFallbackDoc 是"某 VPN 的某颜色查不到就退到颜色 0"的一行。它是 sr-fib 在
// 颜色表里装的一条默认路由，来源是配置包第 7 段（策略页的复选框）。
type fibFallbackDoc struct {
	VPN   uint8 `json:"vpn"`
	Color uint8 `json:"color"`
}

// FibFallback 是这段派生的输入：由 sr-agent 从已安装配置包的策略段解出来。
type FibFallback struct {
	VPN   uint8
	Color uint8
}

// fibAnswer is the part of sr-fib's answer this component reports: whether the
// round it accepted actually converged, and why not.
type fibAnswer struct {
	Last *struct {
		Converged bool     `json:"converged"`
		Error     string   `json:"error"`
		Warnings  []string `json:"warnings"`
	} `json:"last"`
}

// Sync runs one derivation round: read the data plane, derive, compare with
// sr-fib's current table, push only when it differs. Every early return states
// its reason once (see note) — a deriver that silently does nothing is
// indistinguishable from one that is not running.
func (f *FibTableSync) Sync(ctx context.Context) error {
	rows := f.ts.FibRows()

	// 启动瞬态守卫（2026-11-12 换件现场实证）：TunnelSync 还没完成过一轮对账时
	// links 是空的 → rows 空，这时推出去的空表会把 sr-fib 内存里已有的隧道行清掉
	// （sr-fib 按「下一跳不在隧道表里」跳过所有远端路径，直到下一拍补回，实测 ~5s）。
	// 两种"rows 空"必须分开：控制器真的删光了所有隧道（SyncedOnce=true，推空表
	// 是对的）和"隧道事实还没建立"（本轮不推，下一拍再说）。
	if len(rows) == 0 && !f.ts.SyncedOnce() {
		return f.note("TunnelSync 还没完成过一轮对账（隧道事实还没建立），本轮不推 sr-fib 隧道表 —— 现在推空表会把已有的行清掉")
	}

	// 规则 3：颜色缺失整轮拒绝。指针为 nil 只可能是老控制器的 bundle。
	for _, r := range rows {
		if r.Color == nil {
			return f.note("控制器下发的隧道段没有 color 字段（隧道 %s）：拒绝派生 sr-fib 隧道表 —— "+
				"按 0 装进去会让主备关系静默消失（所有路径同优先级），这比装不出来难查得多", r.TunnelID)
		}
	}

	vrfs, err := f.readVRFs()
	if err != nil {
		return f.note("读 %s 的 vrfs: %v（本轮不推：sr-fib 会拒绝空 VPN 集合，而\"还没配\"与\"一个 VPN 都不服务\"是两件事）",
			f.routeConfPath, err)
	}
	if len(vrfs) == 0 {
		return f.note("%s 里还没有 vrfs（第 4 段还没到），本轮不推 sr-fib 隧道表", f.routeConfPath)
	}

	dp := f.ts.DataPlane()
	if dp == nil {
		return f.note("还没有数据面连接（VPP 不在或正在重启），本轮不推 sr-fib 隧道表")
	}
	ifaces, err := dp.DumpInterfaces(ctx)
	if err != nil {
		return f.note("读 VPP 接口表（解析隧道口名字）: %v", err)
	}
	byIdx := make(map[uint32]string, len(ifaces))
	for _, i := range ifaces {
		byIdx[i.SWIfIndex] = i.Name
	}
	tunnels := make([]fibRowDoc, 0, len(rows))
	var skipped []string
	for _, r := range rows {
		name, ok := byIdx[r.SWIfIndex]
		if !ok {
			// 规则 1：查不到就排除并说明，不猜名字。
			skipped = append(skipped, fmt.Sprintf("%s(sw_if_index %d 不在接口表里)", r.TunnelID, r.SWIfIndex))
			continue
		}
		tunnels = append(tunnels, fibRowDoc{NextHop: r.NextHop, Interface: name, Color: *r.Color,
			Weight: effectiveWeight(r)})
	}
	sort.Slice(tunnels, func(i, j int) bool { return tunnels[i].NextHop < tunnels[j].NextHop })

	base, err := f.baseURL()
	if err != nil {
		return f.note("sr-fib 的推送地址读不出来: %v", err)
	}
	cur, err := f.getConfig(ctx, base)
	if err != nil {
		return f.note("sr-fib %s 连不上（还没起来？下一拍再试）: %v", base, err)
	}
	fbs, err := f.readFB()
	if err != nil {
		return f.note("读配置包第 7 段的兜底开关: %v（本轮不推：把兜底当成\"全关\"会静默撤掉已经装好的默认路由）", err)
	}
	fbRows := make([]fibFallbackDoc, 0, len(fbs))
	for _, fb := range fbs {
		fbRows = append(fbRows, fibFallbackDoc{VPN: fb.VPN, Color: fb.Color})
	}
	derived := fibConfigDoc{VRFs: vrfs, Tunnels: tunnels, EscapeAllColors: cur.EscapeAllColors, Fallbacks: fbRows}
	if sameFibTables(cur, derived) {
		f.clearNote()
		return nil
	}

	ans, code, err := f.putConfig(ctx, base, derived)
	if err != nil {
		return f.note("推 sr-fib 隧道表失败（%s）: %v", base, err)
	}
	if code != http.StatusOK && code != http.StatusAccepted {
		return f.note("sr-fib 拒绝了隧道表（HTTP %d）：%s", code, fibAnswerText(ans))
	}
	f.clearNote()
	state := "已收敛"
	if code == http.StatusAccepted {
		// 202 = 已接受、这一轮没收敛（sr-fib 的下一轮自己重试）。这不是"推成功"。
		state = "已接受、尚未收敛"
	} else if ans.Last != nil && !ans.Last.Converged {
		state = "已接受，但本轮没收敛"
	}
	skipNote := ""
	if len(skipped) > 0 {
		skipNote = fmt.Sprintf("，跳过 %d 条（%s）", len(skipped), strings.Join(skipped, "、"))
	}
	f.logf("sr-fib 隧道表%s：%d 行、vrfs=%v、escape=%v、兜底 %d 条%s%s",
		state, len(tunnels), vrfs, derived.EscapeAllColors, len(fbRows), skipNote, fibWarnText(ans))
	return nil
}

// sameFibTables compares the two halves this component owns. Order-insensitive
// on purpose: sr-fib keeps the order it was given, and a reshuffle is not a
// change worth a PUT.
func sameFibTables(a, b fibConfigDoc) bool {
	return sameInts(a.VRFs, b.VRFs) && sameRows(a.Tunnels, b.Tunnels) && sameFallbacks(a.Fallbacks, b.Fallbacks)
}

func sameFallbacks(a, b []fibFallbackDoc) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]fibFallbackDoc(nil), a...)
	y := append([]fibFallbackDoc(nil), b...)
	less := func(r []fibFallbackDoc) {
		sort.Slice(r, func(i, j int) bool {
			if r[i].VPN != r[j].VPN {
				return r[i].VPN < r[j].VPN
			}
			return r[i].Color < r[j].Color
		})
	}
	less(x)
	less(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func sameInts(a, b []int) bool {
	x := append([]int(nil), a...)
	y := append([]int(nil), b...)
	sort.Ints(x)
	sort.Ints(y)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func sameRows(a, b []fibRowDoc) bool {
	x := append([]fibRowDoc(nil), a...)
	y := append([]fibRowDoc(nil), b...)
	less := func(r []fibRowDoc) {
		sort.Slice(r, func(i, j int) bool {
			if r[i].NextHop != r[j].NextHop {
				return r[i].NextHop < r[j].NextHop
			}
			return r[i].Interface < r[j].Interface
		})
	}
	less(x)
	less(y)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func (f *FibTableSync) getConfig(ctx context.Context, base string) (fibConfigDoc, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/config", nil)
	if err != nil {
		return fibConfigDoc{}, err
	}
	resp, err := f.hc.Do(req)
	if err != nil {
		return fibConfigDoc{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fibConfigDoc{}, fmt.Errorf("GET /config: HTTP %d", resp.StatusCode)
	}
	var doc fibConfigDoc
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return fibConfigDoc{}, fmt.Errorf("GET /config 不是 JSON: %w", err)
	}
	return doc, nil
}

func (f *FibTableSync) putConfig(ctx context.Context, base string, doc fibConfigDoc) (fibAnswer, int, error) {
	body, err := json.Marshal(doc)
	if err != nil {
		return fibAnswer{}, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, strings.TrimRight(base, "/")+"/config", bytes.NewReader(body))
	if err != nil {
		return fibAnswer{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.hc.Do(req)
	if err != nil {
		return fibAnswer{}, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var ans fibAnswer
	_ = json.Unmarshal(raw, &ans)
	if resp.StatusCode >= 300 {
		return ans, resp.StatusCode, fmt.Errorf("%s", strings.TrimSpace(string(raw)))
	}
	return ans, resp.StatusCode, nil
}

func fibAnswerText(a fibAnswer) string {
	if a.Last == nil {
		return "（应答里没有 last 段）"
	}
	if a.Last.Error != "" {
		return a.Last.Error
	}
	if len(a.Last.Warnings) > 0 {
		return strings.Join(a.Last.Warnings, "；")
	}
	return "（没有 last.error，也没有 last.warnings）"
}

func fibWarnText(a fibAnswer) string {
	if a.Last == nil {
		return ""
	}
	var parts []string
	if a.Last.Error != "" {
		parts = append(parts, "last.error="+a.Last.Error)
	}
	if len(a.Last.Warnings) > 0 {
		parts = append(parts, "warnings="+strings.Join(a.Last.Warnings, "；"))
	}
	if len(parts) == 0 {
		return ""
	}
	return "（" + strings.Join(parts, "，") + "）"
}

// note logs a reason once per distinct value and returns it as an error.
func (f *FibTableSync) note(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	f.mu.Lock()
	changed := f.lastNote != msg
	f.lastNote = msg
	f.mu.Unlock()
	if changed {
		f.logf("sr-fib 隧道表: %s", msg)
	}
	return fmt.Errorf("%s", msg)
}

func (f *FibTableSync) clearNote() {
	f.mu.Lock()
	f.lastNote = ""
	f.mu.Unlock()
}

// readRouteVRFs reads this box's route.json and returns the VPN ids it serves.
// The file's vrfs rows are sr-route's (id + RT lists); only the ids are this
// table's business.
func readRouteVRFs(path string) ([]int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		VRFs []struct {
			ID uint8 `json:"id"`
		} `json:"vrfs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s 不是 JSON 对象: %w", path, err)
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(doc.VRFs))
	for _, v := range doc.VRFs {
		if seen[int(v.ID)] {
			continue
		}
		seen[int(v.ID)] = true
		out = append(out, int(v.ID))
	}
	sort.Ints(out)
	return out, nil
}

// fibStatusBase turns fib.json's status_addr into a URL this process can dial.
// Same rule as route.json's status_addr (see apply.routeStatusBase): a wildcard
// host is not the same as being reachable there, so it becomes loopback.
func fibStatusBase(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var doc struct {
		StatusAddr string `json:"status_addr"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("%s 不是 JSON 对象: %w", path, err)
	}
	addr := strings.TrimSpace(doc.StatusAddr)
	if addr == "" {
		return "", fmt.Errorf("%s 没有 status_addr：sr-fib 没开热更入口，隧道表推不进去", path)
	}
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return strings.TrimRight(addr, "/"), nil
	}
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		switch host := addr[:i]; host {
		case "", "0.0.0.0", "::", "[::]":
			addr = "127.0.0.1" + addr[i:]
		}
	}
	return "http://" + addr, nil
}

// effectiveWeight 把一行隧道读数折成 fib.json 里要写的 weight。
// task 54（2026-11-13，本轮）：静态 —— 配置值原样（0 = 默认 1，omitempty
// 落成"不写"，与老配置字节同形）。
// task 56（owner 已拍未做）：Dynamic 开时由档位函数按探针 SLA 输出 1..Weight
// 的有效值（丢包优先、四档、非线性，迟滞 3 拍 × 5s，一拍一个评估周期）；
// Dynamic 关 = 恒配置值。sr-fib 对两者一视同仁：weight 变了 → 路径身份变了
// → 重下发，它不需要知道值的来历。
func effectiveWeight(r FibTunnelRow) uint8 {
	return r.Weight
}
