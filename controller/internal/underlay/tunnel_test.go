package underlay

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"sdwan/controller/internal/keybinding"
	"sdwan/controller/internal/wire"
)

// agentBudget is the ceiling a device will accept: the budget for the
// dataplane's WORST-CASE segment list, which internal/agent NewLink enforces.
// Anything the controller hands out above it does not fail loudly -- the agent
// refuses the tunnel, which is what the 2026-10-28 lab e2e hit when MTU came
// from the current hop count (1440 handed out, 1368 allowed on a 1500 underlay).
func agentBudget(underlayMTU uint16) uint16 {
	return wire.TunnelMTUBudget(underlayMTU, wire.MaxSRSegments)
}

func TestCreateDirectTunnel(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2)
	tun := mustCreate(t, s, views, 1, 2, nil)

	if tun.A.NodeID != 1 || tun.B.NodeID != 2 {
		t.Fatalf("端点不对: %+v", tun)
	}
	if tun.A.Iface != "eth0" {
		t.Fatalf("应默认用设备上报的物理端口 eth0，实际 %q", tun.A.Iface)
	}
	// The observed endpoint is the reflector's address for the node. It is what
	// the peer is dialed at and must be carried through untouched.
	if tun.B.Addr != "203.0.113.2" {
		t.Fatalf("没填地址时应使用观测值，实际 %q", tun.B.Addr)
	}
	if tun.MTU != agentBudget(1500) {
		t.Fatalf("直连隧道的 MTU 也必须是设备侧的最坏预算 %d，实际 %d", agentBudget(1500), tun.MTU)
	}
	if tun.Path != nil {
		t.Fatal("直连隧道不该有路径")
	}
}

func TestCreateRelayTunnelKeepsTheSameInterfaceMTU(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2, 3)
	tpl, err := s.CreatePath("", hopList(3), 1000)
	if err != nil {
		t.Fatal(err)
	}
	tun := mustCreate(t, s, views, 1, 2, &TunnelPath{TemplateID: tpl.ID})
	if tun.Path == nil || tun.Path.TemplateID != tpl.ID {
		t.Fatalf("路径没记下来: %+v", tun.Path)
	}
	// A relay hop does NOT lower the number: the interface has one MTU and the
	// segment list is rewritten at will, so both a direct and a relayed tunnel
	// start at the budget for the maximum path the dataplane can build.
	if tun.MTU != agentBudget(1500) {
		t.Fatalf("一跳路径的 MTU 应是 %d，实际 %d", agentBudget(1500), tun.MTU)
	}
	if agentBudget(1500) > wire.TunnelMTUBudget(1500, 1) {
		t.Fatal("最坏预算不该高于短路径预算")
	}
}

func TestCreateChecks(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2, 3)

	cases := []struct {
		name string
		req  CreateRequest
		want string
	}{
		{"两端同机", CreateRequest{A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 1}}, "同一台设备"},
		{"设备未入网", CreateRequest{A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 9}}, "不存在或未入网"},
		{"接口不存在", CreateRequest{
			A: EndpointSpec{NodeID: 1, Iface: "eth9"}, B: EndpointSpec{NodeID: 2}}, "没有接口"},
		{"中转就是端点", CreateRequest{
			A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2},
			Path: via(1)}, "同时是隧道的一端"},
		{"第 2 跳是端点", CreateRequest{
			A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2},
			Path: via(3, 2)}, "第 2 跳"},
		{"中转设备未入网", CreateRequest{
			A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2},
			Path: via(3, 77)}, "不存在或未入网"},
		{"同一条链绕回同一台", CreateRequest{
			A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2},
			Path: via(3, 5, 3)}, "出现了两次"},
		{"超过跳数上限", CreateRequest{
			A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2},
			Path: via(3, 5, 7, 12)}, "最多 3 跳"},
		{"空路径", CreateRequest{
			A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2},
			Path: &TunnelPath{}}, "至少一个中转设备"},
		{"模板不存在", CreateRequest{
			A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2},
			Path: &TunnelPath{TemplateID: "p99"}}, "不存在"},
		{"端口不在池内", CreateRequest{
			A: EndpointSpec{NodeID: 1, Port: 999}, B: EndpointSpec{NodeID: 2}}, "不在 VPN 端口池"},
	}
	for _, tc := range cases {
		tc.req.Now = 1000
		if _, err := s.Create(tc.req, views); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: 期望含 %q 的错误，实际 %v", tc.name, tc.want, err)
		}
	}
	if len(s.Tunnels) != 0 {
		t.Fatalf("失败请求留下了隧道: %d", len(s.Tunnels))
	}
}

func TestCreateRequiresAnOuterAddress(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2)
	v := views[2]
	v.Observed = ""
	views[2] = v
	if _, err := s.Create(CreateRequest{A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2}, Now: 1000}, views); err == nil {
		t.Fatal("没有观测地址也没手填，竟然建出来了")
	}
	// A hand-filled address makes it possible.
	if _, err := s.Create(CreateRequest{
		A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2, Addr: "198.51.100.7:4000"}, Now: 1000,
	}, views); err != nil {
		t.Fatalf("手填地址仍然失败: %v", err)
	}
}

func TestCreateRejectsTwoSymmetricEnds(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2)
	for _, id := range []uint16{1, 2} {
		v := views[id]
		v.Symmetric = true
		views[id] = v
	}
	if _, err := s.Create(CreateRequest{A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2}, Now: 1000}, views); err == nil {
		t.Fatal("两端对称 NAT 竟然建出来了")
	}
}

// No device has reported a link MTU, so the tunnel carries none: delivery falls
// back to wire.DefaultTunnelMTU and labels it as the default instead of
// pretending a number was computed.
func TestUnknownIfaceMTULeavesMTUUnset(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2)
	for _, id := range []uint16{1, 2} {
		v := views[id]
		v.Ifaces = map[string]uint16{"eth0": 0}
		views[id] = v
	}
	tun := mustCreate(t, s, views, 1, 2, nil)
	if tun.MTU != 0 {
		t.Fatalf("MTU 没上报时应留 0（未知），实际 %d", tun.MTU)
	}
}

// One side reporting is enough to bound the tunnel, and it is the side that did
// NOT report that skips the device check: a default 1360 on a 1450-byte link is
// what the agent rejects, so the known value has to win even when its peer is
// silent.
func TestOneSidedIfaceMTUStillBoundsTunnel(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2)
	v := views[2]
	v.Ifaces = map[string]uint16{"eth0": 1450}
	views[2] = v
	w := views[1]
	w.Ifaces = nil
	views[1] = w

	tun := mustCreate(t, s, views, 1, 2, nil)
	if want := agentBudget(1450); tun.MTU != want {
		t.Fatalf("只有一端上报时应按它算：%d，实际 %d", want, tun.MTU)
	}
}

func TestUpdateAndDelete(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2, 3)
	tun := mustCreate(t, s, views, 1, 2, nil)
	directMTU := tun.MTU
	tpl, _ := s.CreatePath("", hopList(3), 1000)

	upd, err := s.Update(tun.ID, UpdateRequest{Path: &TunnelPath{TemplateID: tpl.ID}, Now: 2000}, views)
	if err != nil {
		t.Fatal(err)
	}
	if upd.Path == nil || upd.Path.TemplateID != tpl.ID {
		t.Fatalf("路径没改上: %+v", upd.Path)
	}
	if upd.MTU != directMTU {
		t.Fatalf("加了一跳不该改接口 MTU（最坏预算与路径无关）：%d -> %d", directMTU, upd.MTU)
	}
	if upd.A.Port != tun.A.Port || upd.B.Port != tun.B.Port {
		t.Fatal("没要求改端口，端口却动了")
	}

	// Two hops on the same tunnel: an update has to carry the WHOLE chain. A
	// resolve that returned only the first hop would save a 2-hop edit as a 1-hop
	// route and the device would install it without complaining.
	wide := testViews(1, 2, 3, 5)
	two, err := s.Update(tun.ID, UpdateRequest{Path: via(3, 5), Now: 2500}, wide)
	if err != nil {
		t.Fatal(err)
	}
	if len(two.Path.Hops) != 2 || two.Path.Hops[0].NodeID != 3 || two.Path.Hops[1].NodeID != 5 {
		t.Fatalf("两跳路径没整条存下来: %+v", two.Path)
	}
	if s.TransitPorts[5] != 15000 {
		t.Fatalf("第二跳没拿到中转端口: %v", s.TransitPorts)
	}

	// Back to direct without touching anything else.
	back, err := s.Update(tun.ID, UpdateRequest{ClearPath: true, Now: 3000}, views)
	if err != nil {
		t.Fatal(err)
	}
	if back.Path != nil || back.MTU != directMTU {
		t.Fatalf("回到直连失败: path=%+v mtu=%d", back.Path, back.MTU)
	}
	// The relay device keeps its transit port: it may still serve other tunnels.
	if s.TransitPorts[3] != 15000 {
		t.Fatalf("中转端口被回收了: %v", s.TransitPorts)
	}

	if err := s.Delete(tun.ID); err != nil {
		t.Fatal(err)
	}
	if len(s.Tunnels) != 0 {
		t.Fatal("删除后隧道还在")
	}
	if got := s.OverlayFreed; len(got) != 2 {
		t.Fatalf("删除应把两个 overlay 放回队尾，实际 %v", got)
	}
	if err := s.Delete(tun.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除应返回 ErrNotFound，实际 %v", err)
	}
}

// The M-3 gate is open: a device pair may hold up to keybinding.WGKeySlots
// tunnels (same-color multi-tunnel is what ECMP/UCMP runs on). The pair's n-th
// tunnel takes key slot n on BOTH ends -- derived at creation, never typed --
// and the first tunnel of a pair always lands on slot 0, where each device's
// pre-slot key already lives, so old single-tunnel pairs cost no re-handshake.
// Deleting a tunnel frees its slot; the next create reuses the lowest free one.
func TestSamePairTunnelsGetDistinctKeySlots(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2)
	first := mustCreate(t, s, views, 1, 2, nil)
	if first.Slot != 0 {
		t.Fatalf("同对端第一条隧道应落在预置槽 0，实际 %d", first.Slot)
	}
	second := mustCreate(t, s, views, 2, 1, nil)
	if second.Slot != 1 {
		t.Fatalf("同对端第二条隧道应拿槽 1，实际 %d", second.Slot)
	}
	if len(s.Tunnels) != 2 {
		t.Fatalf("同对端第二条隧道现在应被接受: %d", len(s.Tunnels))
	}
	// Deleting the first frees slot 0; the next create reuses it instead of
	// climbing to 2 (slots stay dense, live tunnels are never renumbered).
	if err := s.Delete(first.ID); err != nil {
		t.Fatal(err)
	}
	third := mustCreate(t, s, views, 1, 2, nil)
	if third.Slot != 0 {
		t.Fatalf("删掉槽 0 的隧道后新建应复用最小空槽 0，实际 %d", third.Slot)
	}
}

// The cap is the key count itself: WGKeySlots tunnels per pair, then ErrPairFull.
func TestSamePairTunnelsCapAtKeySlots(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2)
	for i := 0; i < keybinding.WGKeySlots; i++ {
		if _, err := s.Create(CreateRequest{A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2}, Now: int64(1000 + i)}, views); err != nil {
			t.Fatalf("第 %d 条应能建: %v", i+1, err)
		}
	}
	_, err := s.Create(CreateRequest{A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2}, Now: 9999}, views)
	if err == nil || !errors.Is(err, ErrPairFull) {
		t.Fatalf("超过 %d 把密钥后应拒绝并返回 ErrPairFull，实际 %v", keybinding.WGKeySlots, err)
	}
	if len(s.Tunnels) != keybinding.WGKeySlots {
		t.Fatalf("被拒的请求不该留下隧道: %d", len(s.Tunnels))
	}
	// Freeing one slot makes room again.
	last := make([]string, 0)
	for id := range s.Tunnels {
		last = append(last, id)
	}
	sort.Strings(last)
	if err := s.Delete(last[len(last)-1]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(CreateRequest{A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2}, Now: 10000}, views); err != nil {
		t.Fatalf("腾出一个槽后应能再建: %v", err)
	}
}

func TestUpdateRejectsPortCollisionOnSameDevice(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2, 3)
	mustCreate(t, s, views, 1, 2, nil)       // node1 port 20000
	t2 := mustCreate(t, s, views, 1, 3, nil) // node1 port 20001
	if _, err := s.Update(t2.ID, UpdateRequest{PortA: 20000, Now: 2000}, views); err == nil {
		t.Fatal("把第二条隧道改到已被占用的端口，竟然通过了")
	}
}

func TestPathTemplateFanOut(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2, 3, 4, 5)
	tpl, _ := s.CreatePath("主用路径", hopList(3), 1000)
	t1 := mustCreate(t, s, views, 1, 2, &TunnelPath{TemplateID: tpl.ID})
	t2 := mustCreate(t, s, views, 1, 4, &TunnelPath{TemplateID: tpl.ID})

	if users := s.PathUsers(tpl.ID); len(users) != 2 {
		t.Fatalf("模板应被 2 条隧道使用，实际 %v", users)
	}
	if err := s.DeletePath(tpl.ID); !errors.Is(err, ErrPathInUse) {
		t.Fatalf("在用模板删除应被拒绝，实际 %v", err)
	}
	// Pointing the shared path at a device that is one of the ends must fail for
	// the whole fan-out, not just the first tunnel.
	if err := s.UpdatePath(tpl.ID, "", hopList(1)); err == nil {
		t.Fatal("把中转设备改成隧道端点，竟然通过了")
	}
	if err := s.UpdatePath(tpl.ID, "", hopList(4)); err != nil && !strings.Contains(err.Error(), "一端") {
		t.Fatalf("改成另一台中转设备应被端点检查拦下: %v", err)
	}
	// A shared template may be re-pointed at a whole chain: every tunnel that uses
	// it moves together, and each new hop is checked against every user's own ends
	// -- node 1 is an end of both tunnels and node 4 of one of them, so a chain
	// through 3→5 is the one that is legal for both.
	if err := s.UpdatePath(tpl.ID, "", hopList(3, 5)); err != nil {
		t.Fatalf("换成 3→5 两跳（与四条端点都不冲突）应通过: %v", err)
	}
	for _, id := range []string{t1.ID, t2.ID} {
		if got, _, err := s.PathFor(s.Tunnels[id].Path); err != nil {
			t.Fatalf("模板改多跳后 %s 解析失败: %v", id, err)
		} else if len(got.Hops) != 2 || got.Hops[0].NodeID != 3 || got.Hops[1].NodeID != 5 {
			t.Fatalf("%s 应继承模板的两跳，实际 %+v", id, got.Hops)
		}
	}
	// UpdatePath deliberately does not hand out a transit port for a hop it just
	// gained: enrollment and publish own that port, and re-granting it here would
	// undo an operator's clear. Delivery is what reports a hop with no port.
	for _, id := range []string{t1.ID, t2.ID} {
		if err := s.Delete(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeletePath(tpl.ID); err != nil {
		t.Fatalf("没有隧道引用后应能删除模板: %v", err)
	}
}

// Color is the one tunnel field that means nothing to the underlay and
// everything to the FIB (preference = 230+颜色), so the two things that must
// hold are: it survives create/update/restart, and recoloring to 0 is possible.
func TestTunnelColorIsStoredAndRecolorable(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2)

	tun, err := s.Create(CreateRequest{
		A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2}, Color: 3, Now: 1000,
	}, views)
	if err != nil {
		t.Fatalf("带颜色建隧道失败: %v", err)
	}
	if tun.Color != 3 {
		t.Fatalf("颜色应存成 3，实际 %d", tun.Color)
	}

	// A request that says nothing about color keeps it -- the page edits the path
	// and the ports through the same form.
	got, err := s.Update(tun.ID, UpdateRequest{Now: 1100}, views)
	if err != nil {
		t.Fatal(err)
	}
	if got.Color != 3 {
		t.Fatalf("没写颜色的更新不该动颜色，实际 %d", got.Color)
	}

	// 0 is a real color, so it has to be settable.
	zero := uint8(0)
	got, err = s.Update(tun.ID, UpdateRequest{Color: &zero, Now: 1200}, views)
	if err != nil {
		t.Fatalf("改回颜色 0 失败: %v", err)
	}
	if got.Color != 0 {
		t.Fatalf("颜色应改成 0，实际 %d", got.Color)
	}

	// It is configuration: it must survive the state round trip.
	blob, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back State
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if bt := back.Tunnels[tun.ID]; bt == nil || bt.Color != 0 {
		t.Fatalf("重启后颜色丢了: %+v", bt)
	}
}

// Colors above 7 have no verified table-number segment behind them
// (颜色*1000+主表号，八段落在 0..7255), and a color that overflowed the u8
// preference would quietly invert the order -- the failure
// docs/overlay-design-draft.md §2.7 measured on the FRR side (kernel metric 300
// => VPP preference 44).
func TestColorOutOfRangeIsRefused(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2)

	if _, err := s.Create(CreateRequest{
		A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2}, Color: MaxTunnelColor + 1, Now: 1000,
	}, views); err == nil || !strings.Contains(err.Error(), "0..") {
		t.Fatalf("颜色 %d 应被拒，实际 %v", MaxTunnelColor+1, err)
	}
	if len(s.Tunnels) != 0 {
		t.Fatalf("被拒的创建不该留下隧道，实际 %d 条", len(s.Tunnels))
	}

	tun := mustCreate(t, s, views, 1, 2, nil)
	bad := uint8(200)
	if _, err := s.Update(tun.ID, UpdateRequest{Color: &bad, Now: 1100}, views); err == nil {
		t.Fatal("把颜色改成 200 应被拒")
	}
	if s.Tunnels[tun.ID].Color != 0 {
		t.Fatalf("失败的改色不该写库，实际颜色 %d", s.Tunnels[tun.ID].Color)
	}
}

// A batch colors every tunnel it makes: "从 A 到这几台都走颜色 1" is one question,
// and a color that is out of range fails the batch once instead of per peer.
func TestFanAppliesOneColorToTheWholeBatch(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2, 3, 4)

	res, err := s.Fan(FanRequest{
		Local: EndpointSpec{NodeID: 1}, Peers: []uint16{2, 3, 4}, Color: 1, Now: 1000,
	}, views)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 3 {
		t.Fatalf("应建 3 条，实际 %d", len(res.Created))
	}
	for _, tun := range res.Created {
		if tun.Color != 1 {
			t.Fatalf("隧道 %s 颜色 %d，批量应统一成 1", tun.ID, tun.Color)
		}
	}

	before := len(s.Tunnels)
	if _, err := s.Fan(FanRequest{
		Local: EndpointSpec{NodeID: 1}, Peers: []uint16{5}, Color: MaxTunnelColor + 1, Now: 1000,
	}, testViews(1, 2, 3, 4, 5)); err == nil {
		t.Fatal("越界颜色的批量应整体报错，而不是每台记一条 skip")
	}
	if len(s.Tunnels) != before {
		t.Fatalf("越界颜色的批量不该建任何隧道，%d -> %d", before, len(s.Tunnels))
	}
}

// weight / dynamic 与 color 同一套存取纪律：Create 带入、Update 指针语义
// （nil = 保持）、0/false 是合法值（= 回默认 / 关）、随状态往返存盘。
func TestTunnelWeightStoredAndUpdatable(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2)

	tun, err := s.Create(CreateRequest{
		A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2},
		Color: 3, Weight: 30, Dynamic: true, Now: 1000,
	}, views)
	if err != nil {
		t.Fatalf("带权重建隧道失败: %v", err)
	}
	if tun.Weight != 30 || !tun.Dynamic {
		t.Fatalf("weight/dynamic 应存成 30/true，实际 %d/%v", tun.Weight, tun.Dynamic)
	}

	// 不带这两个字段的更新是"保持"：页面只改路径/端口时不许静默清掉权重。
	got, err := s.Update(tun.ID, UpdateRequest{Now: 1100}, views)
	if err != nil {
		t.Fatal(err)
	}
	if got.Weight != 30 || !got.Dynamic {
		t.Fatalf("没带 weight 的更新不该动权重，实际 %d/%v", got.Weight, got.Dynamic)
	}

	// 0 是合法值（= 回到默认 1，等价分担），false 是合法值（= 关动态）。
	zero := uint8(0)
	off := false
	got, err = s.Update(tun.ID, UpdateRequest{Weight: &zero, Dynamic: &off, Now: 1200}, views)
	if err != nil {
		t.Fatalf("重置权重失败: %v", err)
	}
	if got.Weight != 0 || got.Dynamic {
		t.Fatalf("weight/dynamic 应重置成 0/false，实际 %d/%v", got.Weight, got.Dynamic)
	}

	// 配置必须活过状态往返（存盘/重启）。
	blob, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back State
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if bt := back.Tunnels[tun.ID]; bt == nil || bt.Weight != 0 || bt.Dynamic {
		t.Fatalf("重启后 weight/dynamic 不对: %+v", bt)
	}
}

// FEC 比例走同一套存取纪律（Create 带入、Update 指针语义、0 是合法值），
// 但多一条硬边界：只有 0..4 五档。越界在 Create/Update 里就挡下来 —— 让
// 它漏到 agent 去，症状是"控制台显示 1:6、设备上什么都不做"。
func TestTunnelFecRatioStoredAndUpdatable(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2)

	tun, err := s.Create(CreateRequest{
		A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2},
		FecRatio: 2, Now: 1000,
	}, views)
	if err != nil {
		t.Fatalf("带 FEC 建隧道失败: %v", err)
	}
	if tun.FecRatio != 2 {
		t.Fatalf("fec_ratio 应存成 2，实际 %d", tun.FecRatio)
	}

	// 不带 fec_ratio 的更新是"保持"：页面改端口不该顺手关掉 FEC。
	got, err := s.Update(tun.ID, UpdateRequest{Now: 1100}, views)
	if err != nil {
		t.Fatal(err)
	}
	if got.FecRatio != 2 {
		t.Fatalf("没带 fec_ratio 的更新不该动它，实际 %d", got.FecRatio)
	}

	// 五档里剩下两档：4（最省）与 0（关）。
	four := uint8(4)
	if got, err = s.Update(tun.ID, UpdateRequest{FecRatio: &four, Now: 1150}, views); err != nil {
		t.Fatal(err)
	}
	if got.FecRatio != 4 {
		t.Fatalf("改成 1:4 失败，实际 %d", got.FecRatio)
	}
	zero := uint8(0)
	if got, err = s.Update(tun.ID, UpdateRequest{FecRatio: &zero, Now: 1200}, views); err != nil {
		t.Fatal(err)
	}
	if got.FecRatio != 0 {
		t.Fatalf("关掉 FEC 失败，实际 %d", got.FecRatio)
	}

	// 越界必须被挡，而且不能写库（先开回 3 才能看出"被拒后没动"）。
	three := uint8(3)
	if _, err := s.Update(tun.ID, UpdateRequest{FecRatio: &three, Now: 1300}, views); err != nil {
		t.Fatal(err)
	}
	bad := uint8(5)
	if _, err := s.Update(tun.ID, UpdateRequest{FecRatio: &bad, Now: 1400}, views); err == nil {
		t.Fatal("FEC 比例 5 应当被拒")
	}
	if cur := s.Tunnels[tun.ID]; cur.FecRatio != 3 {
		t.Fatalf("被拒的越界比例写进了库: %d", cur.FecRatio)
	}

	// 配置必须活过状态往返（存盘/重启）。
	blob, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back State
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if bt := back.Tunnels[tun.ID]; bt == nil || bt.FecRatio != 3 {
		t.Fatalf("重启后 fec_ratio 不对: %+v", bt)
	}
}

// 建隧道时的越界同样在这里挡：批量（Fan）与单建共用 create()。
func TestTunnelCreateRejectsOutOfRangeFec(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2)
	before := len(s.Tunnels)
	if _, err := s.Create(CreateRequest{
		A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2},
		FecRatio: 5, Now: 1000,
	}, views); err == nil {
		t.Fatal("FEC 比例 5 建隧道应当被拒")
	}
	if len(s.Tunnels) != before {
		t.Fatalf("被拒的创建留下了隧道：%d -> %d", before, len(s.Tunnels))
	}
}
