package underlay

import (
	"fmt"
	"strings"
	"testing"
)

func planTunnels(p Plan) map[string]Tunnel {
	out := map[string]Tunnel{}
	for _, t := range p.Add {
		out[pairKey(t.A.NodeID, t.B.NodeID)] = t
	}
	return out
}

func pairKey(a, b uint16) string {
	if a > b {
		a, b = b, a
	}
	return fmt.Sprintf("%d-%d", a, b)
}

func skipReason(p Plan, a, b uint16) (string, bool) {
	want := pairKey(a, b)
	for _, s := range p.Skip {
		if pairKey(s.A, s.B) == want {
			return s.Reason, true
		}
	}
	return "", false
}

func mustNotHavePair(t *testing.T, s *State, a, b uint16) {
	t.Helper()
	if s.HasPair(a, b) {
		t.Fatalf("不该存在 %s 的隧道", pairKey(a, b))
	}
}

func TestFullMeshIsAllDirectPairs(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	plan, err := s.AutoApply(AutoRequest{Nodes: []uint16{1, 2, 3, 4}, Mode: ModeFullMesh, Now: 1000}, testViews(1, 2, 3, 4))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Add) != 6 || len(plan.Skip) != 0 || plan.Pairs() != 6 {
		t.Fatalf("4 台全互联应有 6 对，实际 add=%d skip=%d", len(plan.Add), len(plan.Skip))
	}
	for _, tun := range plan.Add {
		if tun.Path != nil {
			t.Fatalf("%s 是全互联的直连隧道，不该有路径", tun.ID)
		}
		if tun.Roles.A != "" || tun.Roles.B != "" {
			t.Fatalf("全互联没有 hub/spoke 角色，实际 %+v", tun.Roles)
		}
		if tun.MTU != agentBudget(1500) {
			t.Fatalf("%s 的 MTU 应是设备侧强制的最坏预算 %d，实际 %d", tun.ID, agentBudget(1500), tun.MTU)
		}
	}
	if len(s.TransitPorts) != 0 {
		t.Fatalf("全互联没有中转，不该分配中转端口: %v", s.TransitPorts)
	}
	// Duplicate detection is at the pair level, so the second identical batch is
	// a no-op that still reports every pair instead of silently doing nothing.
	again, err := s.AutoApply(AutoRequest{Nodes: []uint16{1, 2, 3, 4}, Mode: ModeFullMesh, Now: 2000}, testViews(1, 2, 3, 4))
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Add) != 0 || len(again.Skip) != 6 {
		t.Fatalf("重复批次应全部跳过，实际 add=%d skip=%d", len(again.Add), len(again.Skip))
	}
	for _, sk := range again.Skip {
		if !strings.Contains(sk.Reason, "已经有隧道") {
			t.Fatalf("跳过原因应是重复对儿，实际 %q", sk.Reason)
		}
	}
	if len(s.Tunnels) != 6 {
		t.Fatalf("重复批次改动了隧道表: %d", len(s.Tunnels))
	}
}

func TestHubSpokeWithoutSpokeTunnels(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	plan, err := s.AutoApply(AutoRequest{
		Nodes: []uint16{1, 2, 3, 4}, Mode: ModeHubSpoke, Hub: 2, SpokeMode: SpokeNone, Now: 1000,
	}, testViews(1, 2, 3, 4))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Add) != 3 || len(plan.Skip) != 0 {
		t.Fatalf("hub + 3 spoke 的“无”模式应只建 3 条，实际 add=%d skip=%d", len(plan.Add), len(plan.Skip))
	}
	for _, tun := range plan.Add {
		if tun.A.NodeID != 2 && tun.B.NodeID != 2 {
			t.Fatalf("%s 没有 hub 参与，不该在“无”模式里", tun.ID)
		}
		if tun.Path != nil {
			t.Fatalf("%s 应是直连", tun.ID)
		}
		hubSide, spokeSide := tun.Roles.A, tun.Roles.B
		if tun.B.NodeID == 2 {
			hubSide, spokeSide = tun.Roles.B, tun.Roles.A
		}
		if hubSide != "hub" || spokeSide != "spoke" {
			t.Fatalf("%s 的角色不对: %+v", tun.ID, tun.Roles)
		}
	}
	if _, found := planTunnels(plan)["3-4"]; found {
		t.Fatal("“无”模式不该建 spoke 之间的隧道")
	}
}

func TestHubSpokeViaHubIsFullMesh(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	plan, err := s.AutoApply(AutoRequest{
		Nodes: []uint16{1, 2, 3, 4}, Mode: ModeHubSpoke, Hub: 1, SpokeMode: SpokeViaHub, Now: 1000,
	}, testViews(1, 2, 3, 4))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Add) != 6 || len(plan.Skip) != 0 {
		t.Fatalf("4 台 full mesh 应有 6 条，实际 add=%d skip=%d", len(plan.Add), len(plan.Skip))
	}
	byPair := planTunnels(plan)
	for _, key := range []string{"1-2", "1-3", "1-4"} {
		tun, ok := byPair[key]
		if !ok {
			t.Fatalf("缺少 hub 直连 %s", key)
		}
		if tun.Path != nil {
			t.Fatalf("%s 是 hub<->spoke，应直连", key)
		}
		if tun.MTU != agentBudget(1500) {
			t.Fatalf("%s 直连 MTU 不对: %d，应是 %d", key, tun.MTU, agentBudget(1500))
		}
	}
	for _, key := range []string{"2-3", "2-4", "3-4"} {
		tun, ok := byPair[key]
		if !ok {
			t.Fatalf("缺少 spoke 间隧道 %s", key)
		}
		if tun.Path == nil || tun.Path.TemplateID == "" {
			t.Fatalf("%s 应经 hub 中转并引用共享模板: %+v", key, tun.Path)
		}
		// Same number as the direct tunnels: the interface MTU is bounded by the
		// worst path the dataplane can build, not by this tunnel's hop count.
		if tun.MTU != agentBudget(1500) {
			t.Fatalf("%s 一跳 MTU 不对: %d，应是 %d", key, tun.MTU, agentBudget(1500))
		}
	}
	// One template for the whole batch: editing it moves every relayed tunnel.
	if len(s.Paths) != 1 {
		t.Fatalf("一批 spoke 间隧道应共用一个模板，实际 %d 个", len(s.Paths))
	}
	for _, tpl := range s.Paths {
		if tpl.NodeID != 1 {
			t.Fatalf("模板的中转设备应是 hub 1，实际 %d", tpl.NodeID)
		}
		if tpl.Name != "经 dev1 中转" {
			t.Fatalf("模板名应带设备名，实际 %q", tpl.Name)
		}
	}
	if s.TransitPorts[1] != 15000 || len(s.TransitPorts) != 1 {
		t.Fatalf("只有 hub 需要中转端口，实际 %v", s.TransitPorts)
	}
}

func TestHubOnlyTransitBuildsSpokeMeshWithoutHubTunnel(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	plan, err := s.AutoApply(AutoRequest{
		Nodes: []uint16{1, 2, 3, 4}, Mode: ModeHubSpoke, Hub: 1, SpokeMode: SpokeHubTransit, Now: 1000,
	}, testViews(1, 2, 3, 4))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Add) != 3 || len(plan.Skip) != 0 {
		t.Fatalf("3 台 spoke 应有 3 条，实际 add=%d skip=%d", len(plan.Add), len(plan.Skip))
	}
	for _, tun := range plan.Add {
		if tun.A.NodeID == 1 || tun.B.NodeID == 1 {
			t.Fatalf("%s 涉及 hub，但“hub 仅中转”不该建 hub 的隧道", tun.ID)
		}
		if tun.Path == nil || tun.Path.TemplateID == "" {
			t.Fatalf("%s 应经 hub 中转: %+v", tun.ID, tun.Path)
		}
	}
	for _, key := range []string{"2-3", "2-4", "3-4"} {
		if _, ok := planTunnels(plan)[key]; !ok {
			t.Fatalf("缺少 spoke 间隧道 %s", key)
		}
	}
	if s.TransitPorts[1] != 15000 {
		t.Fatalf("hub 仍要分配中转端口，实际 %v", s.TransitPorts)
	}
	mustNotHavePair(t, s, 1, 2)
}

// A preview must be free of side effects and must predict the exact numbers the
// apply will hand out -- otherwise the operator confirms one thing and gets
// another.
func TestAutoPlanDoesNotCommitAndMatchesApply(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2, 3)
	req := AutoRequest{Nodes: []uint16{1, 2, 3}, Mode: ModeHubSpoke, Hub: 1, SpokeMode: SpokeViaHub, Now: 1000}

	preview, err := s.AutoPlan(req, views)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Tunnels) != 0 || len(s.Paths) != 0 || len(s.TransitPorts) != 0 || s.Seq != 0 || s.OverlayNext != 0 {
		t.Fatalf("AutoPlan 改了真实状态: tunnels=%d paths=%d transit=%v seq=%d next=%d",
			len(s.Tunnels), len(s.Paths), s.TransitPorts, s.Seq, s.OverlayNext)
	}

	applied, err := s.AutoApply(req, views)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Add) != len(preview.Add) {
		t.Fatalf("预览 %d 条，实际 %d 条", len(preview.Add), len(applied.Add))
	}
	pa, aa := planTunnels(preview), planTunnels(applied)
	for key, p := range pa {
		a, ok := aa[key]
		if !ok {
			t.Fatalf("预览有 %s，实际没有", key)
		}
		if p.ID != a.ID || p.A.Port != a.A.Port || p.B.Port != a.B.Port ||
			p.A.Overlay != a.A.Overlay || p.B.Overlay != a.B.Overlay || p.MTU != a.MTU {
			t.Fatalf("%s 预览与实际不一致:\n预览 %+v\n实际 %+v", key, p, a)
		}
	}
	if len(s.Tunnels) != 3 {
		t.Fatalf("应用后应有 3 条隧道，实际 %d", len(s.Tunnels))
	}
}

func TestAutoSkipsAreExplained(t *testing.T) {
	views := testViews(1, 2, 3, 4)
	// node 4: offline; 3: no observed address; 2: symmetric together with 1.
	v := views[2]
	v.Symmetric = true
	views[2] = v
	v = views[1]
	v.Symmetric = true
	views[1] = v
	v = views[3]
	v.Observed = ""
	views[3] = v
	v = views[4]
	v.Stale = true
	views[4] = v

	s := newTestState(t, "10.9.0.0/24")
	plan, err := s.AutoApply(AutoRequest{Nodes: []uint16{1, 2, 3, 4}, Mode: ModeFullMesh, Now: 1000}, views)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"1-2": "对称",
		"1-3": "外层地址",
		"1-4": "离线",
		"2-3": "外层地址",
		"2-4": "离线",
		"3-4": "离线",
	}
	for key, frag := range want {
		var a, b uint16
		fmt.Sscanf(strings.ReplaceAll(key, "-", " "), "%d %d", &a, &b)
		reason, ok := skipReason(plan, a, b)
		if !ok {
			t.Fatalf("%s 应被跳过，实际建了", key)
		}
		if !strings.Contains(reason, frag) {
			t.Fatalf("%s 的跳过原因应含 %q，实际 %q", key, frag, reason)
		}
	}
	if len(plan.Add) != 0 || len(s.Tunnels) != 0 {
		t.Fatalf("全部不可用时不建隧道，实际 add=%d tunnels=%d", len(plan.Add), len(s.Tunnels))
	}
}

func TestAutoSkipsNodesWithoutPhysicalPort(t *testing.T) {
	views := testViews(1, 2, 3)
	v := views[3]
	v.DefaultIface = ""
	views[3] = v
	s := newTestState(t, "10.9.0.0/24")
	plan, err := s.AutoApply(AutoRequest{Nodes: []uint16{1, 2, 3}, Mode: ModeFullMesh, Now: 1000}, views)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := planTunnels(plan)["1-2"]; !ok {
		t.Fatal("没有物理端口问题的 1-2 应该建起来")
	}
	for _, key := range []string{"1-3", "2-3"} {
		var a, b uint16
		fmt.Sscanf(strings.ReplaceAll(key, "-", " "), "%d %d", &a, &b)
		reason, ok := skipReason(plan, a, b)
		if !ok || !strings.Contains(reason, "物理端口") {
			t.Fatalf("%s 应因缺少物理端口被跳过，实际 %q", key, reason)
		}
	}
}

func TestAutoSkipsUnknownDevice(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	// Device 9 was selected but never enrolled.
	plan, err := s.AutoApply(AutoRequest{Nodes: []uint16{1, 2, 9}, Mode: ModeFullMesh, Now: 1000}, testViews(1, 2))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := planTunnels(plan)["1-2"]; !ok {
		t.Fatal("已知设备之间的隧道应照建")
	}
	reason, ok := skipReason(plan, 2, 9)
	if !ok || !strings.Contains(reason, "不存在") {
		t.Fatalf("未入网设备应被跳过并说明，实际 %q", reason)
	}
}

func TestViaHubSkipsSpokePairsWhenHubIsOffline(t *testing.T) {
	views := testViews(1, 2, 3)
	v := views[1]
	v.Stale = true
	views[1] = v
	s := newTestState(t, "10.9.0.0/24")
	plan, err := s.AutoApply(AutoRequest{
		Nodes: []uint16{1, 2, 3}, Mode: ModeHubSpoke, Hub: 1, SpokeMode: SpokeViaHub, Now: 1000,
	}, views)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Add) != 0 {
		t.Fatalf("hub 离线时不该建出隧道: %+v", plan.Add)
	}
	for _, b := range []uint16{2, 3} {
		reason, _ := skipReason(plan, 1, b)
		if !strings.Contains(reason, "离线") {
			t.Fatalf("hub<->%d 应报设备离线，实际 %q", b, reason)
		}
	}
	if reason, _ := skipReason(plan, 2, 3); !strings.Contains(reason, "中转设备离线") {
		t.Fatalf("spoke 间应报中转设备离线，实际 %q", reason)
	}
	if len(s.TransitPorts) != 0 {
		t.Fatalf("失败批次不该留下中转端口: %v", s.TransitPorts)
	}
}

func TestAutoRejectsBadRequestsWithoutTouchingState(t *testing.T) {
	views := testViews(1, 2, 3)
	cases := []struct {
		name string
		req  AutoRequest
		want string
	}{
		{"设备太少", AutoRequest{Nodes: []uint16{1}, Mode: ModeFullMesh}, "至少选择 2 台"},
		{"未知模型", AutoRequest{Nodes: []uint16{1, 2}, Mode: "star"}, "未知的模型"},
		{"没选 hub", AutoRequest{Nodes: []uint16{1, 2}, Mode: ModeHubSpoke}, "hub"},
		{"hub 不在选中设备里", AutoRequest{Nodes: []uint16{1, 2}, Mode: ModeHubSpoke, Hub: 3}, "hub"},
		{"未知 spoke 模式", AutoRequest{Nodes: []uint16{1, 2}, Mode: ModeHubSpoke, Hub: 1, SpokeMode: "x"}, "spoke 间隧道模式"},
		{"hub 仅中转只有一个 spoke", AutoRequest{Nodes: []uint16{1, 2}, Mode: ModeHubSpoke, Hub: 1, SpokeMode: SpokeHubTransit}, "至少需要 2 台 spoke"},
	}
	for _, tc := range cases {
		s := newTestState(t, "10.9.0.0/24")
		tc.req.Now = 1000
		if _, err := s.AutoApply(tc.req, views); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: 期望含 %q 的错误，实际 %v", tc.name, tc.want, err)
		}
		if len(s.Tunnels) != 0 || len(s.Paths) != 0 || s.Seq != 0 {
			t.Fatalf("%s: 失败请求改动了状态", tc.name)
		}
	}
}

// A pool shortage skips the pair instead of failing the whole batch: an
// operator who selected 20 devices should still get as many tunnels as the
// configured pools can hold.
func TestAutoSkipsExhaustedPoolsInsteadOfFailing(t *testing.T) {
	s := newTestState(t, "10.9.0.0/30") // two overlay addresses
	plan, err := s.AutoApply(AutoRequest{Nodes: []uint16{1, 2, 3}, Mode: ModeFullMesh, Now: 1000}, testViews(1, 2, 3))
	if err != nil {
		t.Fatalf("池子不足不该让整批失败: %v", err)
	}
	if len(plan.Add) != 1 || len(plan.Skip) != 2 {
		t.Fatalf("3 对里只装得下 1 对，实际 add=%d skip=%d", len(plan.Add), len(plan.Skip))
	}
	for _, sk := range plan.Skip {
		if !strings.Contains(sk.Reason, "overlay") {
			t.Fatalf("跳过原因应说明 overlay 池耗尽，实际 %q", sk.Reason)
		}
	}
	if len(s.Tunnels) != 1 {
		t.Fatalf("应保留成功的一对，实际 %d 条", len(s.Tunnels))
	}
}

func TestAutoSkipsWhenPortPoolIsExhausted(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	cfg := s.Settings
	cfg.VPNPorts = PortRange{Min: 20000, Max: 20000} // one port per device
	if err := s.SetSettings(cfg); err != nil {
		t.Fatal(err)
	}
	plan, err := s.AutoApply(AutoRequest{Nodes: []uint16{1, 2, 3}, Mode: ModeFullMesh, Now: 1000}, testViews(1, 2, 3))
	if err != nil {
		t.Fatalf("端口耗尽不该让整批失败: %v", err)
	}
	if len(plan.Add) != 1 || len(plan.Skip) != 2 {
		t.Fatalf("每台设备只有一个端口时只建得成 1 对，实际 add=%d skip=%d", len(plan.Add), len(plan.Skip))
	}
	for _, sk := range plan.Skip {
		if !strings.Contains(sk.Reason, "端口池") {
			t.Fatalf("跳过原因应说明端口池耗尽，实际 %q", sk.Reason)
		}
	}
}

// A hard error (here: an underlay MTU that cannot carry the SR header at all)
// aborts the batch and leaves no partial result behind.
func TestAutoApplyIsAtomicOnHardError(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2, 3)
	for id := range views {
		v := views[id]
		v.Ifaces = map[string]uint16{"eth0": 400}
		views[id] = v
	}
	if _, err := s.AutoApply(AutoRequest{Nodes: []uint16{1, 2, 3}, Mode: ModeFullMesh, Now: 1000}, views); err == nil {
		t.Fatal("underlay MTU 太小应报错")
	}
	if len(s.Tunnels) != 0 || s.Seq != 0 || s.OverlayNext != 0 || len(s.Paths) != 0 {
		t.Fatalf("失败的批次留下了半成品: tunnels=%d seq=%d next=%d paths=%d",
			len(s.Tunnels), s.Seq, s.OverlayNext, len(s.Paths))
	}
}

// The auto path template is shared across batches through the same hub, so a
// later edit reaches tunnels created by either batch.
func TestAutoReusesHubTemplateAcrossBatches(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2, 3, 4)
	if _, err := s.AutoApply(AutoRequest{
		Nodes: []uint16{1, 2, 3}, Mode: ModeHubSpoke, Hub: 1, SpokeMode: SpokeViaHub, Now: 1000,
	}, views); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AutoApply(AutoRequest{
		Nodes: []uint16{1, 2, 4}, Mode: ModeHubSpoke, Hub: 1, SpokeMode: SpokeViaHub, Now: 2000,
	}, views); err != nil {
		t.Fatal(err)
	}
	if len(s.Paths) != 1 {
		t.Fatalf("同一 hub 的第二批应复用模板，实际 %d 个", len(s.Paths))
	}
	var tplID string
	for id := range s.Paths {
		tplID = id
	}
	// 1-2 existed already; 1-4 is direct; the only new relayed pair is 2-4.
	if users := s.PathUsers(tplID); len(users) != 2 {
		t.Fatalf("两个批次的 spoke 间隧道都应挂在这个模板上，实际 %v", users)
	}
}

func TestAutoDedupesNodes(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	plan, err := s.AutoApply(AutoRequest{
		Nodes: []uint16{1, 0, 2, 1}, Mode: ModeFullMesh, Now: 1000,
	}, testViews(1, 2))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Pairs() != 1 || len(plan.Add) != 1 {
		t.Fatalf("重复和 0 号设备应被去掉，实际 %d 对", plan.Pairs())
	}
}
