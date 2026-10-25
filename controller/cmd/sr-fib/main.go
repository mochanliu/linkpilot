// sr-fib 把 sr-route 的 RIB 收敛成 VPP 的转发状态：VRF 表、带标签路由、本地标签。
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
	"syscall"
	"time"

	"sdwan/controller/internal/fib"
	"sdwan/controller/internal/vppapi"
)

type fileConfig struct {
	StatusURL string         `json:"status_url"`
	VPPSocket string         `json:"vpp_socket"`
	PollSec   int            `json:"poll_sec"`
	VRFs      []uint8        `json:"vrfs"`
	Tunnels   []tunnelConfig `json:"tunnels"`
}

// tunnelConfig 是控制面分配的隧道映射。下一跳必须与 RIB 里的 next_hop 逐字相同。
type tunnelConfig struct {
	NextHop   string `json:"next_hop"`
	Interface string `json:"interface"`
}

const defaultVPPSocket = "/run/vpp/api.sock"

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
// 那个 JSON 还有会话统计等，与本进程无关。
type statusDoc struct {
	RIB map[string][]ribRoute `json:"rib"`
}

type ribRoute struct {
	VRFID   uint8  `json:"vrf_id"`
	Prefix  string `json:"prefix"`
	NextHop string `json:"next_hop"`
}

func fetchRIB(ctx context.Context, hc *http.Client, url string) ([]fib.RIBRoute, error) {
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
	for _, rs := range doc.RIB {
		for _, r := range rs {
			out = append(out, fib.RIBRoute{VRF: r.VRFID, Prefix: r.Prefix, NextHop: r.NextHop})
		}
	}
	return out, nil
}

func main() {
	configPath := flag.String("config", "", "path to the sr-fib config JSON")
	once := flag.Bool("once", false, "收敛一次后退出（e2e / 手工验证用）")
	flag.Parse()
	if *configPath == "" {
		log.Fatal("-config is required")
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

	tunnels := make([]fib.Tunnel, 0, len(cfg.Tunnels))
	for _, t := range cfg.Tunnels {
		tunnels = append(tunnels, fib.Tunnel{NextHop: t.NextHop, Interface: t.Interface})
	}
	applier := fib.NewApplier(vc)
	hc := &http.Client{Timeout: 5 * time.Second}
	var lastWarn string

	poll := func() error {
		routes, err := fetchRIB(ctx, hc, cfg.StatusURL)
		if err != nil {
			return fmt.Errorf("读 RIB: %w", err)
		}
		plan, err := fib.PlanFrom(cfg.VRFs, routes, tunnels)
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
	log.Printf("sr-fib: %d 个 VRF、%d 条隧道，每 %s 从 %s 收敛一次",
		len(cfg.VRFs), len(tunnels), interval, cfg.StatusURL)
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
