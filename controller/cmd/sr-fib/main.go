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
	"syscall"
	"time"

	"sdwan/controller/internal/fib"
	"sdwan/controller/internal/vppapi"
)

type fileConfig struct {
	StatusURL string `json:"status_url"`
	VPPSocket string `json:"vpp_socket"`
	PollSec   int    `json:"poll_sec"`
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

// statusDoc 是 sr-route /status 里本进程用到的部分。刻意只声明需要的字段：
// 那个 JSON 还有会话统计、local 前缀等，与本进程无关 —— local 前缀由盒内
// FRR 经 linux-nl 直接装进 VPP，不经过 sr-fib。
//
// rib 的**键是目标主表**（本机 import 命中了的 VPN），条目的 vrf_id 是**来源** VPN：
// 导通时两者不同，标签取来源 VPN、装表取键（草案 §2.4/§4.3）。
type statusDoc struct {
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
	var doc statusDoc
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

	tunnels := fibTunnels(cfg.Tunnels)
	applier := fib.NewApplier(vc)
	hc := &http.Client{Timeout: 5 * time.Second}
	var lastWarn string

	poll := func() error {
		routes, err := fetchStatus(ctx, hc, cfg.StatusURL)
		if err != nil {
			return fmt.Errorf("读 RIB: %w", err)
		}
		// 可达性只认本机 VPP 里的 BFD 会话：它就是这个 CPE 眼里"隧道通不通"
		// 的权威答案，不用绕到北向去拿一份更旧的。
		sessions, err := vc.BFDSessions(ctx)
		if err != nil {
			return fmt.Errorf("读 BFD 会话: %w", err)
		}
		plan, err := fib.PlanFrom(cfg.VRFs, routes, tunnels, fib.UnreachablePeers(sessions), cfg.Escape)
		if err != nil {
			return err
		}
		// 下一跳不在隧道表里是配置问题，每次轮询都会重现；只在内容变化时打一次。
		if ws := fib.WarningsString(plan.Warnings); ws != lastWarn {
			if ws != "" {
				log.Printf("sr-fib: 警告: %s", ws)
			}
			lastWarn = ws
		}
		res, err := applier.Apply(ctx, plan)
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

	if *once {
		if err := poll(); err != nil {
			log.Fatalf("sr-fib: %v", err)
		}
		return
	}

	interval := time.Duration(cfg.PollSec) * time.Second
	log.Printf("sr-fib: %d 个 VRF、%d 条隧道，每 %s 从 %s 收敛一次（逃生开关=%v）",
		len(cfg.VRFs), len(tunnels), interval, cfg.StatusURL, cfg.Escape)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := poll(); err != nil {
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
