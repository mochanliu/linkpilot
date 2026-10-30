package underlay

import (
	"encoding/json"
	"strings"
	"testing"

	"sdwan/controller/internal/wire"
)

// MaxRelayHops is a policy number, but it is bounded by a dataplane one: the VPP
// path message holds wire.MaxSRSegments segments and segments[0] is the tunnel
// destination, so a chain can carry at most MaxSRSegments-1 relays. If someone
// raises MaxRelayHops past that, the controller would accept a path the agent
// cannot install -- and the failure only shows up on the box.
func TestRelayDepthFitsSegments(t *testing.T) {
	if MaxRelayHops+1 > wire.MaxSRSegments {
		t.Fatalf("中转上限 %d 跳需要 %d 个段，数据面一条路径只有 %d 个段",
			MaxRelayHops, MaxRelayHops+1, wire.MaxSRSegments)
	}
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2, 10, 11, 12, 13, 14)
	deepest := hopList(10, 11, 12)
	if _, err := s.CreatePath("最深", deepest, 1000); err != nil {
		t.Fatalf("%d 跳（上限值）应通过: %v", len(deepest), err)
	}
	if _, err := s.Create(
		CreateRequest{A: EndpointSpec{NodeID: 1}, B: EndpointSpec{NodeID: 2},
			Path: via(10, 11, 12, 13), Now: 1000}, views); err == nil {
		t.Fatal("比上限多一跳竟然通过了")
	} else if !strings.Contains(err.Error(), "3 跳") {
		t.Fatalf("拒绝里应说明上限和原因，实际: %v", err)
	}
}

// A relay chain is ordered and the order IS the path: 3→5 pops a segment at 3
// first and 5 second, 5→3 is a different route. Nothing in the allocator, the
// view or the JSON may re-sort it -- a sorted copy would silently change which
// box the packet reaches first, and both chains look identical in the console.
func TestHopsKeepSubmissionOrder(t *testing.T) {
	s := newTestState(t, "10.9.0.0/24")
	views := testViews(1, 2, 3, 5, 7)
	tun := mustCreate(t, s, views, 1, 2, via(7, 3, 5))
	rp, has, err := s.PathFor(tun.Path)
	if err != nil || !has {
		t.Fatalf("解析失败: has=%v err=%v", has, err)
	}
	if len(rp.Hops) != 3 || rp.Hops[0].NodeID != 7 || rp.Hops[1].NodeID != 3 || rp.Hops[2].NodeID != 5 {
		t.Fatalf("跳序被改了: %+v", rp.Hops)
	}

	blob, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back State
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("含多跳路径的状态存不回去: %v", err)
	}
	got := back.Tunnels[tun.ID]
	if got == nil || len(got.Path.Hops) != 3 || got.Path.Hops[0].NodeID != 7 ||
		got.Path.Hops[1].NodeID != 3 || got.Path.Hops[2].NodeID != 5 {
		t.Fatalf("存盘后跳序变了: %+v", got)
	}
}

// The first release of this layer stored exactly one relay, as node_id/addr on
// the path itself. Those files are on real disks, so they must load as a
// one-hop chain rather than as a path with no hops -- "no hops" would render as
// a direct tunnel and, worse, deliver as one.
func TestLegacySingleHopJSONMigrates(t *testing.T) {
	var p TunnelPath
	if err := json.Unmarshal([]byte(`{"node_id":3,"addr":"192.0.2.9"}`), &p); err != nil {
		t.Fatalf("旧路径读不了: %v", err)
	}
	if len(p.Hops) != 1 || p.Hops[0].NodeID != 3 || p.Hops[0].Addr != "192.0.2.9" {
		t.Fatalf("旧路径没有落成单跳链: %+v", p.Hops)
	}

	var tpl PathTemplate
	if err := json.Unmarshal([]byte(`{"id":"p1","name":"老模板","node_id":5}`), &tpl); err != nil {
		t.Fatalf("旧模板读不了: %v", err)
	}
	if len(tpl.Hops) != 1 || tpl.Hops[0].NodeID != 5 || tpl.Hops[0].Addr != "" {
		t.Fatalf("旧模板没有落成单跳链: %+v", tpl.Hops)
	}

	// A whole state file, the way the store actually hands it over.
	var st State
	blob := `{"settings":{},"tunnels":{"t1":{"id":"t1","a":{"node_id":1},"b":{"node_id":2},` +
		`"path":{"node_id":3}}},"paths":{"p1":{"id":"p1","name":"x","node_id":4}}}`
	if err := json.Unmarshal([]byte(blob), &st); err != nil {
		t.Fatalf("旧状态文件读不了: %v", err)
	}
	if h := st.Tunnels["t1"].Path.Hops; len(h) != 1 || h[0].NodeID != 3 {
		t.Fatalf("旧状态里的隧道路径: %+v", h)
	}
	if h := st.Paths["p1"].Hops; len(h) != 1 || h[0].NodeID != 4 {
		t.Fatalf("旧状态里的模板路径: %+v", h)
	}

	// Both shapes at once is not something to guess a precedence for: whichever
	// wins, the other half of the file is a lie about the path.
	if err := json.Unmarshal([]byte(`{"node_id":3,"hops":[{"node_id":9}]}`), &p); err == nil {
		t.Fatal("同时写 hops 和 node_id 竟然解析成功了")
	}
	if err := json.Unmarshal([]byte(`{"id":"p1","node_id":3,"hops":[{"node_id":9}]}`), &tpl); err == nil {
		t.Fatal("模板同时写 hops 和 node_id 竟然解析成功了")
	}
}

// Writing the new shape out and reading it back must not go through the legacy
// branch: if the encoder dropped hops, every tunnel would wake up as a direct one.
func TestPathJSONRoundTripKeepsHops(t *testing.T) {
	blob, err := json.Marshal(via(3, 5))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"hops"`) {
		t.Fatalf("新路径没带 hops 字段: %s", blob)
	}
	var p TunnelPath
	if err := json.Unmarshal(blob, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Hops) != 2 || p.Hops[1].NodeID != 5 {
		t.Fatalf("回读不对: %+v", p.Hops)
	}
}
