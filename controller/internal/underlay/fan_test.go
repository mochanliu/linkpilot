package underlay

import (
	"strings"
	"testing"
)

// The batch the console form actually describes: CPE A, one path, N peers. Each
// tunnel must be built exactly the way a manual create would build it -- same
// path, same per-device port and overlay allocation -- because the page has no
// per-peer fields to fill in.
func TestFanBuildsOneTunnelPerPeerOnTheSamePath(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2, 3, 4, 5, 7)
	res, err := s.Fan(FanRequest{
		Local: EndpointSpec{NodeID: 1, Iface: "eth0"},
		Peers: []uint16{4, 2, 3, 4, 0},
		Path:  via(5, 7),
		Now:   1000,
	}, views)
	if err != nil {
		t.Fatalf("批量建隧道失败: %v", err)
	}
	// Duplicated and zero peers collapse; the order they come back in is the
	// device-number order the page lists, not the click order.
	if len(res.Created) != 3 || len(res.Skipped) != 0 {
		t.Fatalf("built %d, skipped %d: %+v", len(res.Created), len(res.Skipped), res.Skipped)
	}
	for i, want := range []uint16{2, 3, 4} {
		if got := res.Created[i].B.NodeID; got != want {
			t.Fatalf("第 %d 条隧道对端是 %d，应是 %d", i+1, got, want)
		}
	}
	if res.BatchID == "" {
		t.Fatal("批量结果必须带批次号，否则页面无法说明这批是哪一次")
	}
	seenPort := map[uint16]bool{}
	seenOverlay := map[string]bool{}
	for _, tun := range res.Created {
		if tun.BatchID != res.BatchID {
			t.Fatalf("隧道 %s 没带上批次号", tun.ID)
		}
		if seenPort[tun.A.Port] {
			t.Fatalf("设备 1 的三条隧道共用了监听端口 %d", tun.A.Port)
		}
		seenPort[tun.A.Port] = true
		if seenOverlay[tun.A.Overlay] {
			t.Fatalf("设备 1 的三条隧道共用了 overlay %s", tun.A.Overlay)
		}
		seenOverlay[tun.A.Overlay] = true
		if s.Tunnels[tun.ID] == nil {
			t.Fatalf("隧道 %s 没落进状态里", tun.ID)
		}
		// Every peer goes through the SAME chain, in the same order: that is the
		// whole point of the form.
		rp, has, err := s.PathFor(tun.Path)
		if err != nil || !has || len(rp.Hops) != 2 ||
			rp.Hops[0].NodeID != 5 || rp.Hops[1].NodeID != 7 {
			t.Fatalf("隧道 %s 的路径不对: has=%v err=%v %+v", tun.ID, has, err, rp.Hops)
		}
	}
	// Both relays got a transit port once, and all three tunnels share it.
	for _, n := range []uint16{5, 7} {
		if s.TransitPorts[n] == 0 {
			t.Fatalf("中转设备 %d 没分到端口", n)
		}
	}
}

// A batch that references a template is how "同一条路径" stays one object: the
// page edits the template once and all N tunnels move.
func TestFanThroughOneTemplateMovesTogether(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2, 3, 5, 7)
	tpl, err := s.CreatePath("主用", hopList(5, 7), 1000)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Fan(FanRequest{
		Local: EndpointSpec{NodeID: 1, Iface: "eth0"},
		Peers: []uint16{2, 3},
		Path:  &TunnelPath{TemplateID: tpl.ID},
		Now:   1000,
	}, views)
	if err != nil || len(res.Created) != 2 {
		t.Fatalf("built %+v err %v", res.Created, err)
	}
	if users := s.PathUsers(tpl.ID); len(users) != 2 {
		t.Fatalf("模板应被这批 2 条隧道使用，实际 %v", users)
	}
	if err := s.UpdatePath(tpl.ID, "", hopList(7, 5)); err != nil {
		t.Fatal(err)
	}
	for _, tun := range res.Created {
		rp, _, err := s.PathFor(tun.Path)
		if err != nil || rp.Hops[0].NodeID != 7 || rp.Hops[1].NodeID != 5 {
			t.Fatalf("改模板后 %s 没跟着走: %+v err %v", tun.ID, rp.Hops, err)
		}
	}
}

// A peer that already has a tunnel is a per-peer result, not a failed batch: the
// operator selected five devices and five was optimistic, not wrong. Rolling back
// the three that worked would force the same five clicks again.
func TestFanSkipsTakenPairsAndKeepsTheRest(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2, 3, 4, 5)
	existing := mustCreate(t, s, views, 1, 3, nil)
	res, err := s.Fan(FanRequest{
		Local: EndpointSpec{NodeID: 1, Iface: "eth0"},
		Peers: []uint16{2, 3, 4},
		Now:   1000,
	}, views)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 2 || len(res.Skipped) != 1 {
		t.Fatalf("built %d skipped %d", len(res.Created), len(res.Skipped))
	}
	if res.Skipped[0].NodeID != 3 || !strings.Contains(res.Skipped[0].Reason, "只能有一条隧道") {
		t.Fatalf("跳过的原因没说是哪台、为什么: %+v", res.Skipped[0])
	}
	// The pre-existing tunnel is the one that was there, not the one the batch
	// would have made.
	if s.Tunnels[existing.ID] == nil {
		t.Fatal("批量把已有隧道换掉了")
	}
	if len(s.Tunnels) != 3 {
		t.Fatalf("状态里应有 3 条隧道，实际 %d", len(s.Tunnels))
	}
}

// A peer that is also a relay hop is impossible for that one tunnel only. It must
// not poison the peers that do not appear in the chain.
func TestFanSkipsPeerThatIsAlsoAHop(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2, 3, 5)
	res, err := s.Fan(FanRequest{
		Local: EndpointSpec{NodeID: 1, Iface: "eth0"},
		Peers: []uint16{2, 3, 5},
		Path:  via(5),
		Now:   1000,
	}, views)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 2 || len(res.Skipped) != 1 || res.Skipped[0].NodeID != 5 {
		t.Fatalf("built %d skipped %+v", len(res.Created), res.Skipped)
	}
	if !strings.Contains(res.Skipped[0].Reason, "同时是隧道的一端") {
		t.Fatalf("跳过原因不对: %q", res.Skipped[0].Reason)
	}
	// The self pair is the other per-peer impossibility: selecting A as its own
	// peer is a checkbox slip, not a reason to lose the other four tunnels.
	res, err = s.Fan(FanRequest{
		Local: EndpointSpec{NodeID: 1, Iface: "eth0"},
		Peers: []uint16{1, 2},
		Now:   2000,
	}, views)
	if err != nil {
		t.Fatal(err)
	}
	// 1↔2 already exists from the previous fan, so only the self skip is new here.
	if len(res.Created) != 0 || len(res.Skipped) != 2 {
		t.Fatalf("built %d skipped %+v", len(res.Created), res.Skipped)
	}
	if res.Skipped[0].NodeID != 1 || !strings.Contains(res.Skipped[0].Reason, "同一台设备") {
		t.Fatalf("自连原因不对: %+v", res.Skipped[0])
	}
}

// Everything that fails for the WHOLE batch must fail once, before any tunnel is
// written: a missing local device or a path that does not resolve would otherwise
// produce N identical skips and a state that half-changed.
func TestFanHardErrorsLeaveNothingBehind(t *testing.T) {
	views := testViews(1, 2, 3, 5)
	cases := []struct {
		name  string
		req   FanRequest
		wantS string
	}{
		{"本端没入网", FanRequest{Local: EndpointSpec{NodeID: 9, Iface: "eth0"}, Peers: []uint16{2, 3}}, "本端设备 9 不存在或未入网"},
		{"没有对端", FanRequest{Local: EndpointSpec{NodeID: 1, Iface: "eth0"}, Peers: []uint16{0}}, "对端至少要选一台设备"},
		{"路径模板不存在", FanRequest{Local: EndpointSpec{NodeID: 1, Iface: "eth0"},
			Peers: []uint16{2, 3}, Path: &TunnelPath{TemplateID: "p99"}}, "路径模板 p99 不存在"},
		{"路径超深", FanRequest{Local: EndpointSpec{NodeID: 1, Iface: "eth0"},
			Peers: []uint16{2, 3}, Path: via(5, 5, 5, 5)}, "中转最多 3 跳"},
		{"路径绕回同一台", FanRequest{Local: EndpointSpec{NodeID: 1, Iface: "eth0"},
			Peers: []uint16{2, 3}, Path: via(5, 5)}, "设备 5 在路径里出现了两次"},
	}
	for _, tc := range cases {
		s := newTestState(t, "10.9.0.0/24")
		gen := s.Gen
		tc.req.Now = 1000
		res, err := s.Fan(tc.req, views)
		if err == nil {
			t.Fatalf("%s: 本该整批拒绝，实际 res=%+v", tc.name, res)
		}
		if !strings.Contains(err.Error(), tc.wantS) {
			t.Fatalf("%s: 错误里没有 %q，实际 %q", tc.name, tc.wantS, err)
		}
		if len(s.Tunnels) != 0 {
			t.Fatalf("%s: 拒绝之前已经写了一条隧道", tc.name)
		}
		if s.Gen != gen {
			t.Fatalf("%s: 拒绝的一次调用把投递版本号也推进了", tc.name)
		}
	}
}

// Before the global config is published there is no overlay pool and no transit
// port range, so a batch cannot be allocated at all. The page needs the reason,
// not an empty success.
func TestFanRefusesBeforePublish(t *testing.T) {
	s := New(DefaultSettings().Normalize())
	res, err := s.Fan(FanRequest{
		Local: EndpointSpec{NodeID: 1, Iface: "eth0"},
		Peers: []uint16{2},
		Now:   1000,
	}, testViews(1, 2))
	if err == nil {
		t.Fatalf("未发布全局配置时批量竟然成功: %+v", res)
	}
	if !strings.Contains(err.Error(), "还没有发布") {
		t.Fatalf("错误里没说要先发布什么: %q", err)
	}
	if len(s.Tunnels) != 0 {
		t.Fatal("未发布时批量却写了隧道")
	}
}
