// sr-frr 是盒子内核与客户侧 FRR 之间的两个方向的接缝：
//
//   - SDWAN → 客户：sr-route 的 RIB 以**内核 K 路由**写进对应 VPN 表（VPN 0 =
//     main/254，其余 = table N、出口 vpn<N>），客户侧 BGP/OSPF 用
//     `redistribute kernel` 再分发。linux-nl 不镜像这类路由，VPP 看不到；
//     zebra 把它们认成 K（distance 0）。
//   - 客户 → SDWAN：按（VPN, 来源）订阅 zebra 的再分发通知，按该 VPN 的白名单
//     过滤、扣掉本 VPN 正在导出的前缀（防回环），整份 PUT 给 sr-route。
//
// 配置由控制器发布（bundle 第 5 段）并经 sr-apply 推到
// /run/sr-frr/control.sock 的 PUT /config；PUT 只换运行时配置，绝不重启进程
// （退出会把上报的前缀清空一次，等于全网抖一下）。
//
// 两个方向的接缝有固定的次序：每一轮先把客户侧读完并上报（导入），再对账注入
// （导出）。这个次序是实测出来的（2026-11-06，lab-cpe3）。
//
// 这条接缝上没有任何"等一拍/维持旧值"的窗口（2026-11-07 owner 定案，AGENTS.md
// 第 3 条）：链路事件、zebra 重连、首轮对账都按当时的真实集合立即上报、立即
// 对账。代价写在 Importer.Reset 上 —— 重连到回放落地之间会短报一次。
//
// "本盒客户侧已经拥有的前缀不该被自己的 K 路由顶掉"这一条不在这里挡。K 路由
// 在 zebra 里是 distance 0，客户的 bgpd/ospfd 又对它做 redistribute kernel，于
// 是顶掉真路径、zebra 从此不发通知、远端反射回来的同前缀被当成远端路由装上隧
// 道 —— 这是一条路由环路，环是客户侧通告策略要断的（控制台的 CPE-SDWAN 那一行
// 只播本站客户侧的前缀），不是注入侧偷偷少注入。取证与配置解法见
// docs/vmlab-rebuild-plan.md §9.20（同一条链的四层取证在 §9.19）。
//
// 退出与崩溃的账：正常退出逐条撤掉 K 路由再删状态文件；被强杀时状态文件还
// 在，下次启动先把它当基线做差量 —— 内核不会替我们收尾。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"sdwan/controller/internal/frr"
	"sdwan/controller/internal/frr/krt"
)

type fileConfig struct {
	StatusURL     string          `json:"status_url"`
	ZServSocket   string          `json:"zserv_socket"`
	StateFile     string          `json:"state_file"`
	PollSec       int             `json:"poll_sec"`
	ControlSocket string          `json:"control_socket"`
	ExportVRFs    json.RawMessage `json:"export_vrfs"`
	ImportRules   json.RawMessage `json:"import_rules"`

	// 已退役的旧 schema（ZAPI/1250 导出 + 单 VRF 只订 BGP 的 import）。出现
	// 就告警并把两个方向都置空：绝不按旧值继续跑（那条导出路径已经删了）。
	LegacyImport   json.RawMessage `json:"import"`
	LegacyVRFs     json.RawMessage `json:"vrfs"`
	LegacyDistance *int            `json:"distance"`
	LegacyMetric   *int            `json:"metric"`
}

const (
	defaultZServSocket   = "/run/frr/zserv.api"
	defaultStateFile     = "/run/sr-frr/state.json"
	defaultControlSocket = "/run/sr-frr/control.sock"
)

func loadConfig(path string) (*fileConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var cfg fileConfig
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.ZServSocket == "" {
		cfg.ZServSocket = defaultZServSocket
	}
	if cfg.StateFile == "" {
		cfg.StateFile = defaultStateFile
	}
	if cfg.ControlSocket == "" {
		cfg.ControlSocket = defaultControlSocket
	}
	if cfg.PollSec == 0 {
		cfg.PollSec = 2
	}
	if cfg.PollSec < 0 {
		return nil, errors.New("poll_sec 不能为负")
	}
	if cfg.LegacyImport != nil || cfg.LegacyVRFs != nil || cfg.LegacyDistance != nil || cfg.LegacyMetric != nil {
		if len(bytes.TrimSpace(cfg.ExportVRFs)) > 0 || len(bytes.TrimSpace(cfg.ImportRules)) > 0 {
			return nil, errors.New("配置里同时出现旧 schema（vrfs/import/distance/metric）与 export_vrfs/import_rules；删掉旧键再启动")
		}
		log.Printf("sr-frr: 配置是旧 schema（vrfs/import/distance/metric），export 与 import 都置空；等控制器重写 /etc/sr-frr.json")
		cfg.ExportVRFs = nil
		cfg.ImportRules = nil
	}
	return &cfg, nil
}

// runtimeConfig is one applied configuration. Slices are never mutated in
// place: a PUT swaps the whole struct, so readers can share them.
type runtimeConfig struct {
	exportVRFs []uint32
	rules      []frr.ImportRule
	exportRaw  []byte
	importRaw  []byte
	hash       string
}

func compileRuntime(exportRaw, importRaw []byte) (runtimeConfig, error) {
	var cfg runtimeConfig
	vrfs, err := frr.CompileExportVRFs(exportRaw)
	if err != nil {
		return cfg, err
	}
	rules, err := frr.CompileImportRules(importRaw)
	if err != nil {
		return cfg, err
	}
	// 出口与订阅的 ifindex 都在这里暴露，而不是等第一轮轮询才发现。
	for _, v := range vrfs {
		if _, err := krt.Index(v); err != nil {
			return cfg, fmt.Errorf("导出 VPN %d 的出口: %w", v, err)
		}
	}
	h, err := frr.ConfigHash(exportRaw, importRaw)
	if err != nil {
		return cfg, err
	}
	cfg.exportVRFs = vrfs
	cfg.rules = rules
	cfg.exportRaw = append([]byte(nil), exportRaw...)
	cfg.importRaw = append([]byte(nil), importRaw...)
	cfg.hash = h
	return cfg, nil
}

type runtimeStore struct {
	mu  sync.RWMutex
	cfg runtimeConfig
}

func (s *runtimeStore) snapshot() runtimeConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *runtimeStore) set(cfg runtimeConfig) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

// vpnMapper maps a ZAPI header vrf_id (kernel VRF ifindex) to a VPN. It is
// replaced as a whole so the read loop never blocks on a config change.
type vpnMapper struct {
	p atomic.Pointer[map[uint32]uint8]
}

func (m *vpnMapper) set(v map[uint32]uint8) { m.p.Store(&v) }

func (m *vpnMapper) lookup(id uint32) (uint8, bool) {
	p := m.p.Load()
	if p == nil {
		return 0, id == 0
	}
	v, ok := (*p)[id]
	return v, ok
}

// exportState owns the netlink client and the K-route reconciler. One mutex
// covers the poll loop, the control socket's touch, and /status.
type exportState struct {
	mu      sync.Mutex
	dp      *krt.Client
	ex      *frr.Exporter
	lastSig string
	skipSig string
	plan    []frr.Route
	skipped []frr.Route
	lastErr string
}

func planSig(routes []frr.Route) string {
	var b strings.Builder
	for _, r := range routes {
		b.WriteString(strconv.FormatUint(uint64(r.Table), 10))
		b.WriteByte('|')
		b.WriteString(r.Prefix.String())
		b.WriteByte('|')
		b.WriteString(strconv.Itoa(r.OIF))
		b.WriteByte('\n')
	}
	return b.String()
}

func exportList(vrfs []uint32) string {
	parts := make([]string, 0, len(vrfs))
	for _, v := range vrfs {
		parts = append(parts, fmt.Sprintf("vpn%d→table %d", v, frr.KernelTable(uint8(v))))
	}
	return strings.Join(parts, ",")
}

func vpnOfTable(table uint32) uint32 {
	if table == 254 {
		return 0
	}
	return table
}

// exportedSet is the anti-loop input: what this VPN currently has as K routes.
// Read under the same lock as Apply so a concurrent config change cannot make
// it claim prefixes that were just withdrawn.
func (e *exportState) exportedSet(exportVRFs []uint32) map[uint8]map[netip.Prefix]struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[uint8]map[netip.Prefix]struct{}, len(exportVRFs))
	for _, r := range e.plan {
		vpn := vpnOfTable(r.Table)
		if vpn > 255 {
			continue
		}
		set := out[uint8(vpn)]
		if set == nil {
			set = map[netip.Prefix]struct{}{}
			out[uint8(vpn)] = set
		}
		set[r.Prefix] = struct{}{}
	}
	return out
}

// reconcile brings the kernel tables in line with sr-route's RIB. Callers are
// the poll loop; the control socket only touches prefixes that already exist.
// statusURL and stateFile are box keys, not part of the published config, so
// they come from the file the process was started with.
func (e *exportState) reconcile(ctx context.Context, hc *http.Client, cfg runtimeConfig, statusURL, stateFile string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	var routes []frr.RIBRoute
	if len(cfg.exportVRFs) > 0 {
		var err error
		routes, err = fetchRIB(ctx, hc, statusURL)
		if err != nil {
			e.lastErr = err.Error()
			return fmt.Errorf("读 RIB: %w", err)
		}
	}
	plan, err := frr.PlanFrom(cfg.exportVRFs, routes, krt.Index)
	if err != nil {
		e.lastErr = err.Error()
		return err
	}
	// 先落盘再下发：中途被杀时，文件里是"打算写的全部"，最多多删几条不存在
	// 的路由，不会漏删。计划为空时删文件 —— 状态文件存在就代表我们留了东西。
	if sig := planSig(plan.Routes); sig != e.lastSig {
		if len(plan.Routes) == 0 {
			err = frr.RemoveState(stateFile)
		} else {
			err = frr.SaveState(stateFile, plan.Routes)
		}
		if err != nil {
			e.lastErr = err.Error()
			return fmt.Errorf("写状态文件: %w", err)
		}
		e.lastSig = sig
	}
	res, err := e.ex.Apply(ctx, plan)
	if err != nil {
		e.lastErr = err.Error()
		return err
	}
	e.plan = e.ex.Routes()
	e.skipped = res.Skipped
	e.lastErr = ""
	if res.Changed() {
		log.Printf("sr-frr: 收敛 routes+%d/-%d（%s）", res.Added, res.Removed, exportList(cfg.exportVRFs))
	}
	if res.Adopted > 0 {
		log.Printf("sr-frr: 收编上一轮留下的 %d 条 K 路由", res.Adopted)
	}
	// 跳过的前缀只在集合变化时打一行：盒子自己的网段（或别人的同前缀路由）
	// 永远不注入，但这件事必须留痕，否则"少了几条"看起来像丢路由。
	if sig := planSig(res.Skipped); sig != e.skipSig {
		e.skipSig = sig
		if len(res.Skipped) > 0 {
			log.Printf("sr-frr: 跳过 %d 条（盒子自己已有同前缀路由或就是它的接口网段，不覆盖）: %s",
				len(res.Skipped), routeList(res.Skipped))
		}
	}
	return nil
}

func routeList(routes []frr.Route) string {
	parts := make([]string, 0, len(routes))
	for _, r := range routes {
		parts = append(parts, fmt.Sprintf("%s(表 %d)", r.Prefix, r.Table))
	}
	return strings.Join(parts, ",")
}

// touch forces zebra to recompute redistribution for the given VPNs: when a
// protocol is added to a VPN that was already exporting, the K routes are
// already in the table and `redistribute kernel` does not look at them again
// (measured, §1.3). Delete+add is pure kernel; VPP never sees it.
func (e *exportState) touch(ctx context.Context, vrfs []uint32) (map[uint32]int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	want := make(map[uint32]bool, len(vrfs))
	for _, v := range vrfs {
		want[v] = true
	}
	touched := map[uint32]int{}
	for _, r := range e.plan {
		vpn := vpnOfTable(r.Table)
		if !want[vpn] {
			continue
		}
		if err := e.dp.RouteDelete(ctx, r); err != nil {
			return touched, fmt.Errorf("touch 删 %s（表 %d）: %w", r.Prefix, r.Table, err)
		}
		if err := e.dp.RouteAdd(ctx, r); err != nil {
			return touched, fmt.Errorf("touch 加 %s（表 %d）: %w", r.Prefix, r.Table, err)
		}
		touched[vpn]++
	}
	return touched, nil
}

// linkSettle 把一次拆除（VRF 设备 + 挂在它下面的接口 + 地址）产生的连续事件
// 合成一个窗口，避免对同一次变化重放好几遍。
const linkSettle = 300 * time.Millisecond

// watchLinkEvents 守住一个实测坑：
//
//   - zebra 丢 K 路由视图：接口 down / 删接口 / 删地址会让 zebra 把自己 RIB
//     里的 K 路由整表清掉，内核里还在、sr-frr 也以为还注入着，redistribute
//     kernel 于是静默停发，客户侧从此学不到远端前缀。zebra 对重复 add 不发
//     通知，只有 delete+add 能让它重新学到 —— 复用 touch。
//
// 这里只重放 K 路由，不在导入侧维持旧集合：链路事件带走客户会话之后，本地集
// 合由 zebra 的撤销通知直接归零，下一次轮询就把"现在没有"报出去（2026-11-07
// 定案，见文件头与 AGENTS.md 第 3 条）。
func watchLinkEvents(ctx context.Context, exp *exportState, rt *runtimeStore) {
	ch, err := krt.WatchLinks(ctx)
	if err != nil {
		log.Printf("sr-frr: 链路事件监听没开（接口一变更、zebra 可能丢 K 路由视图，只能等下一轮路由变化自愈）: %v", err)
		return
	}
	for {
		var evs []krt.LinkEvent
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			evs = append(evs, ev)
		}
		timer := time.NewTimer(linkSettle)
	collect:
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					break collect
				}
				evs = append(evs, ev)
			case <-timer.C:
				break collect
			case <-ctx.Done():
				timer.Stop()
				return
			}
		}
		timer.Stop()
		touched, terr := exp.touch(ctx, rt.snapshot().exportVRFs)
		total := 0
		for _, n := range touched {
			total += n
		}
		if terr != nil {
			log.Printf("sr-frr: 链路事件（%s）后重放 K 路由失败: %v", describeEvents(evs), terr)
			continue
		}
		if total > 0 {
			log.Printf("sr-frr: 链路事件（%s）后重放 %d 条 K 路由（zebra 会清掉自己的内核路由视图）",
				describeEvents(evs), total)
		}
	}
}

func describeEvents(evs []krt.LinkEvent) string {
	parts := make([]string, 0, len(evs))
	for _, ev := range evs {
		parts = append(parts, fmt.Sprintf("%s:%s", ev.Kind, ev.Name))
	}
	return strings.Join(parts, ",")
}

// importState is the read side's bookkeeping: connection, subscription
// signature, and the numbers /status reports.
type importState struct {
	mu       sync.Mutex
	cl       *frr.Client
	clSig    string
	lastWarn string
	lastErr  string
	reported int
	loop     int
	unknown  uint64
	subs     []string
	subSig   string
}

func (s *importState) setPoll(err error, reps int, loop int, unknown uint64, subs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reported = reps
	s.loop = loop
	s.unknown = unknown
	s.subs = subs
	if err != nil {
		s.lastErr = err.Error()
	} else {
		s.lastErr = ""
	}
}

func (s *importState) setWarn(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg != s.lastWarn {
		s.lastWarn = msg
		if msg != "" {
			log.Printf("sr-frr: %s", msg)
		}
	}
}

func (s *importState) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cl != nil {
		s.cl.Close()
		s.cl = nil
		s.clSig = ""
	}
}

type subSpec struct {
	VPN   uint8
	VRFID uint32
	Type  uint8
}

func subsSig(subs []subSpec) string {
	var b strings.Builder
	for _, s := range subs {
		fmt.Fprintf(&b, "%d:%d@%d,", s.VPN, s.Type, s.VRFID)
	}
	return b.String()
}

func subNames(subs []subSpec) []string {
	out := make([]string, 0, len(subs))
	for _, s := range subs {
		out = append(out, fmt.Sprintf("vpn%d/type%d@%d", s.VPN, s.Type, s.VRFID))
	}
	return out
}

// subsFor resolves the (VRF, source) pairs to subscribe and the ifindex→VPN
// map notifications will be attributed with. A VPN whose vpn<N> device does
// not exist is a warning: the rest still work, and the missing one shows up in
// /status instead of silently subscribing nothing.
func subsFor(rules []frr.ImportRule) ([]subSpec, map[uint32]uint8, []string) {
	vmap := map[uint32]uint8{0: 0}
	var subs []subSpec
	var warns []string
	for _, r := range rules {
		if len(r.Sources) == 0 {
			continue
		}
		id, err := krt.ImportIndex(r.VPN)
		if err != nil {
			warns = append(warns, fmt.Sprintf("VPN %d 的 VRF 设备不在，导入跳过：%v", r.VPN, err))
			continue
		}
		if prev, ok := vmap[id]; ok && prev != r.VPN {
			warns = append(warns, fmt.Sprintf("VPN %d 与 VPN %d 解析到同一个 vrf_id %d，后者被忽略", prev, r.VPN, id))
			continue
		}
		vmap[id] = r.VPN
		for _, t := range r.Sources {
			subs = append(subs, subSpec{VPN: r.VPN, VRFID: id, Type: t})
		}
	}
	sort.Slice(subs, func(i, j int) bool {
		if subs[i].VRFID != subs[j].VRFID {
			return subs[i].VRFID < subs[j].VRFID
		}
		return subs[i].Type < subs[j].Type
	})
	return subs, vmap, warns
}

// ensureImport reconnects when zebra died or the subscription set changed. A
// reconnect resets the importer: the new connection's dump is what this box has
// on the customer side, and nothing older than that connection is reported.
func ensureImport(ctx context.Context, st *importState, imp *frr.Importer, path string, subs []subSpec) error {
	sig := subsSig(subs)
	st.mu.Lock()
	cl := st.cl
	clSig := st.clSig
	st.mu.Unlock()
	if cl != nil && cl.Err() == nil && clSig == sig {
		return nil
	}
	if cl != nil {
		if err := cl.Err(); err != nil {
			log.Printf("sr-frr: 与 zebra 的连接断开（%v），重连", err)
		}
		cl.Close()
	}
	st.mu.Lock()
	st.cl = nil
	st.clSig = ""
	st.mu.Unlock()
	if len(subs) == 0 {
		return nil
	}
	c, err := frr.Dial(ctx, path)
	if err != nil {
		return err
	}
	imp.Reset()
	c.SetRouteHandler(imp.Handle)
	for _, s := range subs {
		if err := c.SubscribeRoutes(ctx, s.VRFID, s.Type); err != nil {
			c.Close()
			return fmt.Errorf("订阅 vpn%d type%d@%d: %w", s.VPN, s.Type, s.VRFID, err)
		}
	}
	st.mu.Lock()
	st.cl = c
	st.clSig = sig
	st.mu.Unlock()
	if len(subs) > 0 {
		log.Printf("sr-frr: 已订阅 %d 组（vpn/来源/ifindex）", len(subs))
	}
	return nil
}

func (s *importState) zebraConnected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cl != nil && s.cl.Err() == nil
}

// statusDoc is sr-route /status 里本进程用到的部分。
//
// rib 的**键是目标主表**（本机 import 命中了的 VPN）—— 要往客户内网通告的
// 就是这一份，条目里的 vrf_id 是来源 VPN（只决定标签，与本进程无关）。
type statusDoc struct {
	RIB map[string][]ribRoute `json:"rib"`
}

type ribRoute struct {
	Prefix   string   `json:"prefix"`
	NextHops []string `json:"next_hops"`
}

func fetchRIB(ctx context.Context, hc *http.Client, url string) ([]frr.RIBRoute, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	var doc statusDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", url, err)
	}
	var out []frr.RIBRoute
	for key, rs := range doc.RIB {
		table, err := strconv.Atoi(key)
		if err != nil || uint64(table) > 255 {
			return nil, fmt.Errorf("%s 的 RIB 键 %q 不是主表号（0..255）", url, key)
		}
		for _, r := range rs {
			out = append(out, frr.RIBRoute{VRF: uint8(table), Prefix: r.Prefix, NextHops: r.NextHops})
		}
	}
	return out, nil
}

// loadStale 读上一轮的状态文件。旧格式（ZAPI/1250）直接删掉：那些条目在
// zebra 的表里，用内核 netlink 既删不对也删不到，留着只会误导下一次对账。
func loadStale(path string) ([]frr.Route, error) {
	stale, err := frr.LoadState(path)
	if errors.Is(err, frr.ErrLegacyState) {
		log.Printf("sr-frr: %s 是旧版 ZAPI/1250 导出的状态文件，已退役；删文件，不碰 1250 表", path)
		return nil, frr.RemoveState(path)
	}
	return stale, err
}

func prefixesURL(status string) (string, error) {
	u, err := url.Parse(status)
	if err != nil {
		return "", err
	}
	u.Path = "/local-prefixes"
	u.RawQuery = ""
	return u.String(), nil
}

func main() {
	configPath := flag.String("config", "", "path to the sr-frr config JSON")
	once := flag.Bool("once", false, "收敛一次后退出（e2e / 手工验证用；不撤路由）")
	reset := flag.Bool("reset", false, "按状态文件撤掉上一轮写进内核的前缀后退出")
	flag.Parse()
	if *configPath == "" {
		log.Fatal("-config is required")
	}
	if *reset && *once {
		log.Fatal("-reset 与 -once 不能同时用")
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	rt := &runtimeStore{}
	initial, err := compileRuntime(cfg.ExportVRFs, cfg.ImportRules)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	rt.set(initial)

	stale, err := loadStale(cfg.StateFile)
	if err != nil {
		log.Fatalf("sr-frr: %v", err)
	}

	dp, err := krt.New()
	if err != nil {
		log.Fatalf("sr-frr: %v", err)
	}
	defer dp.Close()
	ex := frr.NewExporterFrom(dp, stale)
	exp := &exportState{dp: dp, ex: ex, lastSig: planSig(stale), plan: ex.Routes()}

	importURL, err := prefixesURL(cfg.StatusURL)
	if err != nil {
		log.Fatalf("sr-frr: status_url: %v", err)
	}
	if *reset {
		n := 0
		for _, r := range stale {
			if err := dp.RouteDelete(ctx, r); err != nil {
				log.Fatalf("sr-frr: 撤 %s（表 %d）: %v", r.Prefix, r.Table, err)
			}
			n++
		}
		if err := frr.RemoveState(cfg.StateFile); err != nil {
			log.Fatalf("sr-frr: 删状态文件: %v", err)
		}
		log.Printf("sr-frr: reset 撤掉 %d 条内核路由", n)
		if len(initial.rules) > 0 {
			clearImports(importURL)
		}
		return
	}

	needExport := len(initial.exportVRFs) > 0
	needImport := len(initial.rules) > 0
	if !*once && !needExport && !needImport && len(stale) == 0 {
		log.Fatal("sr-frr: export_vrfs 与 import_rules 都为空，无事可做；检查配置")
	}
	if (*once || needExport || needImport) && cfg.StatusURL == "" {
		log.Fatal("sr-frr: 需要 status_url（sr-route 的 /status）")
	}

	mapper := &vpnMapper{}
	imp := frr.NewImporter(mapper.lookup)
	st := &importState{}

	hc := &http.Client{Timeout: 5 * time.Second}
	lastImportSig := ""

	poll := func() {
		rc := rt.snapshot()

		// 先读后写：这一轮先问客户侧、先把本地前缀报出去，再决定注入什么。
		// 顺序反过来会把上一轮的本地集当成本轮的，sr-route 于是晚一拍才知道
		// "这条前缀是本盒自己的"，多出来的那一拍足够把远端那条装上隧道。
		subs, vmap, warns := subsFor(rc.rules)
		mapper.set(vmap)
		st.setWarn(strings.Join(warns, "；"))
		exported := exp.exportedSet(rc.exportVRFs)
		var ierr error
		if err := ensureImport(ctx, st, imp, cfg.ZServSocket, subs); err != nil {
			ierr = err
			log.Printf("sr-frr: 反向导入未连上 zebra: %v", err)
		} else if st.zebraConnected() {
			st.mu.Lock()
			cl := st.cl
			st.mu.Unlock()
			if zerr := cl.ZebraError(); zerr != nil {
				ierr = zerr
				log.Printf("sr-frr: %v", zerr)
			}
			reps, stats := imp.Report(rc.rules, exported)
			if sig := frr.Signature(reps); sig != lastImportSig {
				if err := pushLocalPrefixes(ctx, hc, importURL, reps); err != nil {
					ierr = err
					log.Printf("sr-frr: 上报客户侧前缀失败（下次重试）: %v", err)
				} else {
					lastImportSig = sig
					log.Printf("sr-frr: 客户侧前缀 %d 条 → sr-route（%d 个 VPN）", len(reps), vpnCount(reps))
				}
			}
			st.setPoll(ierr, stats.Reported, stats.LoopDropped, stats.UnknownVRF, subNames(subs))
		} else {
			if len(rc.rules) == 0 && lastImportSig != "" {
				// 规则被撤光：把上一份上报清掉，不等 zebra 重新通知。
				if err := pushLocalPrefixes(ctx, hc, importURL, nil); err != nil {
					log.Printf("sr-frr: 清空客户侧前缀失败: %v", err)
				} else {
					lastImportSig = ""
					log.Printf("sr-frr: import_rules 为空，已清空上报")
				}
			}
			st.setPoll(ierr, 0, 0, 0, subNames(subs))
		}

		if err := exp.reconcile(ctx, hc, rc, cfg.StatusURL, cfg.StateFile); err != nil {
			if ctx.Err() == nil {
				log.Printf("sr-frr: 本轮导出收敛失败（保留上次成功的计划）: %v", err)
			}
		}
	}

	if !*once {
		ln, err := listenControl(cfg.ControlSocket)
		if err != nil {
			log.Fatalf("sr-frr: %v", err)
		}
		srv := &http.Server{Handler: controlMux(rt, exp, st, cfg, ctx)}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("sr-frr: 控制口退出: %v", err)
			}
		}()
		defer func() {
			shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdown)
		}()
		log.Printf("sr-frr: 控制口 %s（PUT /config, GET /status）", cfg.ControlSocket)
	}

	if *once {
		// -once 就是一轮：读客户侧、上报、对账注入，然后退出。不等任何窗口
		// —— 注入是同步的 netlink 写，退出时内核里已经装好；zebra 通过组播
		// 自己学到，那是它的事。
		poll()
		return
	}

	go watchLinkEvents(ctx, exp, rt)

	interval := time.Duration(cfg.PollSec) * time.Second
	if len(initial.exportVRFs) > 0 {
		log.Printf("sr-frr: 每 %s 从 %s 收敛 %s 到内核；FRR 侧用 redistribute kernel",
			interval, cfg.StatusURL, exportList(initial.exportVRFs))
	}
	if len(initial.rules) > 0 {
		log.Printf("sr-frr: 反向导入已开：%d 个 VPN → %s", len(initial.rules), importURL)
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		poll()
		select {
		case <-ctx.Done():
			st.close()
			fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer fcancel()
			exp.mu.Lock()
			routes := exp.ex.Routes()
			exp.mu.Unlock()
			if len(routes) > 0 {
				log.Printf("sr-frr: 停止，撤掉自己下发的 %d 条内核路由", len(routes))
				exp.mu.Lock()
				ferr := exp.ex.Flush(fctx)
				exp.mu.Unlock()
				if ferr != nil {
					// 留着状态文件，下次启动按它对账。
					log.Printf("sr-frr: 退出清理未完成（状态文件已保留）: %v", ferr)
					return
				}
				if err := frr.RemoveState(cfg.StateFile); err != nil {
					log.Printf("sr-frr: 删状态文件: %v", err)
				}
			} else {
				log.Printf("sr-frr: 停止")
			}
			if len(rt.snapshot().rules) > 0 {
				if err := pushLocalPrefixes(fctx, hc, importURL, nil); err != nil {
					log.Printf("sr-frr: 退出时清空本地前缀失败: %v", err)
				}
			}
			return
		case <-tick.C:
		}
	}
}

func vpnCount(ps []frr.ReportedPrefix) int {
	seen := map[uint8]bool{}
	for _, p := range ps {
		seen[p.VPN] = true
	}
	return len(seen)
}

func clearImports(importURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pushLocalPrefixes(ctx, &http.Client{Timeout: 5 * time.Second}, importURL, nil); err != nil {
		log.Printf("sr-frr: 清空本地前缀失败: %v", err)
	}
}

// localPrefixJSON 与 sr-route PUT /local-prefixes 的请求体逐字段对应。
type localPrefixJSON struct {
	VRFID  uint8  `json:"vrf_id"`
	Prefix string `json:"prefix"`
}

type localPrefixesDoc struct {
	Prefixes []localPrefixJSON `json:"prefixes"`
}

func pushLocalPrefixes(ctx context.Context, hc *http.Client, url string, ps []frr.ReportedPrefix) error {
	doc := localPrefixesDoc{Prefixes: make([]localPrefixJSON, 0, len(ps))}
	for _, p := range ps {
		doc.Prefixes = append(doc.Prefixes, localPrefixJSON{VRFID: p.VPN, Prefix: p.Prefix.String()})
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("PUT %s: %s: %s", url, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// listenControl prepares the Unix socket. A stale file from a killed process is
// removed; the directory is created here because nothing else owns it.
func listenControl(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("删旧控制口 %s: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("监听 %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

type controlBody struct {
	ExportVRFs  json.RawMessage `json:"export_vrfs"`
	ImportRules json.RawMessage `json:"import_rules"`
	RefreshVRFs []uint32        `json:"refresh_vrfs"`
}

func controlMux(rt *runtimeStore, exp *exportState, st *importState, cfg *fileConfig, ctx context.Context) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "PUT required", http.StatusMethodNotAllowed)
			return
		}
		var body controlBody
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		if err := dec.Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if len(bytes.TrimSpace(body.ExportVRFs)) == 0 || len(bytes.TrimSpace(body.ImportRules)) == 0 {
			http.Error(w, "export_vrfs 与 import_rules 都必须在请求里（空数组表示没有）", http.StatusBadRequest)
			return
		}
		next, err := compileRuntime(body.ExportVRFs, body.ImportRules)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Touch only VPNs that stay exported: re-adding a prefix in a table the
		// new config no longer exports would just be deleted one poll later.
		var refresh []uint32
		keep := map[uint32]bool{}
		for _, v := range next.exportVRFs {
			keep[v] = true
		}
		for _, v := range body.RefreshVRFs {
			if keep[v] {
				refresh = append(refresh, v)
			}
		}
		touched, err := exp.touch(ctx, refresh)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var touchedList []uint32
		for v, n := range touched {
			touchedList = append(touchedList, v)
			log.Printf("sr-frr: touch vpn%d %d 条（协议集合变化，触发再分发重算）", v, n)
		}
		sort.Slice(touchedList, func(i, j int) bool { return touchedList[i] < touchedList[j] })
		rt.set(next)
		log.Printf("sr-frr: 配置热更 export_vrfs=%v import_rules=%d hash=%s",
			next.exportVRFs, len(next.rules), next.hash[:12])
		writeJSON(w, map[string]any{
			"config_hash":  next.hash,
			"refreshed":    touchedList,
			"export_vrfs":  next.exportVRFs,
			"import_rules": len(next.rules),
		})
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		rc := rt.snapshot()
		exp.mu.Lock()
		injected := len(exp.plan)
		skipped := make([]string, 0, len(exp.skipped))
		for _, sr := range exp.skipped {
			skipped = append(skipped, sr.Prefix.String())
		}
		exportErr := exp.lastErr
		exp.mu.Unlock()
		st.mu.Lock()
		reported, loop, unknown := st.reported, st.loop, st.unknown
		subs, ierr := append([]string(nil), st.subs...), st.lastErr
		st.mu.Unlock()
		errText := ierr
		if exportErr != "" {
			if errText != "" {
				errText = exportErr + "；" + errText
			} else {
				errText = exportErr
			}
		}
		rules := make([]map[string]any, 0, len(rc.rules))
		for _, ru := range rc.rules {
			srcs := make([]string, 0, len(ru.Sources))
			for _, t := range ru.Sources {
				srcs = append(srcs, frr.SourceName(t))
			}
			rules = append(rules, map[string]any{
				"vpn": ru.VPN, "sources": srcs, "prefixes": len(ru.Allow),
			})
		}
		writeJSON(w, map[string]any{
			"pid":                 os.Getpid(),
			"config_hash":         rc.hash,
			"export_vrfs":         rc.exportVRFs,
			"import_rules":        rules,
			"injected":            injected,
			"skipped":             skipped,
			"reported":            reported,
			"dropped_loop":        loop,
			"dropped_unknown_vrf": unknown,
			"subscriptions":       subs,
			"zebra_connected":     st.zebraConnected(),
			"last_error":          errText,
			"state_file":          cfg.StateFile,
			"control_socket":      cfg.ControlSocket,
		})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
