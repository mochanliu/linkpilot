package main

// config_versions_test.go —— 版本历史 / 回滚的回归面。三个决定行为的点：
// ① 发布进历史（重发相同字节不进）；② 回滚恢复的是"草稿输入"并走同一条
// 发布路径（版本号前进、盒子按普通发布拉取）；③ 没有快照的旧版本拒回滚。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sdwan/controller/internal/wire"
)

func newVersionsServer(t *testing.T) (*server, http.Handler, *NodeRecord, *DeviceConfig) {
	t.Helper()
	s := newTestServerWithOps(t)
	now := time.Now().Unix()
	s.state.Accounts["vm-tenant"] = &Account{Username: "vm-tenant", Role: roleTenant,
		TenantID: "tenant-vm", Password: "pbkdf2$sha256$x", CreatedAt: now}
	n := &NodeRecord{TenantID: "tenant-vm", NodeID: 1, Serial: "CPE1", LastSeen: now}
	s.state.Nodes["tenant-vm|1"] = n
	dc := &DeviceConfig{TenantID: "tenant-vm", NodeID: 1}
	dc.Draft = defaultBasicConfig()
	s.state.Configs["tenant-vm|1"] = dc
	return s, s.adminHandler(), n, dc
}

func verReq(t *testing.T, h http.Handler, method, target, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "smoke-sid"})
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return doReq(t, h, req)
}

func publishAs(t *testing.T, s *server, dc *DeviceConfig, ver int) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	dc.Version = ver
	s.publishDeviceLocked("tenant-vm", 1, dc, nil, "tester", time.Now().Unix())
}

func TestPublishPushesHistoryAndSource(t *testing.T) {
	s, _, _, dc := newVersionsServer(t)
	publishAs(t, s, dc, 5)
	if dc.Published == nil || dc.Published.Version != 5 {
		t.Fatalf("首次发布应到 v5: %+v", dc.Published)
	}
	if dc.Published.Source == nil {
		t.Fatal("发布应带草稿快照")
	}
	// 改草稿再发布：旧版进历史。
	dc.Draft.Hostname = "renamed"
	s.mu.Lock()
	changed := s.publishConfigLocked("tenant-vm", 1, dc, nil, "tester", time.Now().Unix())
	s.mu.Unlock()
	if !changed || dc.Published.Version != 6 {
		t.Fatalf("第二次发布应升到 v6: %+v", dc.Published)
	}
	if len(dc.History) != 1 || dc.History[0].Version != 5 {
		t.Fatalf("历史应存 v5: %+v", dc.History)
	}
	if dc.History[0].Source == nil {
		t.Fatal("历史快照也必须带 Source（回滚要用）")
	}
	if dc.History[0].Source.Draft.Hostname == "renamed" {
		t.Fatal("历史快照被随后的草稿编辑污染了（深拷贝失效）")
	}
	// 重发相同字节：不进历史。
	s.mu.Lock()
	changed = s.publishConfigLocked("tenant-vm", 1, dc, nil, "tester", time.Now().Unix())
	s.mu.Unlock()
	if changed {
		t.Fatal("相同字节重发不应算变更")
	}
	if len(dc.History) != 1 {
		t.Fatalf("重发不应进历史: %d", len(dc.History))
	}
}

func TestHistoryCapped(t *testing.T) {
	s, _, _, dc := newVersionsServer(t)
	for i := 1; i <= configHistoryCap+6; i++ {
		dc.Draft.Hostname = "h" + string(rune('a'+i%26))
		publishAs(t, s, dc, i)
	}
	if dc.Published.Version != configHistoryCap+6 {
		t.Fatalf("最新版本号: %+v", dc.Published)
	}
	if len(dc.History) != configHistoryCap {
		t.Fatalf("历史应封顶 %d: %d", configHistoryCap, len(dc.History))
	}
	// 最老的被挤掉，最新的在尾部。
	if dc.History[0].Version != dc.Published.Version-configHistoryCap {
		t.Fatalf("历史首条版本号: %d", dc.History[0].Version)
	}
}

func TestRollbackRestoresDraftAndPublishes(t *testing.T) {
	s, h, n, dc := newVersionsServer(t)
	s.admin.mu.Lock()
	s.admin.sess["smoke-sid"] = consoleIdentity{Role: roleTenant, TenantID: "tenant-vm",
		Username: "vm-tenant", expires: time.Now().Add(time.Hour)}
	s.admin.mu.Unlock()

	// v5：主机名 old，一条静态路由。
	dc.Draft.Hostname = "old-cpe"
	dc.Draft.Statics = []ConfigStatic{{Prefix: "10.9.0.0/16", NextHop: "172.16.12.9", VPN: 0}}
	publishAs(t, s, dc, 5)
	// v6：主机名 new、路由删掉。
	dc.Draft.Hostname = "new-cpe"
	dc.Draft.Statics = nil
	publishAs(t, s, dc, 6)

	// 回滚到 v5。
	code, body := verReq(t, h, "POST", "/api/config/rollback", `{"node_id":1,"version":5}`)
	if code != 200 {
		t.Fatalf("rollback: %d %s", code, body)
	}
	var view configView
	json.Unmarshal(body, &view)
	if view.Version != 7 || view.PublishedVersion != 7 {
		t.Fatalf("回滚应作为 v7 发布: %s", body)
	}
	if view.Config.Hostname != "old-cpe" || len(view.Config.Statics) != 1 {
		t.Fatalf("草稿输入应恢复为 v5 内容: %s", body)
	}
	// 新 bundle 的 artifacts 与 v5 的快照一致（同一世界里的同配置）。
	b5 := findPublishedVersion(dc, 5)
	if b5 == nil || b5.Artifacts.FRR != dc.Published.Artifacts.FRR {
		t.Fatal("回滚后渲染应与 v5 快照一致")
	}
	// 盒子的视角：设备应被告知 v7（buildConfigView 已验证）；台账有回滚行。
	ents := s.ops.query(0, time.Now().Add(time.Minute).Unix(), nil, "tenant-vm", 1)
	found := false
	for _, e := range ents {
		if e.Kind == "config.rollback" && strings.Contains(e.Msg, "v5") {
			found = true
		}
	}
	if !found {
		t.Fatalf("台账缺 config.rollback: %+v", ents)
	}
	_ = n
}

func TestRollbackOldVersionWithoutSourceRefused(t *testing.T) {
	s, h, _, dc := newVersionsServer(t)
	s.admin.mu.Lock()
	s.admin.sess["smoke-sid"] = consoleIdentity{Role: roleTenant, TenantID: "tenant-vm",
		Username: "vm-tenant", expires: time.Now().Add(time.Hour)}
	s.admin.mu.Unlock()
	publishAs(t, s, dc, 5)
	dc.Published.Source = nil // 模拟功能上线前的老 bundle
	code, body := verReq(t, h, "POST", "/api/config/rollback", `{"node_id":1,"version":5}`)
	if code != http.StatusConflict {
		t.Fatalf("无快照应 409: %d %s", code, body)
	}
}

func TestRollbackUnknownVersion(t *testing.T) {
	s, h, _, dc := newVersionsServer(t)
	s.admin.mu.Lock()
	s.admin.sess["smoke-sid"] = consoleIdentity{Role: roleTenant, TenantID: "tenant-vm",
		Username: "vm-tenant", expires: time.Now().Add(time.Hour)}
	s.admin.mu.Unlock()
	publishAs(t, s, dc, 5)
	code, _ := verReq(t, h, "POST", "/api/config/rollback", `{"node_id":1,"version":99}`)
	if code != http.StatusNotFound {
		t.Fatalf("未知版本应 404: %d", code)
	}
}

func TestDiffEndpointDraftVsPublished(t *testing.T) {
	s, h, _, dc := newVersionsServer(t)
	s.admin.mu.Lock()
	s.admin.sess["smoke-sid"] = consoleIdentity{Role: roleTenant, TenantID: "tenant-vm",
		Username: "vm-tenant", expires: time.Now().Add(time.Hour)}
	s.admin.mu.Unlock()
	dc.Draft.Hostname = "published-name"
	publishAs(t, s, dc, 5)
	dc.Draft.Hostname = "draft-name" // 草稿已改、未发布

	code, body := verReq(t, h, "GET", "/api/config/diff?node=1&a=5&b=draft", "")
	if code != 200 {
		t.Fatalf("diff: %d %s", code, body)
	}
	var v configDiffView
	json.Unmarshal(body, &v)
	if len(v.Segs) == 0 {
		t.Fatalf("主机名改动应至少出现在一个段里: %s", body)
	}
	joined := ""
	for _, seg := range v.Segs {
		joined += strings.Join(seg.Lines, "\n")
	}
	if !strings.Contains(joined, "+hostname draft-name") || !strings.Contains(joined, "-hostname published-name") {
		t.Fatalf("diff 应含 +/- 两行: %s", body)
	}
	// 相同选择器 → 全部段无差异。
	code, body = verReq(t, h, "GET", "/api/config/diff?node=1&a=5&b=5", "")
	if code != 200 {
		t.Fatalf("diff 自比: %d %s", code, body)
	}
	json.Unmarshal(body, &v)
	if len(v.Segs) != 0 {
		t.Fatalf("自比应为空: %s", body)
	}
}

func TestVersionsListNewestFirst(t *testing.T) {
	s, h, n, dc := newVersionsServer(t)
	s.admin.mu.Lock()
	s.admin.sess["smoke-sid"] = consoleIdentity{Role: roleTenant, TenantID: "tenant-vm",
		Username: "vm-tenant", expires: time.Now().Add(time.Hour)}
	s.admin.mu.Unlock()
	publishAs(t, s, dc, 3)
	dc.Draft.Hostname = "x"
	publishAs(t, s, dc, 4)
	// 设备自报它正在跑 v4（心跳带回来的 ConfigState）。
	n.Config = &wire.ConfigState{Version: 4}
	code, body := verReq(t, h, "GET", "/api/config/versions?node=1", "")
	if code != 200 {
		t.Fatalf("versions: %d %s", code, body)
	}
	var v configVersionsView
	json.Unmarshal(body, &v)
	if v.PublishedVersion != 4 || len(v.Versions) != 2 {
		t.Fatalf("应有两个版本行: %s", body)
	}
	if v.Versions[0].Version != 4 || v.Versions[1].Version != 3 {
		t.Fatalf("应新版本在前: %s", body)
	}
	if !v.Versions[0].IsRunning {
		t.Fatalf("设备在跑 v4（Config 报告了）——行上应标 is_running: %s", body)
	}
}
