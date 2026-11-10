package sla

import (
	"context"
	"net/netip"
	"testing"

	"sdwan/controller/internal/vppapi"
)

// fakePlane 记录写下去的路由，喂回预置的探针读数。
type fakePlane struct {
	probes []vppapi.ProbeStats
	routes map[uint32]map[netip.Prefix]bool
	// deag[table] = 最近一次写进这张选路表的默认路由指向的表号（即选中的颜色表）。
	deag     map[uint32]uint32
	probeErr error
	written  int
}

// newSelectorWith 把 fakePlane 接进选择器（dial 形状与生产一致）。
func newSelectorWith(f *fakePlane) *Selector {
	return NewSelector(func(context.Context) (Plane, error) { return f, nil }, nil)
}

func (f *fakePlane) Probes(context.Context) ([]vppapi.ProbeStats, error) {
	if f.probeErr != nil {
		return nil, f.probeErr
	}
	return f.probes, nil
}

func (f *fakePlane) IPRoutes(_ context.Context, table uint32) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(f.routes[table]))
	for p := range f.routes[table] {
		out = append(out, p)
	}
	return out, nil
}

func (f *fakePlane) IPRouteAddDel(_ context.Context, table uint32, prefix netip.Prefix, paths []vppapi.FIBPath, add bool) error {
	f.written++
	if f.routes == nil {
		f.routes = map[uint32]map[netip.Prefix]bool{}
	}
	if f.routes[table] == nil {
		f.routes[table] = map[netip.Prefix]bool{}
	}
	if add {
		f.routes[table][prefix] = true
		if len(paths) == 1 && paths[0].SWIfIndex == ^uint32(0) {
			if f.deag == nil {
				f.deag = map[uint32]uint32{}
			}
			f.deag[table] = paths[0].TableID
		}
		return nil
	}
	delete(f.routes[table], prefix)
	return nil
}

func probe(sw uint32, tx uint64, loss uint32, rttUs, jitterUs uint32) vppapi.ProbeStats {
	return vppapi.ProbeStats{SwIfIndex: sw, Tx: tx, LossPermille: loss, LastRTTus: rttUs, JitterUs: jitterUs}
}

func tunnel(sw uint32, peer uint16, color uint8) Tunnel {
	return Tunnel{SWIfIndex: sw, Peer: peer, Color: color}
}

func row(id int, table uint32) Row {
	return Row{ID: id, VPN: 0, Peer: 3, SelectorTable: table, MaxLatencyMS: 50, MaxJitterMS: 10, MaxLossPermille: 100}
}

// 达标里挑延迟最低。
func TestChoosePicksLowestLatencyAmongEligible(t *testing.T) {
	f := &fakePlane{}
	s := newSelectorWith(f)
	s.Reconcile([]Row{row(1, 10001)}, []Tunnel{
		tunnel(10, 3, 2),
		tunnel(11, 3, 3),
	})
	f.probes = []vppapi.ProbeStats{
		probe(10, 100, 10, 20_000, 1_000), // 颜色 2：20ms
		probe(11, 100, 0, 8_000, 2_000),   // 颜色 3：8ms
	}
	p := f
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	st := s.Status()
	if len(st) != 1 || st[0].Color != 3 {
		t.Fatalf("应选颜色 3（8ms 更快），实际 %+v", st)
	}
	// 落表形状：选路表 10001 的默认路由指向颜色 3 的表（VPN 0 → 3*1000）。
	if got := p.deag[10001]; got != 3000 {
		t.Fatalf("选路表 10001 应指向颜色表 3000，实际 %v", got)
	}
}

// 全部不达标：走最接近的一条（丢包最少优先），不断流。
func TestChooseWalksToBestWhenNothingEligible(t *testing.T) {
	f := &fakePlane{}
	s := newSelectorWith(f)
	s.Reconcile([]Row{row(1, 10001)}, []Tunnel{
		tunnel(10, 3, 2),
		tunnel(11, 3, 3),
	})
	f.probes = []vppapi.ProbeStats{
		probe(10, 100, 800, 30_000, 1_000), // 颜色 2：80% 丢包
		probe(11, 100, 300, 90_000, 5_000), // 颜色 3：30% 丢包、延迟超标
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if st := s.Status(); st[0].Color != 3 {
		t.Fatalf("全部不达标应走丢包最少的颜色 3，实际 %+v", st)
	}
}

// 迟滞：双方都达标时，更优者要连续 holdRounds 拍才接管。
func TestHysteresisHoldsHealthySwitch(t *testing.T) {
	f := &fakePlane{}
	s := newSelectorWith(f)
	s.Reconcile([]Row{row(1, 10001)}, []Tunnel{
		tunnel(10, 3, 2),
		tunnel(11, 3, 3),
	})
	// 先让颜色 2 当选（8ms）。
	f.probes = []vppapi.ProbeStats{
		probe(10, 100, 0, 8_000, 1_000),
		probe(11, 100, 0, 30_000, 1_000),
	}
	p := f
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if st := s.Status(); st[0].Color != 2 {
		t.Fatalf("初选应为颜色 2，实际 %+v", st)
	}
	// 颜色 3 变得更快：前 holdRounds-1 拍都保持，第 holdRounds 拍切换。
	p.probes[1].LastRTTus = 3_000
	for i := 1; i < holdRounds; i++ {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		if st := s.Status(); st[0].Color != 2 {
			t.Fatalf("第 %d 拍不应切换（迟滞未到），实际 %+v", i, st)
		}
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if st := s.Status(); st[0].Color != 3 {
		t.Fatalf("第 %d 拍应切到颜色 3，实际 %+v", holdRounds, st)
	}
}

// 当前颜色 100% 丢包：立即切，不等迟滞。
func TestDeadCurrentSwitchesImmediately(t *testing.T) {
	f := &fakePlane{}
	s := newSelectorWith(f)
	s.Reconcile([]Row{row(1, 10001)}, []Tunnel{
		tunnel(10, 3, 2),
		tunnel(11, 3, 3),
	})
	f.probes = []vppapi.ProbeStats{
		probe(10, 100, 0, 8_000, 1_000),
		probe(11, 100, 0, 30_000, 1_000),
	}
	p := f
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if st := s.Status(); st[0].Color != 2 {
		t.Fatalf("初选应为颜色 2，实际 %+v", st)
	}
	p.probes[0].LossPermille = 1000
	p.probes[0].Rx = 0
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if st := s.Status(); st[0].Color != 3 {
		t.Fatalf("当前颜色全丢应立即切到颜色 3，实际 %+v", st)
	}
}

// VPP 重启把选路表清空：下一拍必须把路由恢复回来（哪怕决策没变）。
func TestRouteRestoredAfterVPPRestart(t *testing.T) {
	f := &fakePlane{}
	s := newSelectorWith(f)
	s.Reconcile([]Row{row(1, 10001)}, []Tunnel{tunnel(10, 3, 2)})
	f.probes = []vppapi.ProbeStats{probe(10, 100, 0, 8_000, 1_000)}
	p := f
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if p.written != 1 {
		t.Fatalf("首拍应写一次路由，实际 %d", p.written)
	}
	// 决策不变 → 不再写。
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if p.written != 1 {
		t.Fatalf("决策不变不应重写，实际 %d 次", p.written)
	}
	// VPP 重启：表被清空。
	p.routes = map[uint32]map[netip.Prefix]bool{}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if p.written != 2 {
		t.Fatalf("表被清空后应恢复路由，实际写了 %d 次", p.written)
	}
	if st := s.Status(); st[0].Color != 2 {
		t.Fatalf("恢复的颜色应是 2，实际 %+v", st)
	}
}

// 对端没有候选隧道（隧道正在收敛）：决策保持颜色 0；选路表里没有路由时补上
// 指向主表的恢复路由 —— 这与 apply 的种子等价，避免表真的空着变成黑洞。
func TestNoCandidatesKeepsCurrent(t *testing.T) {
	f := &fakePlane{}
	s := newSelectorWith(f)
	s.Reconcile([]Row{row(1, 10001)}, []Tunnel{tunnel(10, 9, 2)}) // 对端是 9，不是 3
	f.probes = []vppapi.ProbeStats{probe(10, 100, 0, 8_000, 1_000)}
	p := f
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if st := s.Status(); st[0].Color != 0 {
		t.Fatalf("没有候选应保持颜色 0，实际 %+v", st)
	}
	// 第二拍：路由已在、决策没变 → 不再写。
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if p.written != 1 {
		t.Fatalf("决策不变不应重写，实际 %d 次", p.written)
	}
}
