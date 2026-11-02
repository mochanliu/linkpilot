// sr-frr 把 sr-route 的 RIB 以**内核 K 路由**的形式写进盒子的对应 VPN 表，
// 让客户侧的路由进程（BGP/OSPF）用 `redistribute kernel` 把远端站点前缀再
// 分发进客户内网。
//
// 它和 sr-fib 是一对平行的"输出端"，但两者刻意不做同一件事：
//
//   - sr-fib 写 VPP：谁的流量、从哪条隧道出去、压什么标签 —— 这是转发。
//   - sr-frr 写内核：哪些前缀存在 —— 这是给客户路由看的事实。客户侧自己
//     决定怎么 redistribute、加什么 route-map。
//
// VPN 0 写内核 main（254），VPN N 写 table N、出口 vpn<N>（见 internal/frr/krt
// 与 internal/frr.KernelTable）。linux-nl 不镜像这类路由，VPP 完全看不到；
// zebra 把它们认成 K（distance 0）。FRR 侧需要在对应实例里配好
// `redistribute kernel`（可再套 route-map）。为什么不再走 ZAPI 的 TABLE 路
// 由：那条路会在 VPP 里镜像出一张全 dpo-drop 的死表（docs/o6e-leak-plan.md §1）。
//
// 退出与崩溃的账都算清楚：正常退出逐条撤掉再删状态文件；被强杀时状态文件
// 还在，下次启动先把它当基线做差量 —— 内核不会替我们收尾。
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
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"sdwan/controller/internal/frr"
	"sdwan/controller/internal/frr/krt"
)

type fileConfig struct {
	StatusURL   string        `json:"status_url"`
	ZServSocket string        `json:"zserv_socket"`
	StateFile   string        `json:"state_file"`
	PollSec     int           `json:"poll_sec"`
	ExportVRFs  []uint32      `json:"export_vrfs"`
	Import      *importConfig `json:"import"`

	// 已退役的导出键（ZAPI 的 table-direct 1250）。只用来识别旧配置：
	// 出现就告警并关掉导出，绝不按它们的值去写。
	LegacyVRFs     json.RawMessage `json:"vrfs"`
	LegacyDistance *int            `json:"distance"`
	LegacyMetric   *int            `json:"metric"`
}

// importConfig 打开反方向：把客户侧 BGP 学到的前缀上报给 sr-route。
type importConfig struct {
	VRFID       uint8  `json:"vrf_id"`
	PrefixesURL string `json:"prefixes_url"`
}

// importVRF 是 import 用的默认 FRR 实例（L-3 只搬导出方向；多 VRF 导入在 L-4）。
const importVRF uint8 = 0

const (
	defaultZServSocket = "/run/frr/zserv.api"
	defaultStateFile   = "/run/sr-frr/state.json"

	// onceSettle 只在 -once 里等一下：netlink ACK 表示内核已经装好，但 zebra
	// 是通过内核的 netlink 组播异步学到的，紧接着用 vtysh 看 `show ip route`
	// 可能还差一拍。
	onceSettle = 500 * time.Millisecond
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
	if cfg.PollSec == 0 {
		cfg.PollSec = 2
	}
	if cfg.PollSec < 0 {
		return nil, errors.New("poll_sec 不能为负")
	}
	if cfg.Import != nil && cfg.Import.PrefixesURL == "" {
		return nil, errors.New("import.prefixes_url is required（sr-route 的 PUT /local-prefixes）")
	}
	if cfg.LegacyVRFs != nil || cfg.LegacyDistance != nil || cfg.LegacyMetric != nil {
		if len(cfg.ExportVRFs) > 0 {
			return nil, errors.New("配置里同时出现退役的 vrfs/distance/metric 与 export_vrfs；删掉退役键再启动")
		}
		log.Printf("sr-frr: 配置里有已退役的键 vrfs/distance/metric（旧 ZAPI 1250 导出），已忽略并关闭导出；请改成 export_vrfs")
		cfg.ExportVRFs = nil
	}
	return &cfg, nil
}

// statusDoc 是 sr-route /status 里本进程用到的部分。
//
// rib 的**键是目标主表**（本机 import 命中了的 VPN）—— 要往客户内网通告的就是这一份，
// 条目里的 vrf_id 是来源 VPN（只决定标签，与本进程无关，所以不声明）。
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

	stale, err := loadStale(cfg.StateFile)
	if err != nil {
		log.Fatalf("sr-frr: %v", err)
	}

	// 反向导入只在常驻模式打开：-once 只跑一轮导出，订阅回放还没到齐就退出，
	// 反而会用一份不完整的集合清掉 sr-route 里上一轮的结论。
	var imp *frr.Importer
	importEnabled := cfg.Import != nil && !*once
	if cfg.Import != nil {
		imp = frr.NewImporter()
	}

	if *reset {
		dp, err := krt.New()
		if err != nil {
			log.Fatalf("sr-frr: %v", err)
		}
		defer dp.Close()
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
		clearImports(cfg.Import)
		return
	}

	if len(cfg.ExportVRFs) == 0 && !importEnabled {
		log.Fatal("sr-frr: export_vrfs 为空且没有 import，无事可做；检查配置")
	}

	var (
		dp *krt.Client
		ex *frr.Exporter
	)
	if len(cfg.ExportVRFs) > 0 {
		if cfg.StatusURL == "" {
			log.Fatal("sr-frr: 导出需要 status_url（sr-route 的 /status）")
		}
		// 先用空 RIB 跑一次校验：阈值/出口 ifindex 都在这里暴露，而不是等
		// 第一轮收敛时才发现。
		if _, err := frr.PlanFrom(cfg.ExportVRFs, nil, krt.Index); err != nil {
			log.Fatalf("config: %v", err)
		}
		dp, err = krt.New()
		if err != nil {
			log.Fatalf("sr-frr: %v", err)
		}
		defer dp.Close()
		// 崩溃残留就是内核里现在真实存在的集合：拿它当基线，第一次 Apply
		// 只补差量，同一条前缀不会先删后加。
		ex = frr.NewExporterFrom(dp, stale)
	}

	var cl *frr.Client
	connectImport := func() error {
		if cl != nil && cl.Err() == nil {
			return nil
		}
		if cl != nil {
			log.Printf("sr-frr: 与 zebra 的连接断开（%v），重连", cl.Err())
			cl.Close()
			cl = nil
		}
		c, err := frr.Dial(ctx, cfg.ZServSocket)
		if err != nil {
			return err
		}
		// 新连接上的回放会重建整份 BGP 视图；旧连接的条目不会再收到
		// 删除通知。这里不清空，而是开一个乐观窗口：旧集合继续对外可见，
		// 回放灌完（静默 500ms 或 10s 上限）后再一次性对账。
		imp.BeginResync()
		c.SetRouteHandler(imp.Handle)
		if err := c.SubscribeBGP(ctx); err != nil {
			c.Close()
			return fmt.Errorf("订阅 BGP 再分发: %w", err)
		}
		cl = c
		return nil
	}
	defer func() {
		if cl != nil {
			cl.Close()
		}
	}()

	hc := &http.Client{Timeout: 5 * time.Second}
	// 状态文件当前就代表 stale 这一份；计划与它一致时不必重写。
	lastSig := planSig(stale)
	lastImportSig := ""

	poll := func() error {
		if ex != nil {
			routes, err := fetchRIB(ctx, hc, cfg.StatusURL)
			if err != nil {
				return fmt.Errorf("读 RIB: %w", err)
			}
			plan, err := frr.PlanFrom(cfg.ExportVRFs, routes, krt.Index)
			if err != nil {
				return err
			}
			// 先落盘再下发：中途被杀时，文件里是"打算写的全部"，最多多删几条
			// 并不存在的路由，不会漏删。计划为空时删文件 —— 状态文件存在就代表
			// 我们留了东西。
			if sig := planSig(plan.Routes); sig != lastSig {
				if len(plan.Routes) == 0 {
					err = frr.RemoveState(cfg.StateFile)
				} else {
					err = frr.SaveState(cfg.StateFile, plan.Routes)
				}
				if err != nil {
					return fmt.Errorf("写状态文件: %w", err)
				}
				lastSig = sig
			}
			res, err := ex.Apply(ctx, plan)
			if err != nil {
				return err
			}
			if res.Changed() {
				log.Printf("sr-frr: 收敛 routes+%d/-%d（%s）", res.Added, res.Removed, exportList(cfg.ExportVRFs))
			}
		}

		if importEnabled {
			// 导入和导出是两条独立的路：zebra 没连上不该挡住 K 路由收敛。
			if err := connectImport(); err != nil {
				log.Printf("sr-frr: 反向导入未连上 zebra: %v", err)
				return nil
			}
			if zerr := cl.ZebraError(); zerr != nil {
				log.Printf("sr-frr: %v", zerr)
				return nil
			}
			// 窗口到期判定放在每一次收敛里：订阅回放是异步的，只有轮询
			// 这一侧知道"已经安静多久了"。
			imp.Tick(time.Now())
			ps := imp.Prefixes()
			if sig := importSig(cfg.Import.VRFID, ps); sig != lastImportSig {
				if err := pushLocalPrefixes(ctx, hc, cfg.Import, ps); err != nil {
					log.Printf("sr-frr: 上报客户侧前缀失败（下次重试）: %v", err)
					return nil
				}
				lastImportSig = sig
				log.Printf("sr-frr: 客户侧前缀 %d 条 → sr-route vrf %d", len(ps), cfg.Import.VRFID)
			}
		}
		return nil
	}

	if *once {
		if err := poll(); err != nil {
			log.Fatalf("sr-frr: %v", err)
		}
		time.Sleep(onceSettle)
		return
	}

	interval := time.Duration(cfg.PollSec) * time.Second
	if ex != nil {
		log.Printf("sr-frr: 每 %s 从 %s 收敛 %s 到内核；FRR 侧用 redistribute kernel（默认实例或 vpn<N>）",
			interval, cfg.StatusURL, exportList(cfg.ExportVRFs))
	}
	if importEnabled {
		log.Printf("sr-frr: 反向导入已开：BGP 前缀 → sr-route vrf %d（%s）",
			cfg.Import.VRFID, cfg.Import.PrefixesURL)
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := poll(); err != nil {
			// 失败不退出：sr-route 可能还没起来。Exporter 的基线保留着，
			// 下一轮成功后整份计划会重新对账。
			if ctx.Err() == nil {
				log.Printf("sr-frr: 本轮收敛失败（保留上次成功的计划）: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer fcancel()
			if ex != nil && len(ex.Routes()) > 0 {
				log.Printf("sr-frr: 停止，撤掉自己下发的 %d 条内核路由", len(ex.Routes()))
				if err := ex.Flush(fctx); err != nil {
					// 留着状态文件，下次启动按它对账。
					log.Printf("sr-frr: 退出清理未完成（状态文件已保留）: %v", err)
					return
				}
				if err := frr.RemoveState(cfg.StateFile); err != nil {
					log.Printf("sr-frr: 删状态文件: %v", err)
				}
			} else {
				log.Printf("sr-frr: 停止")
			}
			if importEnabled {
				if err := pushLocalPrefixes(fctx, hc, cfg.Import, nil); err != nil {
					log.Printf("sr-frr: 退出时清空本地前缀失败: %v", err)
				}
			}
			return
		case <-tick.C:
		}
	}
}

func clearImports(ic *importConfig) {
	if ic == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pushLocalPrefixes(ctx, &http.Client{Timeout: 5 * time.Second}, ic, nil); err != nil {
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

func importSig(vrf uint8, ps []netip.Prefix) string {
	var b strings.Builder
	b.WriteString(strconv.Itoa(int(vrf)))
	for _, p := range ps {
		b.WriteByte('|')
		b.WriteString(p.String())
	}
	return b.String()
}

func pushLocalPrefixes(ctx context.Context, hc *http.Client, ic *importConfig, ps []netip.Prefix) error {
	doc := localPrefixesDoc{Prefixes: make([]localPrefixJSON, 0, len(ps))}
	for _, p := range ps {
		doc.Prefixes = append(doc.Prefixes, localPrefixJSON{VRFID: ic.VRFID, Prefix: p.String()})
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, ic.PrefixesURL, bytes.NewReader(body))
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
		return fmt.Errorf("PUT %s: %s: %s", ic.PrefixesURL, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}
