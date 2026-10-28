package underlay

import (
	"errors"
	"strings"
	"testing"

	"sdwan/controller/internal/wire"
)

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
	if tun.MTU != wire.TunnelMTUBudget(1500, 0) {
		t.Fatalf("直连 MTU 应是 %d，实际 %d", wire.TunnelMTUBudget(1500, 0), tun.MTU)
	}
	if tun.Path != nil {
		t.Fatal("直连隧道不该有路径")
	}
}

func TestCreateRelayTunnelSubtractsOneHop(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2, 3)
	tpl, err := s.CreatePath("", 3, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	tun := mustCreate(t, s, views, 1, 2, &TunnelPath{TemplateID: tpl.ID})
	if tun.Path == nil || tun.Path.TemplateID != tpl.ID {
		t.Fatalf("路径没记下来: %+v", tun.Path)
	}
	want := wire.TunnelMTUBudget(1500, 1)
	if tun.MTU != want {
		t.Fatalf("一跳路径的 MTU 应是 %d，实际 %d", want, tun.MTU)
	}
	if want >= wire.TunnelMTUBudget(1500, 0) {
		t.Fatalf("预算函数没体现跳数")
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
			Path: &TunnelPath{NodeID: 1}}, "同时是隧道的一端"},
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

func TestUnknownIfaceMTULeavesMTUUnset(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2)
	v := views[1]
	v.Ifaces = map[string]uint16{"eth0": 0}
	views[1] = v
	tun := mustCreate(t, s, views, 1, 2, nil)
	if tun.MTU != 0 {
		t.Fatalf("MTU 没上报时应留 0（未知），实际 %d", tun.MTU)
	}
}

func TestUpdateAndDelete(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2, 3)
	tun := mustCreate(t, s, views, 1, 2, nil)
	directMTU := tun.MTU
	tpl, _ := s.CreatePath("", 3, "", 1000)

	upd, err := s.Update(tun.ID, UpdateRequest{Path: &TunnelPath{TemplateID: tpl.ID}, Now: 2000}, views)
	if err != nil {
		t.Fatal(err)
	}
	if upd.Path == nil || upd.Path.TemplateID != tpl.ID {
		t.Fatalf("路径没改上: %+v", upd.Path)
	}
	if upd.MTU != wire.TunnelMTUBudget(1500, 1) || upd.MTU >= directMTU {
		t.Fatalf("改成一跳后 MTU 应变小：%d -> %d", directMTU, upd.MTU)
	}
	if upd.A.Port != tun.A.Port || upd.B.Port != tun.B.Port {
		t.Fatal("没要求改端口，端口却动了")
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

// One WireGuard static key per device means VPP's peer table (deduped by public
// key, across interfaces) rejects a second tunnel to the same peer with -116.
// The controller must refuse it up front, not let the device fail at apply time.
func TestCreateRejectsSecondTunnelOnSamePair(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2)
	first := mustCreate(t, s, views, 1, 2, nil)

	_, err := s.Create(CreateRequest{A: EndpointSpec{NodeID: 2}, B: EndpointSpec{NodeID: 1}, Now: 2000}, views)
	if err == nil || !strings.Contains(err.Error(), "-116") || !strings.Contains(err.Error(), first.ID) {
		t.Fatalf("同对设备第二条隧道应被拒绝并说明原因，实际 %v", err)
	}
	if len(s.Tunnels) != 1 {
		t.Fatalf("被拒的请求不该留下隧道: %d", len(s.Tunnels))
	}
	// Deleting the first frees the pair again.
	if err := s.Delete(first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(CreateRequest{A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2}, Now: 3000}, views); err != nil {
		t.Fatalf("删掉后应该能重建: %v", err)
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
	views := testViews(1, 2, 3, 4)
	tpl, _ := s.CreatePath("主用路径", 3, "", 1000)
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
	if err := s.UpdatePath(tpl.ID, "", 1, ""); err == nil {
		t.Fatal("把中转设备改成隧道端点，竟然通过了")
	}
	if err := s.UpdatePath(tpl.ID, "", 4, ""); err != nil && !strings.Contains(err.Error(), "一端") {
		t.Fatalf("改成另一台中转设备应被端点检查拦下: %v", err)
	}
	for _, id := range []string{t1.ID, t2.ID} {
		if err := s.Delete(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeletePath(tpl.ID); err != nil {
		t.Fatalf("没有隧道引用后应能删除模板: %v", err)
	}
}
