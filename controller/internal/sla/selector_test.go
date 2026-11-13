package sla

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"sdwan/controller/internal/vppapi"
)

// fakePlane 记录写下去的路由，喂回预置的探针读数。
type fakePlane struct {
	probes []vppapi.ProbeStats
	routes map[uint32]map[netip.Prefix]bool
	// deag[table] = 最近一次写进这张选路表的默认路由指向的表号（即选中的颜色表）。
	deag      map[uint32]uint32
	probeErr  error
	routesErr error // 模拟连接断开（VPP 重启中）：IPRoutes 报错
	written   int
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
	if f.routesErr != nil {
		return nil, f.routesErr
	}
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

func preemptRow(id int, table uint32) Row {
	r := row(id, table)
	r.Preempt = true
	return r
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

// 迟滞（抢占模式）：双方都达标时，更优者要连续 holdRounds 拍才接管。
func TestHysteresisHoldsHealthySwitch(t *testing.T) {
	f := &fakePlane{}
	s := newSelectorWith(f)
	s.Reconcile([]Row{preemptRow(1, 10001)}, []Tunnel{
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

// 粘住模式（默认，不勾抢占）：当前隧道还达标就一直用，更优的候选不抢；直到
// 它不达标才切到达标的候选（owner 2026-11-11 拍）。
func TestStickyKeepsEligibleCurrentUntilItFailsSLA(t *testing.T) {
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
	// 颜色 3 变得远比颜色 2 优：粘住模式按兵不动（抢占模式这里会走迟滞切换）。
	p.probes[1].LastRTTus = 1_000
	for i := 0; i < holdRounds+2; i++ {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		if st := s.Status(); st[0].Color != 2 {
			t.Fatalf("未勾抢占：当前仍达标就不该被颜色 3 抢走，第 %d 拍实际 %+v", i, st)
		}
	}
	if p.written != 1 {
		t.Fatalf("粘住期间不应写路由，实际 %d 次", p.written)
	}
	// 颜色 2 劣化到不达标（延迟 80ms > 50ms）：立即切到达标的颜色 3。
	p.probes[0].LastRTTus = 80_000
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if st := s.Status(); st[0].Color != 3 {
		t.Fatalf("当前颜色不达标且颜色 3 达标，应立即切换，实际 %+v", st)
	}
	if got := p.deag[10001]; got != 3000 {
		t.Fatalf("选路表 10001 应指向颜色表 3000，实际 %v", got)
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

// 实机缺陷复现（CPE1 2026-11-11，跨站客户流量全黑）：VPP 重启后 apply 管道把
// 选路表的 0/0 重种成 drop，表"存在"了；而 agent 内存里 written 还是 true、
// 决策也没变 —— 只看存在性会把种子 drop 当成自己写的路由，永远不再写，
// 0/0 落 dpo-drop。连接代数修复：重拨换代后必须幂等重写一次，盖掉 drop。
func TestSeedDropRouteReplacedAfterVPPRestart(t *testing.T) {
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
	// VPP 重启：表清空 → apply 重种 drop 0/0；连接断开一拍（读表报错）。
	seed := netip.MustParsePrefix("0.0.0.0/0")
	p.routes = map[uint32]map[netip.Prefix]bool{10001: {seed: true}}
	p.routesErr = errors.New("vpp 重启中")
	if err := s.Tick(context.Background()); err == nil {
		t.Fatal("连接断开时这一拍应报错")
	}
	// VPP 回来了：连接重拨（换代），表里有 drop 0/0。
	p.routesErr = nil
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if p.written != 2 {
		t.Fatalf("换代后应幂等重写盖掉 drop 路由，实际写了 %d 次", p.written)
	}
	if got := p.deag[10001]; got != 2000 {
		t.Fatalf("选路表 10001 应指向颜色表 2000（drop 被盖掉），实际 %v", got)
	}
	// 稳定后不再多写。
	if err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if p.written != 2 {
		t.Fatalf("稳定后不应重写，实际 %d 次", p.written)
	}
}
