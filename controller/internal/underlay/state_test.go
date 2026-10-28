package underlay

import (
	"errors"
	"fmt"
	"testing"
)

func testViews(ids ...uint16) map[uint16]NodeView {
	views := map[uint16]NodeView{}
	for _, id := range ids {
		views[id] = NodeView{
			ID:           id,
			Name:         fmt.Sprintf("dev%d", id),
			Observed:     fmt.Sprintf("203.0.113.%d", id),
			DefaultIface: "eth0",
			Ifaces:       map[string]uint16{"eth0": 1500},
		}
	}
	return views
}

func newTestState(t *testing.T, cidr string) *State {
	t.Helper()
	s := DefaultSettings()
	s.OverlayCIDR = cidr
	if err := s.Validate(); err != nil {
		t.Fatalf("测试设置不合法: %v", err)
	}
	st := New(s)
	if err := st.PublishSettings(st.Settings, nil, 1); err != nil {
		t.Fatalf("发布测试全局配置失败: %v", err)
	}
	return st
}

func mustCreate(t *testing.T, s *State, views map[uint16]NodeView, a, b uint16, path *TunnelPath) Tunnel {
	t.Helper()
	tun, err := s.Create(CreateRequest{
		A:    EndpointSpec{NodeID: a},
		B:    EndpointSpec{NodeID: b},
		Path: path,
		Now:  1000,
	}, views)
	if err != nil {
		t.Fatalf("建隧道 %d-%d 失败: %v", a, b, err)
	}
	return tun
}

// The released address must come back only after every never-used one: peers
// still route to a freed /32, and reusing it immediately would hand a stale
// route to a brand-new tunnel.
func TestOverlayFIFOReleasesToTheBack(t *testing.T) {
	s := newTestState(t, "10.9.0.0/29") // .1 .. .6
	views := testViews(1, 2, 3, 4)

	t1 := mustCreate(t, s, views, 1, 2, nil)
	if t1.A.Overlay != "10.9.0.1" || t1.B.Overlay != "10.9.0.2" {
		t.Fatalf("第一条隧道应取 .1/.2，实际 %s/%s", t1.A.Overlay, t1.B.Overlay)
	}
	if err := s.Delete(t1.ID); err != nil {
		t.Fatal(err)
	}
	if len(s.OverlayFreed) != 2 || s.OverlayFreed[0] != "10.9.0.1" {
		t.Fatalf("释放后队尾应是 .1,.2，实际 %v", s.OverlayFreed)
	}

	t2 := mustCreate(t, s, views, 1, 3, nil)
	if t2.A.Overlay != "10.9.0.3" || t2.B.Overlay != "10.9.0.4" {
		t.Fatalf("释放的地址被立刻复用了：新隧道取了 %s/%s", t2.A.Overlay, t2.B.Overlay)
	}
	t3 := mustCreate(t, s, views, 1, 4, nil)
	if t3.A.Overlay != "10.9.0.5" || t3.B.Overlay != "10.9.0.6" {
		t.Fatalf("第三条隧道应取 .5/.6，实际 %s/%s", t3.A.Overlay, t3.B.Overlay)
	}
	t4 := mustCreate(t, s, views, 1, 2, nil)
	if t4.A.Overlay != "10.9.0.1" || t4.B.Overlay != "10.9.0.2" {
		t.Fatalf("未用地址用完后应回到队首的 .1/.2，实际 %s/%s", t4.A.Overlay, t4.B.Overlay)
	}
}

func TestOverlayExhaustionIsAnError(t *testing.T) {
	s := newTestState(t, "10.9.0.0/30") // .1, .2
	views := testViews(1, 2, 3)
	mustCreate(t, s, views, 1, 2, nil)
	// A different pair, so what runs out is the address pool -- not the
	// one-tunnel-per-pair rule.
	if _, err := s.Create(CreateRequest{A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 3}, Now: 1000}, views); !errors.Is(err, ErrOverlayExhausted) {
		t.Fatalf("池子用尽应返回 ErrOverlayExhausted，实际 %v", err)
	}
}

// One static WG key per device: a second tunnel on the same pair cannot be
// installed (VPP dedupes peers by pubkey across interfaces, -116), so it is
// refused at creation rather than at apply time.
func TestSecondTunnelOnTheSamePairIsRefused(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2)
	first := mustCreate(t, s, views, 1, 2, nil)
	if _, err := s.Create(CreateRequest{A: EndpointSpec{NodeID: 2}, B: EndpointSpec{NodeID: 1}, Now: 1000}, views); !errors.Is(err, ErrPairTaken) {
		t.Fatalf("同一对设备的第二条隧道应被拒（ErrPairTaken），实际 %v", err)
	}
	if len(s.Tunnels) != 1 {
		t.Fatalf("被拒的创建不该留下任何东西，实际 %d 条", len(s.Tunnels))
	}
	// The pair becomes usable again once the tunnel is gone.
	if err := s.Delete(first.ID); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s, views, 1, 2, nil)
}

// Ports are unique per DEVICE: both ends may hold 20000, and a second tunnel on
// the same device must move to the next number.
func TestPortsAreUniquePerDevice(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2, 3)
	t1 := mustCreate(t, s, views, 1, 2, nil)
	if t1.A.Port != 20000 || t1.B.Port != 20000 {
		t.Fatalf("第一条隧道两端都该拿到 20000，实际 %d/%d", t1.A.Port, t1.B.Port)
	}
	t2 := mustCreate(t, s, views, 1, 3, nil)
	if t2.A.Port != 20001 {
		t.Fatalf("同一台设备的第二条隧道应拿 20001，实际 %d", t2.A.Port)
	}
	if t2.B.Port != 20000 {
		t.Fatalf("另一台设备仍应从 20000 开始，实际 %d", t2.B.Port)
	}
	// An explicit port that this device already uses is refused.
	if _, err := s.Create(CreateRequest{A: EndpointSpec{NodeID: 1, Port: 20000}, B: EndpointSpec{NodeID: 2}, Now: 1000}, views); err == nil {
		t.Fatal("同一台设备重复使用端口竟然通过了")
	}
}

func TestAvoidedPortsAreSkipped(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	if err := s.SetSettings(func() Settings {
		cfg := s.Settings
		cfg.VPNAvoid = []PortRange{{Min: 20000, Max: 20009}}
		return cfg
	}()); err != nil {
		t.Fatal(err)
	}
	t1 := mustCreate(t, s, testViews(1, 2), 1, 2, nil)
	if t1.A.Port != 20010 {
		t.Fatalf("排除段应被跳过，期望 20010，实际 %d", t1.A.Port)
	}
}

// One transit port per device, however many tunnels are relayed through it.
func TestOneTransitPortPerDevice(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2, 3, 4)
	tpl, err := s.CreatePath("", 1, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s, views, 2, 3, &TunnelPath{TemplateID: tpl.ID})
	mustCreate(t, s, views, 2, 4, &TunnelPath{TemplateID: tpl.ID})
	if got := s.TransitPorts[1]; got != 15000 {
		t.Fatalf("中转端口应是池内第一个 15000，实际 %d", got)
	}
	if len(s.TransitPorts) != 1 {
		t.Fatalf("只有设备 1 做中转，实际分配了 %v", s.TransitPorts)
	}
	// A tunnel that does not relay must not assign one. (Pair 1-2 is free: the
	// relays above are 2-3 and 2-4.)
	mustCreate(t, s, views, 1, 2, nil)
	if len(s.TransitPorts) != 1 {
		t.Fatalf("直连隧道不该分配中转端口，实际 %v", s.TransitPorts)
	}
}

func TestSetSettingsRefusesToStrandAllocations(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2)
	mustCreate(t, s, views, 1, 2, nil)

	next := s.Settings
	next.VPNPorts = PortRange{Min: 21000, Max: 21999}
	if err := s.SetSettings(next); err == nil {
		t.Fatal("缩小端口池把在用端口丢在外面，竟然通过了")
	}

	next = s.Settings
	next.OverlayCIDR = "10.8.0.0/24"
	if err := s.SetSettings(next); err == nil {
		t.Fatal("换 overlay 池把在用地址丢在外面，竟然通过了")
	}

	next = s.Settings
	next.ProbeIntervalMS = 500
	if err := s.SetSettings(next); err != nil {
		t.Fatalf("只改 timer 不该失败: %v", err)
	}
	if s.Settings.ProbeIntervalMS != 500 {
		t.Fatalf("timer 没生效: %d", s.Settings.ProbeIntervalMS)
	}
}

func TestSetSettingsDropsFreedAddressesOutsideNewPool(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	t1 := mustCreate(t, s, testViews(1, 2), 1, 2, nil)
	if err := s.Delete(t1.ID); err != nil {
		t.Fatal(err)
	}
	next := s.Settings
	next.OverlayCIDR = "10.8.0.0/24"
	if err := s.SetSettings(next); err != nil {
		t.Fatal(err)
	}
	if len(s.OverlayFreed) != 0 {
		t.Fatalf("旧池的释放记录应被清掉，实际 %v", s.OverlayFreed)
	}
}

// Publishing is the moment devices are told to listen: every enrolled device
// gets its one transit port then, so a tunnel created afterwards has a relay
// port that actually exists.
func TestPublishSettingsAssignsTransitPorts(t *testing.T) {
	s := New(DefaultSettings())
	if s.Configured() {
		t.Fatal("刚创建的状态不该是已发布")
	}
	if err := s.PublishSettings(s.Settings, []uint16{1, 2, 3, 2, 0}, 1000); err != nil {
		t.Fatal(err)
	}
	if !s.Configured() || s.Settings.UpdatedAt != 1000 {
		t.Fatalf("发布后应记录时间戳，实际 %d", s.Settings.UpdatedAt)
	}
	if len(s.TransitPorts) != 3 {
		t.Fatalf("每台设备都该有一个中转端口，实际 %v", s.TransitPorts)
	}
	for _, id := range []uint16{1, 2, 3} {
		if s.TransitPorts[id] != 15000 {
			t.Fatalf("设备 %d 的中转端口应是池内第一个 15000，实际 %d", id, s.TransitPorts[id])
		}
	}
	// Republishing keeps the port a device already has: moving it would make
	// every peer's SR segment stale for no reason.
	if err := s.PublishSettings(s.Settings, []uint16{1, 2, 3, 4}, 2000); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint16{1, 2, 3} {
		if s.TransitPorts[id] != 15000 {
			t.Fatalf("重新发布动了设备 %d 的中转端口", id)
		}
	}
	if s.TransitPorts[4] != 15000 {
		t.Fatalf("新设备 4 应补分配中转端口，实际 %v", s.TransitPorts)
	}
}

// A device that enrolls after publishing must still receive the configuration.
// Its transit port is the only per-device artifact the published settings carry,
// and a device without one can never relay anything.
func TestEnsureDeviceAssignsForLateEnrollment(t *testing.T) {
	s := New(DefaultSettings())
	if _, assigned, err := s.EnsureDevice(2); err != nil || assigned {
		t.Fatalf("未发布时补分配应什么都不做: assigned=%v err=%v", assigned, err)
	}
	if s.Configured() || len(s.TransitPorts) != 0 {
		t.Fatal("未发布时补分配改动了状态")
	}

	if err := s.PublishSettings(s.Settings, []uint16{1}, 1000); err != nil {
		t.Fatal(err)
	}
	gen := s.Gen
	port, assigned, err := s.EnsureDevice(2)
	if err != nil || !assigned || port != 15000 {
		t.Fatalf("晚入网设备应拿到池内第一个端口 15000，实际 port=%d assigned=%v err=%v", port, assigned, err)
	}
	if s.Gen != gen+1 {
		t.Fatalf("补分配必须推进下发版本，让设备重新拉取: %d -> %d", gen, s.Gen)
	}

	port, assigned, err = s.EnsureDevice(2)
	if err != nil || assigned || port != 15000 {
		t.Fatalf("重复补分配应幂等: port=%d assigned=%v err=%v", port, assigned, err)
	}
	if s.Gen != gen+1 {
		t.Fatalf("幂等调用推进了版本: %d", s.Gen)
	}
	if _, _, err := s.EnsureDevice(0); err == nil {
		t.Fatal("设备号 0 竟然补分配成功")
	}
}

// Transit ports are per device, so a late device takes the same number the
// early ones hold; nothing gets renumbered on the way.
func TestEnsureDeviceNeverRenumbersExistingDevices(t *testing.T) {
	s := New(DefaultSettings())
	if err := s.PublishSettings(s.Settings, []uint16{1, 2}, 1000); err != nil {
		t.Fatal(err)
	}
	gen := s.Gen
	if _, assigned, err := s.EnsureDevice(3); err != nil || !assigned {
		t.Fatalf("设备 3 应被补分配: assigned=%v err=%v", assigned, err)
	}
	for _, id := range []uint16{1, 2, 3} {
		if s.TransitPorts[id] != 15000 {
			t.Fatalf("设备 %d 的中转端口应是 15000，实际 %d", id, s.TransitPorts[id])
		}
	}
	if s.Gen != gen+1 {
		t.Fatalf("一次补分配只该推进一个版本，实际 %d -> %d", gen, s.Gen)
	}
}

// A device whose own tunnels already fill the transit pool cannot be given a
// port. The failure must leave the state exactly as it was so the next heartbeat
// can retry once the operator widens the pool.
func TestEnsureDeviceFailureLeavesStateUntouched(t *testing.T) {
	s := New(DefaultSettings())
	if err := s.PublishSettings(s.Settings, []uint16{1}, 1000); err != nil {
		t.Fatal(err)
	}
	s.Settings.TransitPorts = PortRange{Min: 15000, Max: 15000}
	s.Tunnels["t"] = &Tunnel{
		ID: "t",
		A:  Endpoint{NodeID: 2, Port: 15000},
		B:  Endpoint{NodeID: 3, Port: 20001},
	}
	gen := s.Gen
	if _, assigned, err := s.EnsureDevice(2); err == nil || assigned {
		t.Fatalf("池子被自己的隧道占满时不该分配成功: assigned=%v err=%v", assigned, err)
	}
	if _, ok := s.TransitPorts[2]; ok || s.Gen != gen {
		t.Fatalf("失败的补分配改动了状态: ports=%v gen=%d", s.TransitPorts, s.Gen)
	}
}

func TestPublishSettingsRejectsZeroTimestampAndBadConfig(t *testing.T) {
	s := New(DefaultSettings())
	if err := s.PublishSettings(s.Settings, []uint16{1}, 0); err == nil {
		t.Fatal("没有时间戳的发布竟然通过了")
	}
	bad := s.Settings
	bad.VPNPorts = PortRange{Min: 100, Max: 200}
	if err := s.PublishSettings(bad, []uint16{1}, 1000); err == nil {
		t.Fatal("非法端口池竟然发布成功了")
	}
	if s.Configured() || len(s.TransitPorts) != 0 {
		t.Fatal("失败的发布改动了状态")
	}
}

// No tunnel may exist before the global config is published: the transit port
// is only allocated then, so an earlier push would point at a port nobody binds.
func TestCreateRequiresPublishedGlobalConfig(t *testing.T) {
	s := New(DefaultSettings())
	views := testViews(1, 2)
	if _, err := s.Create(CreateRequest{A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2}, Now: 1000}, views); err == nil {
		t.Fatal("没有全局配置就建出了隧道")
	}
	if err := s.PublishSettings(s.Settings, []uint16{1, 2}, 1000); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s, views, 1, 2, nil)
}

func TestCloneIsDeep(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	tpl, _ := s.CreatePath("p", 3, "", 1000)
	t1 := mustCreate(t, s, testViews(1, 2, 3), 1, 2, &TunnelPath{TemplateID: tpl.ID})

	c := s.Clone()
	c.Tunnels[t1.ID].A.Port = 1
	c.Paths[tpl.ID].NodeID = 9
	c.OverlayFreed = append(c.OverlayFreed, "x")
	c.TransitPorts[42] = 1

	if s.Tunnels[t1.ID].A.Port == 1 || s.Paths[tpl.ID].NodeID == 9 || len(s.OverlayFreed) != 0 {
		t.Fatal("Clone 共享了底层数据，失败请求会污染真实状态")
	}
	if _, ok := s.TransitPorts[42]; ok {
		t.Fatal("Clone 共享了 transit 端口表")
	}
}
