package main

// 运维台账：历史落盘、告警、事件、审计——四样共用同一条 append-only 流水。
//
// 为什么是一条流水而不是四张表：监控管理模块要回答的问题是同一个——"这台网络
// 昨晚到底发生了什么"。设备失联、配置发布、选路切换、告警升起、谁点的发布，
// 都按时间排在一起才读得通；拆成四份存储只会让排障的人来回翻页。台账页按
// kind 过滤，审计与事件在数据上是同一行的两种读法。
//
// 存储：按天一个 JSONL 文件（<dataDir>/opslog/YYYY-MM-DD.jsonl），追加写，
// 原子性靠"一行一次 write + 换行"——崩溃最多丢半行，读侧跳过坏行即可。不用
// sqlite：控制器的 State 还是 fileStore（F1 未做），台账先于主库上数据库没有
// 意义；JSONL 每天一个文件也让清理变成"删旧文件"。
//
// 与遥测实时窗口的关系：postMetrics 存的 s.tel 仍然是"此刻"的事实（见
// telemetry.go 的文件头），台账里的 tunnel.sample 是另一回事——它是历史，
// 服务"过去 24 小时这条隧道质量怎么样"，两个读者两个问题，互不替代。
//
// 并发约定：所有 *Locked 告警函数要求持有 s.mu（调用点都在持锁路径上）；
// opsLedger 自己有锁，append 从任何上下文都能调。测试直接构造 server{}
// 时 s.ops 为 nil——所有入口对 nil 安全，测试不用知道台账的存在。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sdwan/controller/internal/wire"
)

const (
	// opsSampleThrottleSec 是 tunnel.sample 的落盘节流：agent 每个遥测窗口
	// 报一次（探针 300ms 一发，窗口聚合），窗口比这短没有画图价值，还把文件
	// 写爆。10s 一条，一台设备一天 8640 行，7 天也是小文件。
	opsSampleThrottleSec = 10
	// opsDefaultRetentionDays 是台账文件的默认保留期。实际值由
	// State.LedgerRetentionDays 决定（台账与告警页可改），超期文件由每天
	// 一次的清理删掉。
	opsDefaultRetentionDays = 30
	// opsMinRetentionDays / opsMaxRetentionDays 是允许设置的区间。下界 1 天
	// 是"至少留下今天和昨天能对比"的底线；上界 365 天是因为更久的台账本来
	// 也没人翻，而按天一个文件的管理成本已经开始明显。
	opsMinRetentionDays = 1
	opsMaxRetentionDays = 365
	// opsDefaultLossPermille 是隧道丢包告警的默认门槛（‰）。可在
	// /api/alerts/settings 按租户改。
	opsDefaultLossPermille = 200
	// opsLossStrikes 是连续多少个窗口超门槛才算真劣化：探针窗口本身就有
	// 波动，单窗口超标常见于抖动，连续两个窗口才值得叫人。
	opsLossStrikes = 2
	// telemetryGapSec 是"设备在线但遥测断了"的判定线：心跳还在（没 stale）
	// 但遥测窗口超过这么久没刷新——多半是探针插件掉了或 agent 半死，值得
	// 单独一条告警，因为"设备在线"会把它掩盖掉。
	telemetryGapSec = 180
)

// opsEntry 是台账的一行。Kind 的全集：
//
//	node.enrolled   设备入网
//	node.online     设备从失联恢复（或首次上线）
//	node.stale      设备失联
//	config.publish  发布设备配置（含策略/泄露——它们走同一条发布路径）
//	underlay.publish 发布 underlay 全局配置
//	underlay.change 增/改/删隧道
//	selection       动态策略行的选路颜色发生变化
//	tunnel.sample   遥测历史采样（节流 10s，只进台账，SSE 不发）
//	alert.raise     告警升起
//	alert.clear     告警恢复
//	audit           控制台写操作（actor=登录名；中间件自动记录）
type opsEntry struct {
	At     int64           `json:"at_unix"`
	Kind   string          `json:"kind"`
	Tenant string          `json:"tenant,omitempty"`
	Node   uint16          `json:"node,omitempty"`
	Actor  string          `json:"actor,omitempty"` // 控制台登录名；空 = 系统自身
	Sev    string          `json:"sev,omitempty"`   // info | warn | crit
	Msg    string          `json:"msg"`
	Data   json.RawMessage `json:"data,omitempty"` // tunnel.sample 的明细行
}

// ------------------------------------------------------------------ ledger

type opsLedger struct {
	mu  sync.Mutex
	dir string
	day string
	f   *os.File
}

func openOpsLedger(dir string) (*opsLedger, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &opsLedger{dir: dir}, nil
}

// append 追加一行。文件按天轮换（本地时区）：轮换判定在写之前做一次即可，
// 跨天后的第一条记录触发换文件，不必定时器盯着零点。
func (l *opsLedger) append(e opsEntry) {
	if l == nil {
		return // 测试里没有台账的 server：静默跳过，不假装记了
	}
	if e.At == 0 {
		e.At = time.Now().Unix()
	}
	day := time.Unix(e.At, 0).Format("2006-01-02")
	b, err := json.Marshal(e)
	if err != nil {
		return // Data 是我们自己组的 JSON，编不进去的行不值得丢一整条
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.day != day || l.f == nil {
		if l.f != nil {
			l.f.Close()
			l.f = nil
		}
		f, err := os.OpenFile(filepath.Join(l.dir, day+".jsonl"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			// 台账写不进去不能拖垮控制面：记一次日志，之后每条都重试打开。
			log.Printf("opslog: open %s: %v", day, err)
			return
		}
		l.day = day
		l.f = f
	}
	if _, err := l.f.Write(append(b, '\n')); err != nil {
		log.Printf("opslog: write: %v", err)
	}
}

// query 读时间窗内的行（按天扫文件），按 At 升序返回。kinds 为空 = 不过滤；
// tenant 非空 = 只看该租户（租户会话的硬边界）；坏行跳过——半行崩溃残留
// 不该让整个台账查询 500。
func (l *opsLedger) query(from, to int64, kinds map[string]bool, tenant string, node uint16) []opsEntry {
	out := []opsEntry{}
	if l == nil {
		return out
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	toDay := time.Unix(to, 0).Format("2006-01-02")
	for day := time.Unix(from, 0).Format("2006-01-02"); day <= toDay; day = advanceDay(day) {
		f, err := os.Open(filepath.Join(l.dir, day+".jsonl"))
		if err != nil {
			continue // 没有这一天：不是错误，只是还没有东西可读
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			var e opsEntry
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			if e.At < from || e.At > to {
				continue
			}
			if len(kinds) > 0 && !kinds[e.Kind] {
				continue
			}
			if tenant != "" && e.Tenant != tenant {
				continue
			}
			if node != 0 && e.Node != node {
				continue
			}
			out = append(out, e)
		}
		f.Close()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}

// advanceDay 把 YYYY-MM-DD 加一天。自己写是因为 time.ParseDate 之后加 24h
// 会撞上夏令时——本地时区没有夏令时的地方两种写法等价，有夏令时的地方
// 直接加天数才是对的。
func advanceDay(day string) string {
	t, err := time.ParseInLocation("2006-01-02", day, time.Local)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, 1).Format("2006-01-02")
}

// prune 删掉超过保留期的台账文件。每天调一次（调用方判日期变化）。
func (l *opsLedger) prune(now time.Time, keepDays int) {
	if l == nil || keepDays <= 0 {
		return
	}
	cut := now.AddDate(0, 0, -keepDays).Format("2006-01-02")
	ents, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	for _, de := range ents {
		name := de.Name()
		if !strings.HasSuffix(name, ".jsonl") || len(name) < 15 {
			continue
		}
		if name[:10] < cut {
			if err := os.Remove(filepath.Join(l.dir, name)); err != nil {
				log.Printf("opslog: prune %s: %v", name, err)
			}
		}
	}
}

// pruneLedgerIfDueLocked 每天清一次台账，另外保留期一改也立刻清一次——用户
// 在页面上把 30 天改成 3 天，不该等到下一个零点才看到效果。必须持有 s.mu
// （调用方 2s sweeper 本来就持着）。
func (s *server) pruneLedgerIfDueLocked(now int64) {
	days := s.ledgerRetentionDaysLocked()
	day := time.Unix(now, 0).Format("2006-01-02")
	if day == s.opsPruneDay && days == s.opsPruneDays {
		return
	}
	s.opsPruneDay, s.opsPruneDays = day, days
	s.ops.prune(time.Unix(now, 0), days)
}

// ------------------------------------------------------------------ alerts

// alertRecord 是一条活动告警。内存态，控制器重启后由规则重新评估出来
// （该响的下一拍就响），不做持久化——持久化的是 raise/clear 两条台账。
type alertRecord struct {
	Kind     string `json:"kind"`
	Tenant   string `json:"tenant"`
	Node     uint16 `json:"node,omitempty"`
	Ident    string `json:"ident,omitempty"` // kind 内的细分对象：对端节点号、策略行号
	Sev      string `json:"sev"`
	Msg      string `json:"msg"`
	RaisedAt int64  `json:"raised_at_unix"`
	LastAt   int64  `json:"last_at_unix"`
}

// alertKey 告警的幂等键：同一种问题同一条对象只占一条活动告警，
// 反复发生只刷新 LastAt，不刷屏。
func alertKey(kind, tenant string, node uint16, ident string) string {
	return fmt.Sprintf("%s|%s|%d|%s", kind, tenant, node, ident)
}

// appendOps 记一条台账。入口对 s.ops == nil 安全（见文件头）。
func (s *server) appendOps(kind, tenant string, node uint16, actor, sev, msg string) {
	s.ops.append(opsEntry{Kind: kind, Tenant: tenant, Node: node, Actor: actor, Sev: sev, Msg: msg})
}

func (s *server) appendOpsData(kind, tenant string, node uint16, sev, msg string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	s.ops.append(opsEntry{Kind: kind, Tenant: tenant, Node: node, Sev: sev, Msg: msg, Data: b})
}

// alertSettings 是租户的告警配置（State 持久化）。字段零值 = 用默认。
type AlertSettings struct {
	WebhookURL   string `json:"webhook_url,omitempty"`
	LossPermille uint32 `json:"loss_permille,omitempty"`
}

// alertSettingsLocked 取租户配置，缺省补全。
func (s *server) alertSettingsLocked(tenant string) *AlertSettings {
	as := s.state.Alerts[tenant]
	if as == nil {
		return &AlertSettings{LossPermille: opsDefaultLossPermille}
	}
	out := *as
	if out.LossPermille == 0 {
		out.LossPermille = opsDefaultLossPermille
	}
	return &out
}

// ledgerRetentionDaysLocked 返回生效的台账保留天数。0 表示还没配过，取默认；
// 越界的值（改坏了 State 文件、或者从旧版本升上来）夹回合法区间，而不是
// 让清理逻辑拿到一个 0 或负数把自己关掉。必须持有 s.mu。
func (s *server) ledgerRetentionDaysLocked() int {
	d := s.state.LedgerRetentionDays
	if d == 0 {
		return opsDefaultRetentionDays
	}
	if d < opsMinRetentionDays {
		return opsMinRetentionDays
	}
	if d > opsMaxRetentionDays {
		return opsMaxRetentionDays
	}
	return d
}

// raiseAlertLocked 升起一条告警：已活动就刷新时间戳（不再通知——同一条问题
// 每个窗口都喊一遍是噪音不是告警），新告警走台账 + SSE + webhook。
// 必须持有 s.mu（webhook 的取值和调用方在同一把锁下完成——通知本身异步，
// 但取 URL 绝不能在锁外再抢一次锁：调用方全都持着 s.mu，那就是自死锁，
// vm 台 2026-11-11 实测一次：告警一升起，整个控制面停摆）。
func (s *server) raiseAlertLocked(kind, tenant string, node uint16, ident, sev, msg string) {
	if s.alerts == nil {
		s.alerts = map[string]*alertRecord{}
	}
	k := alertKey(kind, tenant, node, ident)
	now := time.Now().Unix()
	if a := s.alerts[k]; a != nil {
		a.LastAt = now
		a.Msg = msg
		return
	}
	a := &alertRecord{Kind: kind, Tenant: tenant, Node: node, Ident: ident,
		Sev: sev, Msg: msg, RaisedAt: now, LastAt: now}
	s.alerts[k] = a
	s.appendOps("alert.raise", tenant, node, "", sev, msg)
	s.admin.publish("alert.raised", tenant, nil)
	s.notifyWebhookAsync(s.alertSettingsLocked(tenant).WebhookURL, tenant, "raise", a)
}

// clearAlertLocked 恢复一条告警：不活动就是无事发生，直接返回。必须持有 s.mu。
func (s *server) clearAlertLocked(kind, tenant string, node uint16, ident string) {
	if s.alerts == nil {
		return
	}
	k := alertKey(kind, tenant, node, ident)
	a := s.alerts[k]
	if a == nil {
		return
	}
	delete(s.alerts, k)
	s.appendOps("alert.clear", tenant, node, "", "info", "已恢复："+a.Msg)
	s.admin.publish("alert.cleared", tenant, nil)
	s.notifyWebhookAsync(s.alertSettingsLocked(tenant).WebhookURL, tenant, "clear", a)
}

// notifyWebhookAsync 把告警推到租户配置的 webhook。url 由调用方在锁内读好
// 传入（本函数绝不碰 s.mu）。异步 + 5s 超时：控制面的写路径不能等一个不回来
// 的接收端。失败只记日志，不重试——下一轮规则评估该响还会响，丢一条推送的
// 代价远小于卡住心跳处理。
func (s *server) notifyWebhookAsync(url, tenant, event string, a *alertRecord) {
	if strings.TrimSpace(url) == "" {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"event": event, // raise | clear
		"kind":  a.Kind, "tenant": a.Tenant, "node": a.Node, "ident": a.Ident,
		"sev": a.Sev, "msg": a.Msg,
		"raised_at_unix": a.RaisedAt, "at_unix": time.Now().Unix(),
	})
	if err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			log.Printf("webhook: bad url for tenant %s: %v", tenant, err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("webhook: tenant %s %s %s: %v", tenant, event, a.Kind, err)
			return
		}
		resp.Body.Close()
	}()
}

// sweepTelemetryGapLocked 在 ttlSweeper 里跑：在线设备的心跳没断，但遥测窗口
// 超时没刷新——单独一条告警，"设备在线"会把它掩盖掉。设备失联时 tel 已被
// ttlSweeper 删掉，但那台设备已经有 node.stale 告警，这里跳过 stale 不重复叫。
func (s *server) sweepTelemetryGapLocked(now int64) {
	for _, rec := range s.state.Nodes {
		if rec.Stale {
			s.clearAlertLocked("telemetry.gap", rec.TenantID, rec.NodeID, "")
			continue
		}
		k := key(rec.TenantID, rec.NodeID)
		t := s.tel[k]
		if t == nil || now-t.seenAt > telemetryGapSec {
			s.raiseAlertLocked("telemetry.gap", rec.TenantID, rec.NodeID, "", "warn",
				fmt.Sprintf("设备在线但遥测已 %d 秒没有上报（探针插件掉了吗？）", telemetryGapSec))
		} else {
			s.clearAlertLocked("telemetry.gap", rec.TenantID, rec.NodeID, "")
		}
	}
}

// ------------------------------------------------------------------ 遥测挂钩

// sampleRow 是 tunnel.sample 里一行隧道的读数明细（Data 字段）。
type sampleRow struct {
	Peer     uint16  `json:"peer"`
	Color    uint8   `json:"color,omitempty"`
	Up       bool    `json:"up"`
	BFD      string  `json:"bfd,omitempty"`
	HasProbe bool    `json:"has_probe,omitempty"`
	RTTms    float64 `json:"rtt_ms"`
	JitterMs float64 `json:"jitter_ms"`
	Loss     uint32  `json:"loss_permille"`

	// 流量（字节，窗口增量；CounterSec=0 是首窗累计）。agent 没给（老盒子、
	// stats 段读不到）时 HasCounters=false——页面上是"未上报"，不是 0 B/s。
	HasCounters bool   `json:"has_counters,omitempty"`
	CounterSec  uint32 `json:"counter_sec,omitempty"`
	RxBytes     uint64 `json:"rx_bytes,omitempty"`
	TxBytes     uint64 `json:"tx_bytes,omitempty"`
}

// recordMetricsLocked 在 postMetrics 存完实时窗口后跑，做四件事：
//
//  1. 节流落一份隧道质量采样（历史趋势图的数据源）；
//  2. 每条隧道评丢包 / BFD down 告警（丢包要连续两个窗口才算）；
//  3. 动态策略行的选路颜色变化写台账（"昨晚为什么切的"靠这个回答）；
//  4. 动态行全不达标告警（阈值就是草稿里配的 SLA）。
//
// 调用点持有 s.mu。
func (s *server) recordMetricsLocked(tenant string, nodeID uint16, rep *wire.MetricsReport, now int64) {
	nk := key(tenant, nodeID)

	// 1. 采样节流落盘。
	if s.lastSampleAt == nil {
		s.lastSampleAt = map[string]int64{}
	}
	if now-s.lastSampleAt[nk] >= opsSampleThrottleSec {
		s.lastSampleAt[nk] = now
		rows := make([]sampleRow, 0, len(rep.Tunnels))
		for _, t := range rep.Tunnels {
			rows = append(rows, sampleRow{
				Peer: t.PeerNodeID, Color: s.underlayColorForPeerLocked(tenant, nodeID, t.PeerNodeID),
				Up: t.Up, BFD: t.BFDState, HasProbe: t.HasProbe,
				RTTms: t.RTTms, JitterMs: t.JitterMs, Loss: t.LossPermille,
				HasCounters: t.HasCounters, CounterSec: t.CounterSec,
				RxBytes: t.RxBytes, TxBytes: t.TxBytes,
			})
		}
		s.appendOpsData("tunnel.sample", tenant, nodeID, "", "", rows)
	}

	// 2. 每隧道告警。
	as := s.alertSettingsLocked(tenant)
	if s.alertStrikes == nil {
		s.alertStrikes = map[string]int{}
	}
	for _, t := range rep.Tunnels {
		if t.PeerNodeID == 0 {
			continue
		}
		ident := strconv.FormatUint(uint64(t.PeerNodeID), 10)
		// 丢包：连续 opsLossStrikes 个窗口超门槛才算。strike 计数落在
		// (tenant,node,peer) 上——窗口间是有序的（agent 串行上报）。
		lk := "loss|" + alertKey("tunnel.loss", tenant, nodeID, ident)
		if t.HasProbe && t.ProbeTx > 0 && t.LossPermille >= as.LossPermille {
			s.alertStrikes[lk]++
			if s.alertStrikes[lk] >= opsLossStrikes {
				s.raiseAlertLocked("tunnel.loss", tenant, nodeID, ident, "warn",
					fmt.Sprintf("到对端 %d 的隧道连续 %d 个窗口丢包 ≥%d‰（当前 %d‰，RTT %.1fms）",
						t.PeerNodeID, s.alertStrikes[lk], as.LossPermille, t.LossPermille, t.RTTms))
			}
		} else {
			delete(s.alertStrikes, lk)
			s.clearAlertLocked("tunnel.loss", tenant, nodeID, ident)
		}
		// BFD down：状态本身就是判定，不设连击。
		if t.BFDState == "down" {
			s.raiseAlertLocked("tunnel.down", tenant, nodeID, ident, "crit",
				fmt.Sprintf("到对端 %d 的隧道 BFD down", t.PeerNodeID))
		} else {
			s.clearAlertLocked("tunnel.down", tenant, nodeID, ident)
		}
	}

	// 3. 选路变化台账。
	if s.lastSelection == nil {
		s.lastSelection = map[string]uint8{}
	}
	for _, sel := range rep.Selections {
		sk := key(tenant, nodeID) + "|" + strconv.Itoa(sel.RowID)
		prev, seen := s.lastSelection[sk]
		s.lastSelection[sk] = sel.Color
		if seen && prev == sel.Color {
			continue
		}
		msg := fmt.Sprintf("动态行 %d 选路：颜色 %d → %d（%s）", sel.RowID, prev, sel.Color, sel.Reason)
		if !seen {
			msg = fmt.Sprintf("动态行 %d 首次上报选路：颜色 %d（%s）", sel.RowID, sel.Color, sel.Reason)
		}
		s.appendOpsData("selection", tenant, nodeID, "", msg, sel)
	}

	// 4. 动态行全不达标：阈值就是草稿里的 SLA，隧道读数就是刚报上来的这份。
	// Policy 与 Leak 一样在 DeviceConfig 上（基础配置页 PUT 的是整份 BasicConfig，
	// 存在 Draft 里会被抹掉）。
	dc := s.state.Configs[configKey(tenant, nodeID)]
	if dc == nil {
		return
	}
	for _, r := range dc.Policy.Rows {
		if r.Mode != policyModeDynamic || r.Peer == 0 || r.SLA == nil {
			continue
		}
		if r.SLA.LatencyMS == 0 && r.SLA.JitterMS == 0 && r.SLA.LossPermille == 0 {
			continue // 不设门槛的行没有"不达标"可言
		}
		ident := strconv.Itoa(r.ID)
		has := false
		ok := false
		for _, t := range rep.Tunnels {
			if t.PeerNodeID != r.Peer {
				continue
			}
			has = true
			if slaEligible(r.SLA, t) {
				ok = true
			}
		}
		if !has {
			// 没有到对端的隧道读数：可能是隧道还没收敛，不是 SLA 问题。
			s.clearAlertLocked("sla.nocand", tenant, nodeID, ident)
			continue
		}
		if ok {
			s.clearAlertLocked("sla.nocand", tenant, nodeID, ident)
		} else {
			s.raiseAlertLocked("sla.nocand", tenant, nodeID, ident, "warn",
				fmt.Sprintf("动态行 %d（对端 %d）没有候选隧道满足 SLA（%s）",
					r.ID, r.Peer, slaThresholdText(r.SLA)))
		}
	}
}

// slaEligible 是控制器侧的达标判定，与盒子上 sla.eligible 同一套规则
// （HasProbe && Tx>0 且每项门槛满足，0 = 不约束）。两边各自实现、语义对齐
// 是这条边界的既有做法（配置包同理）——把阈值放草稿里、读数放上报里，
// 控制器不猜盒子在想什么。
func slaEligible(sla *PolicySLA, t wire.TunnelMetrics) bool {
	if !t.HasProbe || t.ProbeTx == 0 {
		return false
	}
	if sla.LossPermille > 0 && t.LossPermille > sla.LossPermille {
		return false
	}
	if sla.LatencyMS > 0 && t.RTTms > sla.LatencyMS {
		return false
	}
	if sla.JitterMS > 0 && t.JitterMs > sla.JitterMS {
		return false
	}
	return true
}

func slaThresholdText(sla *PolicySLA) string {
	var parts []string
	if sla.LatencyMS > 0 {
		parts = append(parts, fmt.Sprintf("延迟≤%gms", sla.LatencyMS))
	}
	if sla.JitterMS > 0 {
		parts = append(parts, fmt.Sprintf("抖动≤%gms", sla.JitterMS))
	}
	if sla.LossPermille > 0 {
		parts = append(parts, fmt.Sprintf("丢包≤%d‰", sla.LossPermille))
	}
	return strings.Join(parts, "，")
}

// underlayColorForPeerLocked 把 (node, peer) 对到隧道颜色。今天一设备对一条
// 隧道（M-3 门禁），这个映射是唯一的；M-3 落地后一对多颜色时这里要换成
// "隧道清单里该对端全部颜色"。找不到返回 0（主表色），调用方把它当"未知"。
func (s *server) underlayColorForPeerLocked(tenant string, nodeID, peer uint16) uint8 {
	uc := s.state.Underlays[tenant]
	if uc == nil || uc.State == nil {
		return 0
	}
	for _, t := range uc.State.Tunnels {
		if (t.A.NodeID == nodeID && t.B.NodeID == peer) || (t.A.NodeID == peer && t.B.NodeID == nodeID) {
			return t.Color
		}
	}
	return 0
}

// ------------------------------------------------------------------ console API

// opsMaxQueryHours 是台账/历史查询的时间窗上限：台账按天扫文件，无上限的
// 查询会变成一次全量扫描。
const opsMaxQueryHours = 24 * 7

func (s *server) handleAdminHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	hours, node := opsQueryParams(w, r, 24)
	if hours == 0 {
		return
	}
	now := time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	ents := s.ops.query(now-int64(hours)*3600, now,
		map[string]bool{"tunnel.sample": true}, id.TenantID, node)
	samples := make([]map[string]any, 0, len(ents))
	for _, e := range ents {
		var rows []sampleRow
		if json.Unmarshal(e.Data, &rows) != nil {
			continue
		}
		samples = append(samples, map[string]any{"at": e.At, "node": e.Node, "rows": rows})
	}
	writeJSON(w, map[string]any{"samples": samples, "hours": hours})
}

func (s *server) handleAdminLedger(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	hours, node := opsQueryParams(w, r, 48)
	if hours == 0 {
		return
	}
	kinds := map[string]bool{}
	if q := strings.TrimSpace(r.URL.Query().Get("kind")); q != "" {
		for _, k := range strings.Split(q, ",") {
			if k = strings.TrimSpace(k); k != "" {
				kinds[k] = true
			}
		}
	}
	// kind 没指定 = 全部事件（审计在内），但采样明细不混进来——它是画图的
	// 数据，不是给人读的行。
	if len(kinds) == 0 {
		kinds = map[string]bool{"node.enrolled": true, "node.online": true, "node.stale": true,
			"config.publish": true, "config.rollback": true, "underlay.publish": true,
			"underlay.change": true, "selection": true, "alert.raise": true, "alert.clear": true,
			"diag.issue": true, "diag.result": true, "audit": true}
	}
	now := time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	ents := s.ops.query(now-int64(hours)*3600, now, kinds, id.TenantID, node)
	// 最新在前（人看台账先看刚才），并截尾：一次读几万行对浏览器是灾难。
	sort.Slice(ents, func(i, j int) bool { return ents[i].At > ents[j].At })
	const capN = 2000
	if len(ents) > capN {
		ents = ents[:capN]
	}
	writeJSON(w, map[string]any{"entries": ents, "hours": hours})
}

// opsQueryParams 读 hours/node 两个公共参数。返回 hours=0 表示已经写过 400。
func opsQueryParams(w http.ResponseWriter, r *http.Request, def int) (int, uint16) {
	hours := def
	if q := strings.TrimSpace(r.URL.Query().Get("hours")); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 || n > opsMaxQueryHours {
			http.Error(w, fmt.Sprintf("hours must be 1..%d", opsMaxQueryHours), http.StatusBadRequest)
			return 0, 0
		}
		hours = n
	}
	var node uint16
	if q := strings.TrimSpace(r.URL.Query().Get("node")); q != "" {
		n, err := strconv.ParseUint(q, 10, 16)
		if err != nil || n == 0 {
			http.Error(w, "bad node", http.StatusBadRequest)
			return 0, 0
		}
		node = uint16(n)
	}
	return hours, node
}

// handleAdminAlerts 是告警的读 + 配置面：GET 返回活动告警、设置和台账保留
// 期，PUT 改设置。
func (s *server) handleAdminAlerts(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		defer s.mu.Unlock()
		active := make([]*alertRecord, 0, len(s.alerts))
		for _, a := range s.alerts {
			if a.Tenant == id.TenantID {
				active = append(active, a)
			}
		}
		sort.Slice(active, func(i, j int) bool {
			if active[i].Sev != active[j].Sev {
				return active[i].Sev == "crit" // crit 在前
			}
			return active[i].RaisedAt < active[j].RaisedAt
		})
		writeJSON(w, map[string]any{"active": active, "settings": s.alertSettingsLocked(id.TenantID),
			"retention": s.retentionViewLocked()})
	case http.MethodPut:
		var req alertSettingsPut
		if !decodeJSONBody(w, r, &req) {
			return
		}
		url := strings.TrimSpace(req.WebhookURL)
		if url != "" && !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			http.Error(w, "webhook_url must be http(s)://", http.StatusBadRequest)
			return
		}
		if req.LossPermille > 1000 {
			http.Error(w, "loss_permille must be 0..1000", http.StatusBadRequest)
			return
		}
		// 保留期先校验再落任何东西：先写告警设置再发现保留期越界，会留下
		// "告警设置改了、却被 400 说成整份没生效"的半份状态。
		if req.RetentionDays != nil && (*req.RetentionDays < opsMinRetentionDays || *req.RetentionDays > opsMaxRetentionDays) {
			// 越界直接拒，而不是悄悄夹一下：用户以为设了 90 天、实际被记成
			// 365 天，比报错更难发现。
			http.Error(w, fmt.Sprintf("retention_days must be %d..%d",
				opsMinRetentionDays, opsMaxRetentionDays), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.state.Alerts == nil {
			s.state.Alerts = map[string]*AlertSettings{}
		}
		settings := req.AlertSettings
		s.state.Alerts[id.TenantID] = &settings
		msg := fmt.Sprintf("更新告警设置（webhook=%t，丢包门槛=%d‰）", url != "", req.LossPermille)
		if req.RetentionDays != nil {
			msg += fmt.Sprintf("，台账保留=%d 天", *req.RetentionDays)
			s.state.LedgerRetentionDays = *req.RetentionDays
		}
		s.save()
		s.appendOps("audit", id.TenantID, 0, id.Username, "info", msg)
		writeJSON(w, map[string]any{"ok": true, "settings": s.alertSettingsLocked(id.TenantID),
			"retention": s.retentionViewLocked()})
	default:
		methodNotAllowed(w, "GET, PUT")
	}
}

// alertSettingsPut 是 PUT /api/alerts 的请求体：告警设置本身按租户存，台账
// 保留期是整台控制器一个数（台账是所有人共用的一份目录），所以两者放在同
// 一个请求里但落到不同的地方。retention_days 用指针区分"没提这件事"和
// "设成 0"。
type alertSettingsPut struct {
	AlertSettings
	RetentionDays *int `json:"retention_days,omitempty"`
}

// retentionViewLocked 是保留期的读视图：当前值 + 合法区间，页面据此画输入框
// 和提示，不用把常量抄一份到 JS 里。
func (s *server) retentionViewLocked() map[string]any {
	return map[string]any{
		"days":         s.ledgerRetentionDaysLocked(),
		"default_days": opsDefaultRetentionDays,
		"min_days":     opsMinRetentionDays,
		"max_days":     opsMaxRetentionDays,
	}
}

// ------------------------------------------------------------------ audit 中间件

// auditStatusWriter 记下 handler 写的状态码。只包 /api/* 的写方法；SSE
// （/api/events 是 GET）直接透传，不经过这层。
type auditStatusWriter struct {
	http.ResponseWriter
	code int
}

func (sw *auditStatusWriter) WriteHeader(c int) {
	sw.code = c
	sw.ResponseWriter.WriteHeader(c)
}

// Flush 透传：jsonl 的 watch 流不在这里，但保持 ResponseWriter 的完整语义
// 是包装者的义务。
func (sw *auditStatusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// auditMiddleware 给控制台 mux 套一层：所有 /api/* 的非 GET 请求记一条
// audit 台账（actor=登录名，空 = 未登录就撞上来的）。一个钩子覆盖全部
// 现在和将来的写端点，新增 handler 不用记得埋点。登录请求也在内——
// 谁登进来的、失败的尝试，都是台账该有的东西。
func (s *server) auditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") ||
			r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		sw := &auditStatusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		id := s.session(r)
		actor := id.Username
		if actor == "" {
			actor = "-" // 未登录（401/403）：记路径，别把尝试丢掉
		}
		s.ops.append(opsEntry{
			Kind: "audit", Actor: actor,
			Tenant: id.TenantID, Sev: "info",
			Msg: fmt.Sprintf("%s %s%s → %d", r.Method, r.URL.Path,
				queryTail(r.URL.RawQuery), sw.code),
		})
	})
}

// queryTail 把查询串截短放进审计行：node=1 有用，整段 base64 没用。
func queryTail(raw string) string {
	if raw == "" {
		return ""
	}
	if len(raw) > 60 {
		raw = raw[:60] + "…"
	}
	return "?" + raw
}
