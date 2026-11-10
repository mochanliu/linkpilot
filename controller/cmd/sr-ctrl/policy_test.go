package main

// Tests for the 静态策略 halves the console owns (O6-f): how a row is rendered
// into bundle 第 7 段, and which mistakes are refused before a box ever sees it.
//
// The properties worth pinning down are the ones that look fine on the page and
// are wrong on the box: an attach list derived from the wrong VPN, a row that
// survives as an empty attach list, a fallback row for colour 0, an ICMP row
// with ports, and a row that can never match because an earlier one is wider.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"sdwan/controller/internal/underlay"
)

func policyFixture(t *testing.T) *server {
	t.Helper()
	s := newTestServer()
	s.state.Configs = map[string]*DeviceConfig{}
	s.state.Overlays = map[string]*OverlayConfig{}
	s.store = &fileStore{path: filepath.Join(t.TempDir(), "state.json")}
	addNode(s, "t", 1, nil, "203.0.113.1:40000")
	addNode(s, "t", 2, nil, "203.0.113.2:40000")

	s.state.Configs[configKey("t", 1)] = &DeviceConfig{
		TenantID: "t", NodeID: 1, Version: 1,
		Draft: BasicConfig{
			Hostname: "cpe-1",
			VPNs:     []ConfigVPN{{ID: 0, Name: "default"}, {ID: 10, Name: "guest"}, {ID: 20, Name: "idle"}},
			Ifaces: []ConfigIface{
				// 口顺序故意和名字顺序相反：attach 必须按名字排序，否则同一份草稿
				// 会因为接口顺序变一下就渲染出不同字节。
				{VPP: "host-eth2", LCP: "c1", IP: "10.1.0.1", PrefixLen: 24, VPN: 10, MTU: 1500, Up: true},
				{VPP: "host-eth1", LCP: "c0", IP: "10.0.0.1", PrefixLen: 24, VPN: 10, MTU: 1500, Up: true},
				{VPP: "host-eth3", LCP: "c2", IP: "10.2.0.1", PrefixLen: 24, VPN: 0, MTU: 1500, Up: true},
			},
			Statics: []ConfigStatic{},
			VRRP:    []ConfigVRRP{},
		},
	}
	return s
}

func decodePolicyArtifact(t *testing.T, seg string) policyArtifactDoc {
	t.Helper()
	var doc policyArtifactDoc
	if err := json.Unmarshal([]byte(seg), &doc); err != nil {
		t.Fatalf("第 7 段不是 JSON: %v\n%s", err, seg)
	}
	return doc
}

func TestRenderPolicyAttachesEveryPortOfTheVPN(t *testing.T) {
	s := policyFixture(t)
	dc := s.state.Configs[configKey("t", 1)]
	cfg := dc.Draft
	normalizeBasicConfig(&cfg)

	set := PolicySet{
		Rows: []PolicyRow{
			{VPN: 10, Color: 2, Dst: "192.0.2.0/24", Proto: policyProtoTCP, DstPort: "443"},
			{VPN: 0, Color: 0},
			{VPN: 20, Color: 1},
		},
		Fallback: []PolicyFallback{{VPN: 10, Color: 2}},
	}
	doc := decodePolicyArtifact(t, renderPolicyArtifact(&cfg, set))
	if len(doc.Rows) != 3 {
		t.Fatalf("渲染出 %d 行，应为 3", len(doc.Rows))
	}
	if got := strings.Join(doc.Rows[0].Attach, ","); got != "host-eth1,host-eth2" {
		t.Errorf("VPN 10 的入口口 = %q，应为该 VPN 的全部客户口（按名字排序）", got)
	}
	if got := strings.Join(doc.Rows[1].Attach, ","); got != "host-eth3" {
		t.Errorf("VPN 0 的入口口 = %q", got)
	}
	if got := doc.Rows[2].Attach; len(got) != 0 {
		t.Errorf("VPN 20 没有客户口，attach 应为空，实际 %v", got)
	}
	// 行号按数组顺序派生：它就是 VPP 的匹配顺序。
	for i, r := range doc.Rows {
		if r.ID != i+1 {
			t.Errorf("第 %d 行的 id = %d，应为 %d", i+1, r.ID, i+1)
		}
	}
	if len(doc.Fallback) != 1 || doc.Fallback[0].VPN != 10 || doc.Fallback[0].Color != 2 {
		t.Errorf("兜底清单 = %+v", doc.Fallback)
	}
}

// 空策略集必须渲染出 "rows": [] —— 空数组是"把现有策略清掉"这条指令，
// 而缺这个键是"老配置包，什么都别动"。两者在盒子上是相反的。
func TestRenderPolicyEmptySetStillHasRows(t *testing.T) {
	seg := renderPolicyArtifact(&BasicConfig{}, PolicySet{})
	if !strings.Contains(seg, `"rows": []`) {
		t.Fatalf("空策略集没有渲染出 rows: []\n%s", seg)
	}
	if !strings.Contains(seg, `"fallback": []`) {
		t.Fatalf("空策略集没有渲染出 fallback: []\n%s", seg)
	}
	if seg != renderPolicyArtifact(&BasicConfig{}, PolicySet{Rows: []PolicyRow{}, Fallback: []PolicyFallback{}}) {
		t.Fatalf("nil 与空切片应渲染出同一份字节")
	}
}

func TestNormalizePolicySetRenumbersAndSorts(t *testing.T) {
	set := PolicySet{
		Rows: []PolicyRow{
			{ID: 7, VPN: 10, Color: 1, Src: " 192.0.2.0/24 "},
			{ID: 3, VPN: 10, Color: 2, Dst: " 198.51.100.0/24"},
		},
		Fallback: []PolicyFallback{{VPN: 10, Color: 3}, {VPN: 10, Color: 1}, {VPN: 10, Color: 3}},
	}
	normalizePolicySet(&set)
	if set.Rows[0].ID != 1 || set.Rows[1].ID != 2 {
		t.Fatalf("行号没按数组顺序重排: %+v", set.Rows)
	}
	if set.Rows[0].VPN != 10 || set.Rows[0].Color != 1 {
		t.Fatalf("重排动了行的内容（顺序就是匹配顺序，不能重排）: %+v", set.Rows)
	}
	if set.Rows[0].Src != "192.0.2.0/24" || set.Rows[1].Dst != "198.51.100.0/24" {
		t.Fatalf("前后空格没清: %+v", set.Rows)
	}
	if len(set.Fallback) != 2 || set.Fallback[0].Color != 1 || set.Fallback[1].Color != 3 {
		t.Fatalf("兜底应去重并按颜色排序: %+v", set.Fallback)
	}
}

// 一条 error 都不该漏：这些错误在盒子上都是"整段策略被拒"或"这一行一定不通"。
func TestPolicyChecksRefuseWhatTheBoxRefuses(t *testing.T) {
	s := policyFixture(t)
	dc := s.state.Configs[configKey("t", 1)]

	cases := []struct {
		name string
		row  PolicyRow
		want string
	}{
		{"VPN 不存在", PolicyRow{VPN: 99, Color: 1}, "没有 VPN 99"},
		{"overlay 保留表", PolicyRow{VPN: 250, Color: 1}, "overlay 保留表"},
		{"内核保留表", PolicyRow{VPN: 254, Color: 1}, "内核保留表"},
		{"颜色越界", PolicyRow{VPN: 10, Color: 8}, "颜色超出"},
		{"入口口为空", PolicyRow{VPN: 20, Color: 1}, "一个客户口都没有"},
		{"ICMP 带端口", PolicyRow{VPN: 10, Color: 1, Proto: policyProtoICMP, DstPort: "443"}, "不该有端口"},
		{"协议不认识", PolicyRow{VPN: 10, Color: 1, Proto: 47}, "协议 47 不认识"},
		{"源前缀不合法", PolicyRow{VPN: 10, Color: 1, Src: "10.0.0.0/33"}, "源前缀"},
		{"目的前缀非规范", PolicyRow{VPN: 10, Color: 1, Dst: "192.0.2.5/24"}, "不是规范网段"},
		{"端口区间颠倒", PolicyRow{VPN: 10, Color: 1, Proto: policyProtoTCP, SrcPort: "900-100"}, "不合法"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checks := s.policyChecksLocked("t", 1, dc, PolicySet{Rows: []PolicyRow{c.row}})
			if msg := hasBlockingCheck(checks); !strings.Contains(msg, c.want) {
				t.Fatalf("应报含「%s」的错误，实际 checks=%+v", c.want, checks)
			}
		})
	}
}

func TestPolicyChecksRefuseFallbackOnColourZero(t *testing.T) {
	s := policyFixture(t)
	dc := s.state.Configs[configKey("t", 1)]
	checks := s.policyChecksLocked("t", 1, dc, PolicySet{
		Rows:     []PolicyRow{{VPN: 10, Color: 1}},
		Fallback: []PolicyFallback{{VPN: 10, Color: 0}},
	})
	if msg := hasBlockingCheck(checks); !strings.Contains(msg, "颜色 0") {
		t.Fatalf("颜色 0 的兜底应被拒（盒子上会整段拒绝），实际 checks=%+v", checks)
	}
}

// 颜色上没有隧道是 warning 而不是 error：故意让某个颜色黑洞是合法配置（虽然
// 少见），挡住它会逼人先去改隧道。勾了兜底就只剩一句 info。
func TestPolicyChecksColourWithoutTunnel(t *testing.T) {
	s := policyFixture(t)
	dc := s.state.Configs[configKey("t", 1)]

	// 没有 underlay 配置时整体跳过：一台还没建隧道的设备，"颜色没有隧道"是废话。
	checks := s.policyChecksLocked("t", 1, dc, PolicySet{Rows: []PolicyRow{{VPN: 10, Color: 3}}})
	if hasBlockingCheck(checks) != "" {
		t.Fatalf("没配 underlay 时不该报错: %+v", checks)
	}

	uc := s.underlayConfigLocked("t", true)
	uc.State.Settings.UpdatedAt = 100
	uc.State.Tunnels["tun1"] = &underlay.Tunnel{
		ID: "tun1", A: underlay.Endpoint{NodeID: 1}, B: underlay.Endpoint{NodeID: 2}, Color: 3,
	}

	// 颜色 3 有隧道：不吭声。
	checks = s.policyChecksLocked("t", 1, dc, PolicySet{Rows: []PolicyRow{{VPN: 10, Color: 3}}})
	for _, c := range checks {
		if strings.Contains(c.Text, "隧道") {
			t.Fatalf("颜色 3 有隧道，不该有隧道相关的提示: %+v", checks)
		}
	}

	// 颜色 5 没有隧道、也没勾兜底：warning。
	checks = s.policyChecksLocked("t", 1, dc, PolicySet{Rows: []PolicyRow{{VPN: 10, Color: 5}}})
	if !hasSeverity(checks, "warning", "没有颜色 5") {
		t.Fatalf("应有一句「没有颜色 5 的隧道」的警告: %+v", checks)
	}
	if hasBlockingCheck(checks) != "" {
		t.Fatalf("颜色没有隧道不该挡住保存: %+v", checks)
	}

	// 勾了兜底：只剩 info，并且说明会退到主表。
	checks = s.policyChecksLocked("t", 1, dc, PolicySet{
		Rows:     []PolicyRow{{VPN: 10, Color: 5}},
		Fallback: []PolicyFallback{{VPN: 10, Color: 5}},
	})
	if !hasSeverity(checks, "info", "按兜底落到") {
		t.Fatalf("勾了兜底应说明会退到主表: %+v", checks)
	}
	if hasBlockingCheck(checks) != "" {
		t.Fatalf("不该有错误: %+v", checks)
	}
}

// 前面一行更宽 => 后面那行永远匹配不到。这条最容易被当成"两条都能用"。
func TestPolicyChecksFlagShadowedRow(t *testing.T) {
	s := policyFixture(t)
	dc := s.state.Configs[configKey("t", 1)]
	checks := s.policyChecksLocked("t", 1, dc, PolicySet{Rows: []PolicyRow{
		{VPN: 10, Color: 1},
		{VPN: 10, Color: 2, Dst: "192.0.2.0/24", Proto: policyProtoTCP, DstPort: "443"},
	}})
	if !hasSeverity(checks, "warning", "被第 1 行完全覆盖") {
		t.Fatalf("第 2 行被第 1 行覆盖，应有警告: %+v", checks)
	}

	// 反过来（窄的在前、宽的在后）不是错误：后面那行仍然匹配剩下的流量。
	checks = s.policyChecksLocked("t", 1, dc, PolicySet{Rows: []PolicyRow{
		{VPN: 10, Color: 2, Dst: "192.0.2.0/24"},
		{VPN: 10, Color: 1},
	}})
	for _, c := range checks {
		if strings.Contains(c.Text, "覆盖") {
			t.Fatalf("窄行在前不该被报覆盖: %+v", checks)
		}
	}
}

func TestPolicyChecksFlagUnusedFallback(t *testing.T) {
	s := policyFixture(t)
	dc := s.state.Configs[configKey("t", 1)]
	checks := s.policyChecksLocked("t", 1, dc, PolicySet{
		Rows:     []PolicyRow{{VPN: 10, Color: 1}},
		Fallback: []PolicyFallback{{VPN: 10, Color: 4}},
	})
	if !hasSeverity(checks, "info", "没人用") {
		t.Fatalf("兜底没有策略指向它，应有一句提示: %+v", checks)
	}
}

// 页面接线是纯字符串，写错一个 data-view 就是一页空白而没有任何报错。这条测试
// 只钉住"四面都在"：导航项、VIEWS 条目、render 分发、以及页面自己的委托监听。
func TestConsolePolicyPageIsWired(t *testing.T) {
	html := string(consoleHTML)
	for _, want := range []string{
		`data-view="policy"`,
		`policy: {`,
		`S.view === "policy"`,
		`function viewPolicy(`,
		`function plClick(`,
		`function plChange(`,
		`function plInput(`,
		`$("pl-root")`,
		`"/api/policy?node="`,
		`"/api/policy"`,
		`data-pl="save"`,
		`data-pl="publish"`,
		`data-pl-fb="`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("console.html 缺少 %s", want)
		}
	}
}

func hasSeverity(checks []ConfigCheck, severity, contains string) bool {
	for _, c := range checks {
		if c.Severity == severity && strings.Contains(c.Text, contains) {
			return true
		}
	}
	return false
}

func policyDo(t *testing.T, s *server, method, target, body string, sid string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if sid != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	}
	rec := httptest.NewRecorder()
	s.handleAdminPolicy(rec, r)
	return rec
}

func TestPutPolicySavesAndIsIdempotent(t *testing.T) {
	s := policyFixture(t)
	sid := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t", Username: "alice", Via: "password"})
	dc := s.state.Configs[configKey("t", 1)]
	before := dc.Version

	body := `{"node_id":1,"rows":[
		{"vpn":10,"color":2,"dst":"192.0.2.0/24","proto":6,"dst_port":"443"},
		{"vpn":10,"color":1}],
		"fallback":[{"vpn":10,"color":2}]}`
	rec := policyDo(t, s, http.MethodPut, "/api/policy", body, sid)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/policy = %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK      bool       `json:"ok"`
		Changed bool       `json:"changed"`
		Policy  policyView `json:"policy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Changed || dc.Version != before+1 {
		t.Fatalf("第一次保存应改版本（changed=%v, version=%d）", resp.Changed, dc.Version)
	}
	if len(dc.Policy.Rows) != 2 || dc.Policy.Rows[0].ID != 1 || dc.Policy.Rows[1].ID != 2 {
		t.Fatalf("存下来的策略集不对: %+v", dc.Policy)
	}
	if got := strings.Join(resp.Policy.Rows[0].Attach, ","); got != "host-eth1,host-eth2" {
		t.Errorf("视图里的入口口 = %q", got)
	}
	if resp.Policy.Artifact == "" {
		t.Errorf("视图应带上第 7 段原文（页面直接显示）")
	}

	// 同一份内容再存一次：版本不动，否则设备会为一次空改动重下发。
	rec = policyDo(t, s, http.MethodPut, "/api/policy", body, sid)
	if rec.Code != http.StatusOK {
		t.Fatalf("第二次 PUT = %d (%s)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Changed || dc.Version != before+1 {
		t.Fatalf("内容没变不该改版本（changed=%v, version=%d）", resp.Changed, dc.Version)
	}
}

func TestPutPolicyRefusesBlockingCheckAndKeepsStored(t *testing.T) {
	s := policyFixture(t)
	sid := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t", Username: "alice", Via: "password"})
	dc := s.state.Configs[configKey("t", 1)]
	dc.Policy = PolicySet{Rows: []PolicyRow{{VPN: 10, Color: 1}}}
	normalizePolicySet(&dc.Policy)
	want := policySetCopy(dc.Policy)

	// VPN 20 在这台设备上一个客户口都没有 => 盒子会整段拒绝 => 保存就该挡住。
	body := `{"node_id":1,"rows":[{"vpn":20,"color":1}]}`
	rec := policyDo(t, s, http.MethodPut, "/api/policy", body, sid)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT 应被拒（400），实际 %d (%s)", rec.Code, rec.Body.String())
	}
	if !policySetEqual(dc.Policy, want) {
		t.Fatalf("被拒的保存不该改动已存的策略集: %+v", dc.Policy)
	}
}

func TestPutPolicyRequiresSavedDraft(t *testing.T) {
	s := policyFixture(t)
	sid := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t", Username: "alice", Via: "password"})
	// 设备 2 没有草稿。
	body := `{"node_id":2,"rows":[{"vpn":0,"color":1}]}`
	rec := policyDo(t, s, http.MethodPut, "/api/policy", body, sid)
	if rec.Code != http.StatusConflict {
		t.Fatalf("v0 设备应回 409，实际 %d (%s)", rec.Code, rec.Body.String())
	}
}

// 发布只看字节：策略没改，第二次发布不该动版本（不然每次点发布设备都要重下发）。
func TestPublishPolicyArtifactOnlyWhenChanged(t *testing.T) {
	s := policyFixture(t)
	dc := s.state.Configs[configKey("t", 1)]
	dc.Policy = PolicySet{Rows: []PolicyRow{{VPN: 10, Color: 2}}}
	normalizePolicySet(&dc.Policy)

	if !s.publishConfigLocked("t", 1, dc, nil, "alice", 1000) {
		t.Fatalf("第一次发布应该算改动")
	}
	if s.publishConfigLocked("t", 1, dc, nil, "alice", 2000) {
		t.Fatalf("内容没变的第二次发布不该算改动")
	}
	// 加一条策略行必须产生新版本，否则设备那边会因为"同版本不同内容"拒收。
	dc.Policy.Rows = append(dc.Policy.Rows, PolicyRow{VPN: 10, Color: 1})
	normalizePolicySet(&dc.Policy)
	if !s.publishConfigLocked("t", 1, dc, nil, "alice", 3000) {
		t.Fatalf("改了一条策略之后发布必须算改动")
	}
	if dc.Published == nil || !strings.Contains(dc.Published.Artifacts.Policy, `"color": 1`) {
		t.Fatalf("发布的第 7 段里没有新加的那一行: %+v", dc.Published)
	}
}

// 基础配置里的客户口变了，第 7 段的 attach 必须跟着变 —— 否则设备会把策略挂在
// 已经不存在的口上（sr-apply 会整段拒绝）。
func TestPolicyArtifactFollowsCustomerPorts(t *testing.T) {
	s := policyFixture(t)
	dc := s.state.Configs[configKey("t", 1)]
	dc.Policy = PolicySet{Rows: []PolicyRow{{VPN: 10, Color: 1}}}

	first := s.artifactsLocked("t", 1, &dc.Draft, dc.Leak, dc.Policy).Policy
	dc.Draft.Ifaces = dc.Draft.Ifaces[:1] // 只留 host-eth2
	second := s.artifactsLocked("t", 1, &dc.Draft, dc.Leak, dc.Policy).Policy
	if first == second {
		t.Fatalf("客户口变了，第 7 段应该跟着变")
	}
	if strings.Contains(second, "host-eth1") {
		t.Fatalf("删掉的客户口还在 attach 里:\n%s", second)
	}
}
