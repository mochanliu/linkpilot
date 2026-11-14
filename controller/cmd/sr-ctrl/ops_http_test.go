package main

// ops_http_test.go —— 控制台 HTTP 层的冒烟：真实 adminHandler mux + 真实
// 磁盘台账，验证"登录 → 读告警/台账/历史 → 改设置 → 审计留痕"整条链。
// vm 台的活体验证被沙箱挡住了 /api/login 的 POST（宿主侧安全控制），这条
// 测试就是那个缺口的替身。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newOpsHttpServer(t *testing.T) (*server, http.Handler, string, string) {
	t.Helper()
	s := newTestServerWithOps(t)
	// 一个租户账号 + 一台设备 + 一份草稿（动态行有 SLA，供台账侧评估用）。
	now := time.Now().Unix()
	s.state.Accounts["vm-tenant"] = &Account{Username: "vm-tenant", Role: roleTenant,
		TenantID: "tenant-vm", Password: "pbkdf2$sha256$x", CreatedAt: now}
	s.state.Nodes["tenant-vm|1"] = &NodeRecord{TenantID: "tenant-vm", NodeID: 1,
		Serial: "CPE1", LastSeen: now}
	h := s.adminHandler()
	return s, h, "vm-tenant", "pw-not-checked-by-smoke"
}

func (s *server) opsLogin(t *testing.T, h http.Handler, username string) *http.Request {
	t.Helper()
	// 密码哈希对不上会 401——冒烟里直接在 hub 里放一个会话，等价于登录成功，
	// 但不走 PBKDF2（password_test.go 已覆盖真实登录路径）。
	s.admin.mu.Lock()
	s.admin.sess["smoke-sid"] = consoleIdentity{Role: roleTenant, TenantID: "tenant-vm",
		Username: username, expires: time.Now().Add(time.Hour)}
	s.admin.mu.Unlock()
	req := httptest.NewRequest("GET", "/api/alerts", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "smoke-sid"})
	return req
}

func doReq(t *testing.T, h http.Handler, req *http.Request) (int, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestOpsConsoleHTTPSmoke(t *testing.T) {
	s, h, user, _ := newOpsHttpServer(t)
	req := s.opsLogin(t, h, user)

	// 1. GET /api/alerts：空活动 + 默认设置。
	code, body := doReq(t, h, req)
	if code != 200 {
		t.Fatalf("alerts: %d %s", code, body)
	}
	var d map[string]any
	json.Unmarshal(body, &d)
	if d["active"] == nil || len(d["active"].([]any)) != 0 {
		t.Fatalf("初始应无活动告警: %s", body)
	}

	// 2. GET /api/ledger：登录后这里应至少有一条 audit（上面那次 GET 是
	// 读不进台账的，但 opsLogin 放会话不经过 mux；先制造一条写请求）。
	wreq := httptest.NewRequest("PUT", "/api/alerts?node=1",
		strings.NewReader(`{"webhook_url":"http://127.0.0.1:9/none","loss_permille":300}`))
	wreq.AddCookie(&http.Cookie{Name: sessionCookie, Value: "smoke-sid"})
	wreq.Header.Set("Content-Type", "application/json")
	code, body = doReq(t, h, wreq)
	if code != 200 {
		t.Fatalf("PUT alerts: %d %s", code, body)
	}
	greq := httptest.NewRequest("GET", "/api/ledger?hours=24&kind=audit",
		nil)
	greq.AddCookie(&http.Cookie{Name: sessionCookie, Value: "smoke-sid"})
	code, body = doReq(t, h, greq)
	if code != 200 {
		t.Fatalf("ledger: %d %s", code, body)
	}
	json.Unmarshal(body, &d)
	ents := d["entries"].([]any)
	found := false
	for _, e := range ents {
		m := e.(map[string]any)
		if m["kind"] == "audit" && strings.Contains(m["msg"].(string), "PUT /api/alerts?node=1") &&
			m["actor"] == "vm-tenant" {
			found = true
		}
	}
	if !found {
		t.Fatalf("PUT /api/alerts 没有留下审计行: %s", body)
	}
	// 设置真的进了 State。
	as := s.alertSettingsLocked("tenant-vm")
	if as.LossPermille != 300 || as.WebhookURL != "http://127.0.0.1:9/none" {
		t.Fatalf("告警设置没落库: %+v", as)
	}

	// 3. GET /api/history：先落两条采样，读回来必须对上。
	s.mu.Lock()
	s.appendOpsData("tunnel.sample", "tenant-vm", 1, "", "", []sampleRow{
		{Peer: 3, Color: 2, Up: true, RTTms: 1.5, Loss: 0},
	})
	s.mu.Unlock()
	hreq := httptest.NewRequest("GET", "/api/history?hours=24&node=1", nil)
	hreq.AddCookie(&http.Cookie{Name: sessionCookie, Value: "smoke-sid"})
	code, body = doReq(t, h, hreq)
	if code != 200 {
		t.Fatalf("history: %d %s", code, body)
	}
	json.Unmarshal(body, &d)
	samples := d["samples"].([]any)
	if len(samples) != 1 {
		t.Fatalf("应有 1 条采样: %s", body)
	}
	rows := samples[0].(map[string]any)["rows"].([]any)
	r0 := rows[0].(map[string]any)
	if r0["peer"].(float64) != 3 || r0["color"].(float64) != 2 {
		t.Fatalf("采样行失真: %s", body)
	}

	// 4. 运营角色读租户台账：403（租户边界在 API 上，不只在页面上）。
	s.admin.mu.Lock()
	s.admin.sess["op-sid"] = consoleIdentity{Role: roleOperator, Username: "op",
		expires: time.Now().Add(time.Hour)}
	s.admin.mu.Unlock()
	oreq := httptest.NewRequest("GET", "/api/ledger", nil)
	oreq.AddCookie(&http.Cookie{Name: sessionCookie, Value: "op-sid"})
	code, _ = doReq(t, h, oreq)
	if code != 403 {
		t.Fatalf("运营角色读台账应 403，得到 %d", code)
	}

	// 5. 坏 webhook 拒绝。
	breq := httptest.NewRequest("PUT", "/api/alerts",
		strings.NewReader(`{"webhook_url":"ftp://nope"}`))
	breq.AddCookie(&http.Cookie{Name: sessionCookie, Value: "smoke-sid"})
	breq.Header.Set("Content-Type", "application/json")
	code, _ = doReq(t, h, breq)
	if code != 400 {
		t.Fatalf("ftp:// webhook 应 400，得到 %d", code)
	}
}

// TestOpsLedgerTenantIsolation：租户 A 的行对租户 B 永远不存在——哪怕在
// 同一个文件里。
func TestOpsLedgerTenantIsolation(t *testing.T) {
	s := newTestServerWithOps(t)
	s.appendOps("node.stale", "tenant-a", 1, "", "warn", "A 的失联")
	s.appendOps("node.stale", "tenant-b", 2, "", "warn", "B 的失联")
	ents := s.ops.query(0, time.Now().Add(time.Minute).Unix(), nil, "tenant-a", 0)
	for _, e := range ents {
		if e.Tenant != "tenant-a" {
			t.Fatalf("租户隔离漏了: %+v", e)
		}
	}
	if len(ents) != 1 {
		t.Fatalf("tenant-a 应只看到自己的 1 行，得到 %d", len(ents))
	}
	_ = fmt.Sprint()
}

// TestAlertsRetentionAPI：「台账与告警」页上的保留天数——读得出默认、改得动、
// 越界拒、被拒的请求不会留下半份告警设置。
func TestAlertsRetentionAPI(t *testing.T) {
	s, h, user, _ := newOpsHttpServer(t)
	s.opsLogin(t, h, user) // 放一个会话进 hub，下面的请求带上同一个 cookie

	get := func() map[string]any {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/alerts", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "smoke-sid"})
		code, body := doReq(t, h, r)
		if code != 200 {
			t.Fatalf("GET alerts: %d %s", code, body)
		}
		var d map[string]any
		if err := json.Unmarshal(body, &d); err != nil {
			t.Fatalf("alerts 不是 JSON: %s", body)
		}
		return d
	}

	d := get()
	ret := d["retention"].(map[string]any)
	if ret["days"].(float64) != float64(opsDefaultRetentionDays) {
		t.Fatalf("没配过时应报默认 %d 天: %v", opsDefaultRetentionDays, ret)
	}
	if ret["min_days"].(float64) != float64(opsMinRetentionDays) ||
		ret["max_days"].(float64) != float64(opsMaxRetentionDays) {
		t.Fatalf("区间没给全，页面画不出输入框: %v", ret)
	}

	put := func(body string) int {
		t.Helper()
		r := httptest.NewRequest("PUT", "/api/alerts", strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "smoke-sid"})
		r.Header.Set("Content-Type", "application/json")
		code, _ := doReq(t, h, r)
		return code
	}

	if code := put(`{"webhook_url":"http://127.0.0.1:9/x","loss_permille":150,"retention_days":7}`); code != 200 {
		t.Fatalf("改保留期应 200，得到 %d", code)
	}
	if s.state.LedgerRetentionDays != 7 {
		t.Fatalf("保留期没落库: %d", s.state.LedgerRetentionDays)
	}
	if got := get()["retention"].(map[string]any)["days"].(float64); got != 7 {
		t.Fatalf("读回来不是 7 天: %v", got)
	}

	// 越界：拒，而且不能把同一份请求里的 webhook 改掉（半份生效最坑）。
	if code := put(`{"webhook_url":"http://127.0.0.1:9/y","retention_days":0}`); code != 400 {
		t.Fatalf("0 天应 400，得到 %d", code)
	}
	if code := put(`{"webhook_url":"http://127.0.0.1:9/y","retention_days":400}`); code != 400 {
		t.Fatalf("%d 天应 400，得到 %d", opsMaxRetentionDays+35, code)
	}
	if as := s.alertSettingsLocked("tenant-vm"); as.WebhookURL != "http://127.0.0.1:9/x" || as.LossPermille != 150 {
		t.Fatalf("被拒的请求改了告警设置: %+v", as)
	}
	if s.state.LedgerRetentionDays != 7 {
		t.Fatalf("被拒的请求改了保留期: %d", s.state.LedgerRetentionDays)
	}

	// 不带 retention_days 的老请求（只改 webhook）不该把保留期打回默认。
	if code := put(`{"webhook_url":"http://127.0.0.1:9/z","loss_permille":250}`); code != 200 {
		t.Fatalf("只改告警设置应 200，得到 %d", code)
	}
	if s.state.LedgerRetentionDays != 7 {
		t.Fatalf("只改 webhook 把保留期冲掉了: %d", s.state.LedgerRetentionDays)
	}
}
