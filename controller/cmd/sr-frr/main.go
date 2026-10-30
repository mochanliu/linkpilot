// sr-frr 把 sr-route 的 RIB 导出成 FRR 的非主表路由，让客户侧的路由进程
// （BGP/OSPF）能把远端站点前缀再分发进客户内网。
//
// 它和 sr-fib 是一对平行的"输出端"，但两者刻意不做同一件事：
//
//   - sr-fib 写 VPP：谁的流量、从哪条隧道出去、压什么标签 —— 这是转发。
//   - sr-frr 写 FRR 的一张旁路表：哪些前缀存在 —— 这是给客户路由看的事实。
//     客户侧自己决定怎么 redistribute、加什么 route-map。
//
// 表号必须 > 255（见 internal/frr 的说明）。FRR 侧需要在客户实例里配好
// `redistribute table-direct <表号>`（BGP/OSPF 都可以），本进程只负责把
// 前缀放进那张表。
//
// 退出与崩溃的账都算清楚：正常退出逐条撤掉再删状态文件；被强杀时状态文件
// 还在，下次启动先把上一轮的残留撤干净。zebra 不会替客户端收尾。
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
)

type fileConfig struct {
	StatusURL   string        `json:"status_url"`
	ZServSocket string        `json:"zserv_socket"`
	StateFile   string        `json:"state_file"`
	PollSec     int           `json:"poll_sec"`
	Distance    *int          `json:"distance"`
	Metric      *int          `json:"metric"`
	VRFs        []vrfConfig   `json:"vrfs"`
	Import      *importConfig `json:"import"`
}

// importConfig 打开反方向：把客户侧 BGP 学到的前缀上报给 sr-route。
// tag 是客户侧邻居入方向 route-map 打的标记，用来区分"客户告诉我们的前缀"
// 和盒内自己产生/导出的 BGP 路由。
type importConfig struct {
	VRFID       uint8  `json:"vrf_id"`
	Tag         uint32 `json:"tag"`
	PrefixesURL string `json:"prefixes_url"`
}

// vrfConfig 的 vrf_id 是**控制面**的 VRF（sr-route 的编号），不是 FRR 的 VRF；
// 所有表都挂在 FRR 默认实例下，用 table_id 区分来源。
type vrfConfig struct {
	VRFID   uint8  `json:"vrf_id"`
	TableID uint32 `json:"table_id"`
}

const (
	defaultZServSocket = "/run/frr/zserv.api"
	defaultStateFile   = "/run/sr-frr/state.json"

	// settleDelay 是"写完就断开"之前留给 zebra RIB 队列的处理时间。
	// zebra 的 RIB 变更走异步队列；客户端如果在这之前断开，排队中的写入会
	// 随着客户端的下一跳组一起作废（实测：-once 立刻退出时一条都留不下）。
	// ZAPI 对 TABLE 路由没有可用的回执，所以这是时间换正确性。
	settleDelay = 500 * time.Millisecond
)

func settle() { time.Sleep(settleDelay) }

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
		return nil, errors.New("vrfs is required")
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
	if cfg.Distance == nil {
		d := 1
		cfg.Distance = &d
	}
	if *cfg.Distance < 1 || *cfg.Distance > 255 {
		return nil, errors.New("distance 必须在 1..255")
	}
	if cfg.Metric == nil {
		m := 20
		cfg.Metric = &m
	}
	if *cfg.Metric < 0 {
		return nil, errors.New("metric 不能为负")
	}
	if cfg.Import != nil {
		if cfg.Import.Tag == 0 {
			return nil, errors.New("import.tag 不能为 0（没有 tag 就无法区分客户路由与盒内导出的路由）")
		}
		if cfg.Import.PrefixesURL == "" {
			return nil, errors.New("import.prefixes_url is required（sr-route 的 PUT /local-prefixes）")
		}
	}
	return &cfg, nil
}

func (c *fileConfig) tables() ([]frr.Table, error) {
	out := make([]frr.Table, 0, len(c.VRFs))
	for _, v := range c.VRFs {
		out = append(out, frr.Table{VRF: v.VRFID, ID: v.TableID})
	}
	if err := frr.ValidateTables(out); err != nil {
		return nil, err
	}
	return out, nil
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
		b.WriteString(r.Prefix.String())
		b.WriteByte('|')
		b.WriteString(strconv.FormatUint(uint64(r.TableID), 10))
		b.WriteString("|")
		b.WriteString(strconv.FormatUint(uint64(r.Distance), 10))
		b.WriteString("|")
		b.WriteString(strconv.FormatUint(uint64(r.Metric), 10))
		b.WriteByte('\n')
	}
	return b.String()
}

func main() {
	configPath := flag.String("config", "", "path to the sr-frr config JSON")
	once := flag.Bool("once", false, "收敛一次后退出（e2e / 手工验证用；不撤路由）")
	reset := flag.Bool("reset", false, "按状态文件撤掉上一轮写进 FRR 的前缀后退出")
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
	tables, err := cfg.tables()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	distance := uint8(*cfg.Distance)
	metric := uint32(*cfg.Metric)

	// 反向导入只在常驻模式打开：-once 只跑一轮导出，订阅回放还没到齐就退出，
	// 反而会用一份不完整的集合清掉 sr-route 里上一轮的结论。
	var imp *frr.Importer
	importEnabled := cfg.Import != nil && !*once
	if cfg.Import != nil {
		imp = frr.NewImporter(cfg.Import.Tag)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *reset {
		stale, err := frr.LoadState(cfg.StateFile)
		if err != nil {
			log.Fatalf("sr-frr: %v", err)
		}
		if len(stale) == 0 {
			if err := frr.RemoveState(cfg.StateFile); err != nil {
				log.Fatalf("sr-frr: 删状态文件: %v", err)
			}
			log.Printf("sr-frr: 没有待撤状态（%s 不存在或为空）", cfg.StateFile)
			clearImports(cfg.Import)
			return
		}
		cl, err := frr.Dial(ctx, cfg.ZServSocket)
		if err != nil {
			log.Fatalf("sr-frr: %v", err)
		}
		defer cl.Close()
		n := 0
		for _, r := range stale {
			if err := cl.RouteDelete(ctx, r); err != nil {
				log.Fatalf("sr-frr: 撤 %s: %v", r.Prefix, err)
			}
			n++
		}
		settle()
		if err := frr.RemoveState(cfg.StateFile); err != nil {
			log.Fatalf("sr-frr: 删状态文件: %v", err)
		}
		log.Printf("sr-frr: reset 撤掉 %d 条前缀", n)
		clearImports(cfg.Import)
		return
	}

	var (
		cl *frr.Client
		ex *frr.Exporter
	)
	connect := func() error {
		if cl != nil && cl.Err() == nil {
			return nil
		}
		if cl != nil {
			// zebra 重启后旧连接会断；它的 RIB 也是空的，所以换一个
			// Exporter（prev 清零）把整份计划重发一遍。
			log.Printf("sr-frr: 与 zebra 的连接断开（%v），重连", cl.Err())
			cl.Close()
			cl, ex = nil, nil
		}
		c, err := frr.Dial(ctx, cfg.ZServSocket)
		if err != nil {
			return err
		}
		if importEnabled {
			// 新连接上的回放会重建整份 BGP 视图；旧连接的条目不会再收到
			// 删除通知，所以先清空，再让回放把它们重新加回来。
			imp.Reset()
			c.SetRouteHandler(imp.Handle)
			if err := c.SubscribeBGP(ctx); err != nil {
				c.Close()
				return fmt.Errorf("订阅 BGP 再分发: %w", err)
			}
		}
		cl, ex = c, frr.NewExporter(c)
		return nil
	}
	defer func() {
		if cl != nil {
			cl.Close()
		}
	}()

	// 崩溃残留必须在任何新写入之前清掉：上一轮的条目和这一轮的计划混在
	// 同一张表里，就再也分不出哪条该留了。
	if stale, err := frr.LoadState(cfg.StateFile); err != nil {
		log.Fatalf("sr-frr: %v", err)
	} else if len(stale) > 0 {
		if err := connect(); err != nil {
			log.Fatalf("sr-frr: 对账 %s: %v", cfg.StateFile, err)
		}
		for _, r := range stale {
			if err := cl.RouteDelete(ctx, r); err != nil {
				log.Fatalf("sr-frr: 对账撤 %s: %v", r.Prefix, err)
			}
		}
		if err := frr.RemoveState(cfg.StateFile); err != nil {
			log.Fatalf("sr-frr: 删状态文件: %v", err)
		}
		log.Printf("sr-frr: 启动对账撤掉上一轮残留 %d 条", len(stale))
	}

	hc := &http.Client{Timeout: 5 * time.Second}
	lastSig := ""
	lastImportSig := ""
	poll := func() error {
		if err := connect(); err != nil {
			return err
		}
		routes, err := fetchRIB(ctx, hc, cfg.StatusURL)
		if err != nil {
			return fmt.Errorf("读 RIB: %w", err)
		}
		plan, err := frr.PlanFrom(tables, routes, distance, metric)
		if err != nil {
			return err
		}
		// 先落盘再下发：中途被杀时，文件里是"打算写的全部"，最多多删几条
		// 并不存在的路由，不会漏删。计划为空时删文件 —— 状态文件存在就代表
		// 有人在 FRR 里留了东西。
		if sig := planSig(plan.Routes); sig != lastSig {
			var err error
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
		if zerr := cl.ZebraError(); zerr != nil {
			log.Printf("sr-frr: %v", zerr)
		}
		if res.Changed() {
			log.Printf("sr-frr: 收敛 routes+%d/-%d（表 %s）", res.Added, res.Removed, tableList(tables))
		}
		if importEnabled {
			ps := imp.Prefixes()
			if sig := importSig(cfg.Import.VRFID, ps); sig != lastImportSig {
				if err := pushLocalPrefixes(ctx, hc, cfg.Import, ps); err != nil {
					return fmt.Errorf("上报客户侧前缀: %w", err)
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
		settle()
		return
	}

	interval := time.Duration(cfg.PollSec) * time.Second
	log.Printf("sr-frr: 每 %s 从 %s 收敛到 FRR 表 %s；FRR 侧需 redistribute table-direct <表号>",
		interval, cfg.StatusURL, tableList(tables))
	if importEnabled {
		log.Printf("sr-frr: 反向导入已开：tag %d 的 BGP 前缀 → sr-route vrf %d（%s）",
			cfg.Import.Tag, cfg.Import.VRFID, cfg.Import.PrefixesURL)
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := poll(); err != nil {
			// 失败不退出：sr-route 可能还没起来，FRR 也偶尔在重启。重连后
			// 换一个新的 Exporter，整份计划会重发一遍。
			if ctx.Err() == nil {
				log.Printf("sr-frr: 本轮收敛失败（保留上次成功的计划）: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer fcancel()
			if ex != nil && len(ex.Routes()) > 0 {
				log.Printf("sr-frr: 停止，撤掉自己下发的 %d 条前缀", len(ex.Routes()))
				if err := ex.Flush(fctx); err != nil {
					// 留着状态文件，下次启动按它对账。
					log.Printf("sr-frr: 退出清理未完成（状态文件已保留）: %v", err)
					return
				}
				settle()
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

func tableList(tables []frr.Table) string {
	parts := make([]string, 0, len(tables))
	for _, t := range tables {
		parts = append(parts, fmt.Sprintf("vrf%d→%d", t.VRF, t.ID))
	}
	return strings.Join(parts, ",")
}
