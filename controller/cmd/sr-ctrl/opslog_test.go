package main

// opslog_test.go —— 台账与告警的最小回归面。
//
// 重点不在行数，在三件容易悄悄坏的事：
//  1. append/query 的往返要保真（按天轮换、租户过滤、坏行跳过）；
//  2. 告警幂等：同一条问题反复发生只刷新时间戳，不重复通知；
//  3. 审计中间件只记 /api/* 的写方法，GET（SSE 也在内）直接透传。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestLedger(t *testing.T) *opsLedger {
	t.Helper()
	dir := t.TempDir()
	l, err := openOpsLedger(dir)
	if err != nil {
		t.Fatalf("openOpsLedger: %v", err)
	}
	return l
}

func TestLedgerAppendQueryRoundtrip(t *testing.T) {
	l := newTestLedger(t)
	now := time.Now()
	l.append(opsEntry{At: now.Add(-2 * time.Hour).Unix(), Kind: "node.stale", Tenant: "t-a", Node: 1, Sev: "warn", Msg: "失联"})
	l.append(opsEntry{At: now.Add(-1 * time.Hour).Unix(), Kind: "config.publish", Tenant: "t-a", Node: 1, Actor: "alice", Msg: "发布 v3"})
	l.append(opsEntry{At: now.Add(-30 * time.Minute).Unix(), Kind: "node.stale", Tenant: "t-b", Node: 2, Sev: "warn", Msg: "别家租户的失联"})
	l.append(opsEntry{At: now.Unix(), Kind: "tunnel.sample", Tenant: "t-a", Node: 1,
		Data: json.RawMessage(`[{"peer":3,"rtt_ms":1.5,"loss_permille":0}]`)})

	// 全量：按租户过滤。
	got := l.query(now.Add(-24*time.Hour).Unix(), now.Add(time.Minute).Unix(), nil, "t-a", 0)
	if len(got) != 3 {
		t.Fatalf("t-a 应有 3 行，得到 %d: %+v", len(got), got)
	}
	if got[0].At > got[1].At || got[1].At > got[2].At {
		t.Fatalf("query 必须按时间升序: %+v", got)
	}
	// kind 过滤。
	got = l.query(now.Add(-24*time.Hour).Unix(), now.Add(time.Minute).Unix(),
		map[string]bool{"config.publish": true}, "t-a", 0)
	if len(got) != 1 || got[0].Actor != "alice" {
		t.Fatalf("kind 过滤失真: %+v", got)
	}
	// 采样行带 Data。
	got = l.query(now.Add(-24*time.Hour).Unix(), now.Add(time.Minute).Unix(),
		map[string]bool{"tunnel.sample": true}, "t-a", 0)
	if len(got) != 1 || len(got[0].Data) == 0 {
		t.Fatalf("sample 行丢了 Data: %+v", got)
	}
}

func TestLedgerNilSafeAndBadLines(t *testing.T) {
	// nil 台账：append/query 不许 panic（测试里的 server 没有 ops）。
	var l *opsLedger
	l.append(opsEntry{Kind: "audit"})
	if got := l.query(0, time.Now().Unix(), nil, "", 0); len(got) != 0 {
		t.Fatalf("nil ledger query 应返回空: %+v", got)
	}
	// 坏行跳过：半行崩溃残留不能让查询失败。
	dir := t.TempDir()
	day := time.Now().Format("2006-01-02")
	good := `{"at_unix":` + it64(time.Now().Unix()) + `,"kind":"audit","msg":"ok"}`
	os.WriteFile(filepath.Join(dir, day+".jsonl"),
		[]byte(good+"\n{\"at_unix\": broken\n"), 0600)
	l2 := &opsLedger{dir: dir}
	got := l2.query(time.Now().Add(-time.Hour).Unix(), time.Now().Add(time.Hour).Unix(), nil, "", 0)
	if len(got) != 1 || got[0].Msg != "ok" {
		t.Fatalf("坏行应被跳过: %+v", got)
	}
}

func it64(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func newTestServerWithOps(t *testing.T) *server {
	t.Helper()
	l, err := openOpsLedger(t.TempDir())
	if err != nil {
		t.Fatalf("openOpsLedger: %v", err)
	}
	// store 也要给：告警设置保存走 s.save()，没有 store 的 server 一写就 panic。
	return &server{
		state:  seedState(),
		store:  &fileStore{path: filepath.Join(t.TempDir(), "state.json")},
		admin:  newAdminHub(),
		ops:    l,
		alerts: map[string]*alertRecord{},
	}
}

// TestAlertRaiseUnderServerLock 是 vm 台 2026-11-11 事故的回归面：所有
// *Locked 告警入口的真实调用方都持有 s.mu，通知链路（webhook 取设置）绝不能
// 在锁内再抢一次锁。死锁的形状是"测试挂死"——用看门狗把它变成显式失败。
func TestAlertRaiseUnderServerLock(t *testing.T) {
	s := newTestServerWithOps(t)
	s.mu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.raiseAlertLocked("tunnel.loss", "tenant-a", 1, "3", "warn", "丢包")
		s.clearAlertLocked("tunnel.loss", "tenant-a", 1, "3")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("持 s.mu 调 raise/clear 死锁了——通知链路在锁内又抢了一次锁")
	}
	s.mu.Unlock()
}

func TestAlertRaiseClearDedup(t *testing.T) {
	s := newTestServerWithOps(t)
	s.raiseAlertLocked("tunnel.loss", "tenant-a", 1, "3", "warn", "丢包 300‰")
	s.raiseAlertLocked("tunnel.loss", "tenant-a", 1, "3", "warn", "丢包 500‰")
	if len(s.alerts) != 1 {
		t.Fatalf("同一条告警应幂等，现在 %d 条", len(s.alerts))
	}
	if s.alerts[alertKey("tunnel.loss", "tenant-a", 1, "3")].Msg != "丢包 500‰" {
		t.Fatalf("重复发生应刷新消息")
	}
	s.clearAlertLocked("tunnel.loss", "tenant-a", 1, "3")
	if len(s.alerts) != 0 {
		t.Fatalf("clear 后应无活动告警")
	}
	// 再 clear 一次：不活动就是无事发生。
	s.clearAlertLocked("tunnel.loss", "tenant-a", 1, "3")
	// 台账里有 raise ×1 + clear ×1（幂等的那次 raise 不再写台账）。
	ents := s.ops.query(0, time.Now().Add(time.Minute).Unix(),
		map[string]bool{"alert.raise": true, "alert.clear": true}, "tenant-a", 0)
	if len(ents) != 2 {
		t.Fatalf("raise/clear 台账应为 2 行，得到 %d", len(ents))
	}
}

func TestAuditMiddleware(t *testing.T) {
	s := newTestServerWithOps(t)
	s.admin.sess["tok"] = consoleIdentity{Role: roleTenant, TenantID: "tenant-a",
		Username: "alice", expires: time.Now().Add(time.Hour)}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/thing", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		// SSE：只 GET，永远不该进台账。
		w.Write([]byte("ok"))
	})
	h := s.auditMiddleware(mux)

	// 写请求：记。
	req := httptest.NewRequest("PUT", "/api/thing?node=1", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// 读请求：不记。
	req2 := httptest.NewRequest("GET", "/api/events", nil)
	req2.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	// 未登录的写请求：记，actor = "-"。
	req3 := httptest.NewRequest("POST", "/api/thing", nil)
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)

	ents := s.ops.query(0, time.Now().Add(time.Minute).Unix(),
		map[string]bool{"audit": true}, "", 0)
	if len(ents) != 2 {
		t.Fatalf("应记 2 条（PUT + 未登录 POST），GET 不记；得到 %d: %+v", len(ents), ents)
	}
	if ents[0].Actor != "alice" || !strings.Contains(ents[0].Msg, "PUT /api/thing?node=1 → 204") {
		t.Fatalf("审计行缺 actor 或状态码: %+v", ents[0])
	}
	if ents[1].Actor != "-" {
		t.Fatalf("未登录写请求 actor 应为 \"-\": %+v", ents[1])
	}
}

func TestAlertSettingsDefaults(t *testing.T) {
	s := newTestServerWithOps(t)
	// 老租户没有设置块：读侧补默认值，不发明 nil。
	as := s.alertSettingsLocked("tenant-a")
	if as.LossPermille != opsDefaultLossPermille {
		t.Fatalf("默认丢包门槛应为 %d，得到 %d", opsDefaultLossPermille, as.LossPermille)
	}
	s.state.Alerts["tenant-a"] = &AlertSettings{WebhookURL: "http://x/y", LossPermille: 100}
	as = s.alertSettingsLocked("tenant-a")
	if as.LossPermille != 100 || as.WebhookURL != "http://x/y" {
		t.Fatalf("设置读取失真: %+v", as)
	}
}

// TestLedgerPruneFollowsSetting：保留期不是常量了，清理得跟着设置走——
// 改小立刻删，改大不会把已经删掉的变回来（也不需要它变）。
func TestLedgerPruneFollowsSetting(t *testing.T) {
	s := newTestServerWithOps(t)
	now := time.Now()
	mk := func(d time.Time) string {
		name := d.Format("2006-01-02") + ".jsonl"
		if err := os.WriteFile(filepath.Join(s.ops.dir, name), []byte("{}\n"), 0600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		return name
	}
	old := mk(now.AddDate(0, 0, -40))
	mid := mk(now.AddDate(0, 0, -10))
	today := mk(now)
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(s.ops.dir, name))
		return err == nil
	}

	s.mu.Lock()
	s.state.LedgerRetentionDays = 7
	s.pruneLedgerIfDueLocked(now.Unix())
	s.mu.Unlock()
	if exists(old) || exists(mid) {
		t.Fatal("保留 7 天时，40 天前和 10 天前的文件都该删掉")
	}
	if !exists(today) {
		t.Fatal("今天的台账文件不该被删")
	}

	// 同一天再调一次（sweeper 每 2s 一拍）不该重复扫目录——用"记下来的
	// 那一天已经等于今天"来验，顺带证明改大保留期会重新触发一次。
	s.mu.Lock()
	s.state.LedgerRetentionDays = 30
	s.pruneLedgerIfDueLocked(now.Unix())
	days, day := s.opsPruneDays, s.opsPruneDay
	s.mu.Unlock()
	if days != 30 || day != now.Format("2006-01-02") {
		t.Fatalf("保留期改了应重新清一次，现在是 %d 天 / %s", days, day)
	}
}
