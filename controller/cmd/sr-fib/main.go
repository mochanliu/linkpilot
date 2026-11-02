// sr-fib 把 sr-route 的 RIB 收敛成 VPP 的转发状态：VRF 表、带标签路由、本地标签。
// 同时读本机 VPP 的 BFD 会话，把明确不 Up 的隧道从路径集合里剔除 —— 可达性信息
// 与路由同源（都在这台 CPE 上），不在两个进程之间另造一套状态。
//
// 单独一个二进制、单独一次显式下发，是刻意的：sr-route 只维护 RIB（反射、策略、
// 双 RR 切换都在那边），FIB 是控制面的**输出**。同一台 CPE 上，sr-fib 是唯一写
// 这三样东西的进程 —— 出问题时先看它，不用在 sr-route 的 goroutine 里找。
//
// 每次轮询都重算整份计划再与上次成功的计划比差异（见 internal/fib）：RIB 是声明
// 式快照，增量下发会留下已经撤掉的路径。
//
// 运行中改 VPN 集合/隧道表/逃生开关走 `PUT /config`，不重启进程（D3）。它和
// sr-route 的 `PUT /vrfs` 是同一种形状：整份替换、未知字段当场报错、校验不过
// 就不接。区别只有一处：这份配置**不写盘**——磁盘上的 fib.json 归安装/运维，
// 进程只负责把收到的这份变成数据面。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"sdwan/controller/internal/fib"
	"sdwan/controller/internal/vppapi"
)

type fileConfig struct {
	StatusURL string `json:"status_url"`
	VPPSocket string `json:"vpp_socket"`
	PollSec   int    `json:"poll_sec"`
	// StatusAddr 是本进程自己的热更入口（GET /status、PUT /config）。空 = 不开，
	// 与 sr-route 的 status_addr 同一个键名、同一套写法；老 fib.json 不写它照样能跑。
	StatusAddr string `json:"status_addr"`
	// Escape 是草案 §3.1 的 escape_all_colors（§5.2 逃生开关），租户级。
	// 键名与草案逐字一致：读配置用了 DisallowUnknownFields，将来控制器
	// 打包（O6）时两边名字对不上会当场报错，而不是静默按默认关跑。
	Escape  bool           `json:"escape_all_colors"`
	VRFs    []uint8        `json:"vrfs"`
	Tunnels []tunnelConfig `json:"tunnels"`
}

// tunnelConfig 是控制面分配的隧道映射。下一跳必须与 RIB 里 next_hops 的某个元素逐字相同。
//
// Color 是这条隧道的 SD-WAN 颜色：它决定这条下一跳的 VPP preference（10+颜色），
// 颜色 0 最优、颜色 0 全 down 时由 VPP 自己抬到存活的最小颜色。缺省 0，所以现存的
// 配置文件不必改就能继续跑。
type tunnelConfig struct {
	NextHop   string `json:"next_hop"`
	Interface string `json:"interface"`
	Color     uint8  `json:"color,omitempty"`
}

const defaultVPPSocket = "/run/vpp/api.sock"

// fibTunnels is the one place the box's own tunnel table becomes FIB input. It is
// a function because a color that gets dropped here fails silently: the paths
// still install, every one of them at preference 10.
func fibTunnels(cfgs []tunnelConfig) []fib.Tunnel {
	out := make([]fib.Tunnel, 0, len(cfgs))
	for _, t := range cfgs {
		out = append(out, fib.Tunnel{NextHop: t.NextHop, Interface: t.Interface, Color: t.Color})
	}
	return out
}

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
	if cfg.StatusURL == "" {
		return nil, errors.New("status_url is required (sr-route 的 /status)")
	}
	if len(cfg.VRFs) == 0 {
		return nil, errors.New("vrfs is required (本机服务的 VPN VRF，与 sr-route 的配置同源)")
	}
	if cfg.VPPSocket == "" {
		cfg.VPPSocket = defaultVPPSocket
	}
	if cfg.PollSec == 0 {
		cfg.PollSec = 2
	}
	if cfg.PollSec < 0 {
		return nil, errors.New("poll_sec 不能为负")
	}
	return &cfg, nil
}

// liveConfig 是运行中可变的那半份配置。status_url / vpp_socket / poll_sec /
// status_addr 是进程级参数，换个值要有新进程，不在这里。
type liveConfig struct {
	VRFs    []uint8
	Tunnels []fib.Tunnel
	Escape  bool
}

func (cfg *fileConfig) live() liveConfig {
	return liveConfig{VRFs: cfg.VRFs, Tunnels: fibTunnels(cfg.Tunnels), Escape: cfg.Escape}
}

// validateLive 用同一个纯函数校验一份表。没有路由时 PlanFrom 只做形状检查：
// VRF 集合非空、250 保留、不重复；隧道是规范 IPv4、有接口名、颜色不超上限、
// 同一对端只有一条。校验不过就整份不接 —— 这里读错一个字段的代价不是报错，
// 而是某个 VPN 静默不转发。
func validateLive(c liveConfig) error {
	_, err := fib.PlanFrom(c.VRFs, nil, c.Tunnels, nil, c.Escape)
	return err
}

// configDoc 是 GET/PUT /config 的体：与 fib.json 里会变的那三个键逐字同名。
// 整份替换 —— 省略 vrfs 等于"一个 VPN 都不服务"（会被校验拒绝），省略 tunnels
// 等于空隧道表。
//
// VRFs 用 []int 而不是 []uint8：Go 把 []uint8 当成 []byte，编码成 base64 字符串
// （"AA=="），而磁盘上的 fib.json 和 sr-route 的 /vrfs 都是 [0,100] 这种数组。
// 同一份配置在两条通道上形态不同，排障时一定有人读错。
type configDoc struct {
	VRFs            []int          `json:"vrfs"`
	Tunnels         []tunnelConfig `json:"tunnels"`
	EscapeAllColors bool           `json:"escape_all_colors"`
}

func configOf(c liveConfig) configDoc {
	vs := make([]int, 0, len(c.VRFs))
	for _, v := range c.VRFs {
		vs = append(vs, int(v))
	}
	ts := make([]tunnelConfig, 0, len(c.Tunnels))
	for _, t := range c.Tunnels {
		ts = append(ts, tunnelConfig{NextHop: t.NextHop, Interface: t.Interface, Color: t.Color})
	}
	return configDoc{VRFs: vs, Tunnels: ts, EscapeAllColors: c.Escape}
}

func (d configDoc) live() (liveConfig, error) {
	vs := make([]uint8, 0, len(d.VRFs))
	for _, v := range d.VRFs {
		if v < 0 || v > int(fib.MaxVRFID) {
			return liveConfig{}, fmt.Errorf("fib: VRF %d 超出 0..%d（VPN 号在协议里是 8 位）", v, fib.MaxVRFID)
		}
		vs = append(vs, uint8(v))
	}
	ts := make([]fib.Tunnel, 0, len(d.Tunnels))
	for _, t := range d.Tunnels {
		ts = append(ts, fib.Tunnel{NextHop: t.NextHop, Interface: t.Interface, Color: t.Color})
	}
	c := liveConfig{VRFs: vs, Tunnels: ts, Escape: d.EscapeAllColors}
	if err := validateLive(c); err != nil {
		return liveConfig{}, err
	}
	return c, nil
}

// lastApply 是一次收敛的记账。/status 带着它，PUT /config 的应答里也是它：
// "200 已收敛"要能被调用方当场核对，而不是只有一个状态码。
type lastApply struct {
	Converged     bool     `json:"converged"`
	AtUnix        int64    `json:"at_unix"`
	TablesAdded   int      `json:"tables_added"`
	LabelsSet     int      `json:"labels_set"`
	LabelsRemoved int      `json:"labels_removed"`
	RoutesSet     int      `json:"routes_set"`
	RoutesRemoved int      `json:"routes_removed"`
	MPLSChanged   bool     `json:"mpls_changed"`
	Error         string   `json:"error,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

func reportOf(res fib.Result, warnings []string, err error) *lastApply {
	r := &lastApply{
		Converged:     err == nil,
		AtUnix:        time.Now().Unix(),
		TablesAdded:   res.TablesAdded,
		LabelsSet:     res.LabelsSet,
		LabelsRemoved: res.LabelsRemoved,
		RoutesSet:     res.RoutesSet,
		RoutesRemoved: res.RoutesRemoved,
		MPLSChanged:   res.MPLSChanged,
		Warnings:      warnings,
	}
	if err != nil {
		r.Error = err.Error()
	}
	return r
}

// statusDoc 是 GET /status 的体：当前生效的配置 + 上一次收敛的结果。
type statusDoc struct {
	configDoc
	StatusURL string     `json:"status_url"`
	Last      *lastApply `json:"last,omitempty"`
}

// bfdDP 是收敛要用到的那一小块 VPP 面：fib.DP 加上本机 BFD 会话。测试用内存
// 假端替换它，收敛逻辑本身不依赖 VPP。
type bfdDP interface {
	fib.DP
	BFDSessions(ctx context.Context) ([]vppapi.BFDSession, error)
}

// fiber 是收敛器。cur 与 last 是原子的（状态口随时可读，不被一轮收敛堵住），
// mu 只把"换配置 + 收敛"和轮询循环串成一条线 —— Applier 的文档写明它只允许
// 单线程调用。
type fiber struct {
	mu        sync.Mutex
	cur       atomic.Pointer[liveConfig]
	last      atomic.Pointer[lastApply]
	lastWarn  string // 只在 mu 内读写
	dp        bfdDP
	applier   *fib.Applier
	hc        *http.Client
	statusURL string
}

// converge 按当前配置重算整份计划并下发。调用者必须持有 mu。
func (f *fiber) converge(ctx context.Context) error {
	routes, err := fetchStatus(ctx, f.hc, f.statusURL)
	if err != nil {
		return f.note(fib.Result{}, fmt.Errorf("读 RIB: %w", err))
	}
	// 可达性只认本机 VPP 里的 BFD 会话：它就是这个 CPE 眼里"隧道通不通"
	// 的权威答案，不用绕到北向去拿一份更旧的。
	sessions, err := f.dp.BFDSessions(ctx)
	if err != nil {
		return f.note(fib.Result{}, fmt.Errorf("读 BFD 会话: %w", err))
	}
	cfg := *f.cur.Load()
	plan, err := fib.PlanFrom(cfg.VRFs, routes, cfg.Tunnels, fib.UnreachablePeers(sessions), cfg.Escape)
	if err != nil {
		return f.note(fib.Result{}, err)
	}
	// 下一跳不在隧道表里是配置问题，每次轮询都会重现；只在内容变化时打一次。
	if ws := fib.WarningsString(plan.Warnings); ws != f.lastWarn {
		if ws != "" {
			log.Printf("sr-fib: 警告: %s", ws)
		}
		f.lastWarn = ws
	}
	res, err := f.applier.Apply(ctx, plan)
	f.last.Store(reportOf(res, plan.Warnings, err))
	if err != nil {
		return err
	}
	if res.Changed() || res.MPLSChanged {
		log.Printf("sr-fib: 收敛 tables+%d labels+%d/-%d routes+%d/-%d mpls-egress=%v",
			res.TablesAdded, res.LabelsSet, res.LabelsRemoved,
			res.RoutesSet, res.RoutesRemoved, res.MPLSChanged)
	}
	return nil
}

// note 记一次没走到下发的失败：状态口要能说出"这一轮为什么没成"。
func (f *fiber) note(res fib.Result, err error) error {
	f.last.Store(reportOf(res, nil, err))
	return err
}

// update 换掉配置并立刻收敛一次。失败不回滚：这份配置本身合法，失败的是一轮
// 数据面写入（sr-route 还没起、VPP 忙），轮询下一轮按同一份再试。应答因此是
// 202 + last.error —— "已接受、还没收敛"，不写成完成。
func (f *fiber) update(ctx context.Context, next liveConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cur.Store(&next)
	log.Printf("sr-fib: PUT /config：VRF %v、隧道 %d 条、逃生开关=%v（只改内存，磁盘上的 fib.json 没动）",
		next.VRFs, len(next.Tunnels), next.Escape)
	return f.converge(ctx)
}

func (f *fiber) status() statusDoc {
	return statusDoc{
		configDoc: configOf(*f.cur.Load()),
		StatusURL: f.statusURL,
		Last:      f.last.Load(),
	}
}

// statusDoc 是 sr-route /status 里本进程用到的部分。刻意只声明需要的字段：
// 那个 JSON 还有会话统计、local 前缀等，与本进程无关 —— local 前缀由盒内
// FRR 经 linux-nl 直接装进 VPP，不经过 sr-fib。
//
// rib 的**键是目标主表**（本机 import 命中了的 VPN），条目的 vrf_id 是**来源** VPN：
// 导通时两者不同，标签取来源 VPN、装表取键（草案 §2.4/§4.3）。
type routeStatusDoc struct {
	RIB map[string][]ribRoute `json:"rib"`
}

type ribRoute struct {
	DeviceID uint16   `json:"device_id"`
	VRFID    uint8    `json:"vrf_id"` // 来源 VPN
	Prefix   string   `json:"prefix"`
	NextHops []string `json:"next_hops"`
}

func fetchStatus(ctx context.Context, hc *http.Client, url string) ([]fib.RIBRoute, error) {
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
	var doc routeStatusDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", url, err)
	}
	var out []fib.RIBRoute
	for key, rs := range doc.RIB {
		table, err := strconv.Atoi(key)
		if err != nil || uint64(table) > 255 {
			return nil, fmt.Errorf("%s 的 RIB 键 %q 不是主表号（0..255）", url, key)
		}
		for _, r := range rs {
			out = append(out, fib.RIBRoute{
				Table:     uint8(table),
				SourceVRF: r.VRFID,
				Origin:    r.DeviceID,
				Prefix:    r.Prefix,
				NextHops:  r.NextHops,
			})
		}
	}
	// map 迭代顺序是随机的，排一下：本进程的日志与每轮计划比较都靠稳定输入。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		if out[i].Origin != out[j].Origin {
			return out[i].Origin < out[j].Origin
		}
		return out[i].Prefix < out[j].Prefix
	})
	return out, nil
}

func main() {
	configPath := flag.String("config", "", "path to the sr-fib config JSON")
	once := flag.Bool("once", false, "收敛一次后退出（e2e / 手工验证用）")
	reset := flag.Bool("reset", false, "撤掉配置里这些 VRF 的 FIB 状态（路由/标签/表）后退出")
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
	live := cfg.live()
	if err := validateLive(live); err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	vc, err := vppapi.Dial(ctx, cfg.VPPSocket, "sr-fib")
	if err != nil {
		log.Fatalf("vpp: %v", err)
	}
	defer vc.Close()

	if *reset {
		res, err := fib.Reset(ctx, vc, cfg.VRFs)
		if err != nil {
			log.Fatalf("sr-fib: reset: %v", err)
		}
		log.Printf("sr-fib: reset 撤 routes=%d labels=%d tables=%d",
			res.RoutesRemoved, res.LabelsRemoved, res.TablesRemoved)
		return
	}

	f := &fiber{
		dp:        vc,
		applier:   fib.NewApplier(vc),
		hc:        &http.Client{Timeout: 5 * time.Second},
		statusURL: cfg.StatusURL,
	}
	f.cur.Store(&live)

	if *once {
		if err := f.converge(ctx); err != nil {
			log.Fatalf("sr-fib: %v", err)
		}
		return
	}

	if cfg.StatusAddr != "" {
		go serveStatus(cfg.StatusAddr, f)
	} else {
		log.Printf("sr-fib: 配置里没有 status_addr，运行中不能热更（VPN 集合/隧道表/逃生开关要换值得重启）")
	}

	interval := time.Duration(cfg.PollSec) * time.Second
	log.Printf("sr-fib: %d 个 VRF、%d 条隧道，每 %s 从 %s 收敛一次（逃生开关=%v）",
		len(live.VRFs), len(live.Tunnels), interval, cfg.StatusURL, live.Escape)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := f.converge(ctx); err != nil {
			// 失败不退出：sr-route 可能还没起来，VPP 也偶尔在重启。已下发成功的
			// 那份计划仍是基线，下一轮按同一目标重试。
			log.Printf("sr-fib: 本轮收敛失败（保留上次成功的计划）: %v", err)
		}
		select {
		case <-ctx.Done():
			log.Printf("sr-fib: 停止")
			return
		case <-tick.C:
		}
	}
}

// updateTimeout 是一份 PUT 等着收敛完的预算：一次收敛要读 sr-route 的 /status
// （5s 超时）+ BFD 会话 + 下发。超过这个数就答 202 "已接受、尚未收敛"，由轮询
// 接手；那份配置不会丢。
const updateTimeout = 10 * time.Second

// writeJSON / writeJSONStatus / decodeJSON 与 sr-route 的同名函数同一个口径：
// 应答带缩进便于人读，请求体按配置文件的严格度解析（未知字段当场 400）。
func writeJSON(w http.ResponseWriter, v any) {
	writeJSONStatus(w, http.StatusOK, v)
}

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		log.Printf("sr-fib: encode status: %v", err)
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// configMux is split out of serveStatus so the seam can be driven by httptest.
func configMux(f *fiber) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, f.status())
	})
	// PUT replaces the whole live table (vrfs + tunnels + escape switch) and
	// converges once before answering. 200 means the data plane now matches the
	// body; 202 means it was accepted but this round failed (sr-route not up, VPP
	// busy) and the poll loop will retry -- last.error says why, never "done".
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, configOf(*f.cur.Load()))
		case http.MethodPut:
			var doc configDoc
			if !decodeJSON(w, r, &doc) {
				return
			}
			next, err := doc.live()
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			// 客户端断开不取消这次收敛：配置已经接下了，就必须真的写进数据面。
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), updateTimeout)
			defer cancel()
			code := http.StatusOK
			if err := f.update(ctx, next); err != nil {
				code = http.StatusAccepted
			}
			writeJSONStatus(w, code, f.status())
		default:
			http.Error(w, "GET or PUT only", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func serveStatus(addr string, f *fiber) {
	log.Printf("sr-fib: status on http://%s/status（运行时改表入口 PUT /config）", addr)
	if err := http.ListenAndServe(addr, configMux(f)); err != nil {
		log.Printf("sr-fib: status server: %v", err)
	}
}
