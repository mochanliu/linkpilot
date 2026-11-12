// NAT 对账的单元测试。形状 G 的四个前置条件在这里各有一条断言，顺序也断言：
// 身份映射必须早于 outside 特性，否则中间任何一刻外口都在丢管理流量。
package apply

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"

	"sdwan/controller/internal/vppapi"
)

// fakeNAT 是一个 VPP 的替身：只实现这一段用到的消息，状态放在 map 里，
// 每次调用按顺序记进 seq（顺序本身是被测的东西）。
type fakeNAT struct {
	ifaces []vppapi.SwInterface
	on     bool
	feat   map[uint32]byte // sw_if_index → flags
	pool   map[netip.Addr]bool
	ident  map[netip.Addr]bool
	seq    []string
	err    map[string]error
}

func newFakeNAT(ifs ...vppapi.SwInterface) *fakeNAT {
	return &fakeNAT{
		ifaces: ifs,
		feat:   map[uint32]byte{},
		pool:   map[netip.Addr]bool{},
		ident:  map[netip.Addr]bool{},
		err:    map[string]error{},
	}
}

func (f *fakeNAT) note(s string) { f.seq = append(f.seq, s) }

func (f *fakeNAT) DumpInterfaces(context.Context) ([]vppapi.SwInterface, error) {
	return f.ifaces, nil
}

func (f *fakeNAT) DumpIPv4Addresses(_ context.Context, idx uint32) ([]vppapi.Prefix, error) {
	for _, it := range f.ifaces {
		if it.SWIfIndex == idx {
			return it.IPv4, nil
		}
	}
	return nil, nil
}

func (f *fakeNAT) Nat44Enable(_ context.Context, _ vppapi.Nat44Params, enable bool) error {
	if err := f.err["enable"]; err != nil {
		return err
	}
	if enable && f.on {
		return vppapi.APIError{Op: "nat44_ei_plugin_enable_disable", Retval: natRetvalEnabled}
	}
	f.on = enable
	f.note(map[bool]string{true: "enable", false: "disable"}[enable])
	return nil
}

func (f *fakeNAT) Nat44InterfaceFeature(_ context.Context, idx uint32, flags byte, add bool) error {
	if err := f.err["feature"]; err != nil {
		return err
	}
	if add {
		if f.feat[idx]&flags != 0 {
			return vppapi.APIError{Op: "nat44_ei_interface_add_del_feature", Retval: natRetvalExists}
		}
		f.feat[idx] |= flags
		f.note("feature+")
		return nil
	}
	f.feat[idx] &^= flags
	f.note("feature-")
	return nil
}

func (f *fakeNAT) Nat44AddressRange(_ context.Context, first, last netip.Addr, _ uint32, add bool) error {
	if err := f.err["range"]; err != nil {
		return err
	}
	if add {
		if f.pool[first] {
			return vppapi.APIError{Op: "nat44_ei_add_del_address_range", Retval: natRetvalExists}
		}
		f.pool[first] = true
		f.note("range+")
		return nil
	}
	delete(f.pool, first)
	f.note("range-")
	return nil
}

func (f *fakeNAT) Nat44IdentityMapping(_ context.Context, addr netip.Addr, _ uint32, add bool) error {
	if err := f.err["identity"]; err != nil {
		return err
	}
	if add {
		if f.ident[addr] {
			return vppapi.APIError{Op: "nat44_ei_add_del_identity_mapping", Retval: natRetvalExists}
		}
		f.ident[addr] = true
		f.note("ident+")
		return nil
	}
	delete(f.ident, addr)
	f.note("ident-")
	return nil
}

func (f *fakeNAT) Nat44Interfaces(context.Context) ([]vppapi.Nat44Interface, error) {
	var out []vppapi.Nat44Interface
	for idx, flags := range f.feat {
		if flags == 0 {
			continue
		}
		out = append(out, vppapi.Nat44Interface{
			SWIfIndex: idx,
			Inside:    flags&vppapi.Nat44FlagIfInside != 0,
			Outside:   flags&vppapi.Nat44FlagIfOutside != 0,
		})
	}
	return out, nil
}

func (f *fakeNAT) Nat44PoolAddresses(context.Context) ([]vppapi.Nat44PoolAddress, error) {
	var out []vppapi.Nat44PoolAddress
	for a := range f.pool {
		out = append(out, vppapi.Nat44PoolAddress{Addr: a})
	}
	return out, nil
}

func (f *fakeNAT) Close() error { return nil }

func (f *fakeNAT) index(sub string) int {
	for i, s := range f.seq {
		if strings.Contains(s, sub) {
			return i
		}
	}
	return -1
}

func swIf(idx uint32, name string, addrs ...string) vppapi.SwInterface {
	it := vppapi.SwInterface{SWIfIndex: idx, Name: name}
	for _, s := range addrs {
		p := netip.MustParsePrefix(s)
		it.IPv4 = append(it.IPv4, vppapi.Prefix{IP: net.IP(p.Addr().AsSlice()), Len: uint8(p.Bits())})
	}
	return it
}

func natTestApplier(t *testing.T, plane *fakeNAT, fx *fakeExec) *Applier {
	t.Helper()
	// vppctl 的地址增删要反馈回替身：真盒子上 DumpInterfaces 看得见它们，
	// 幂等性靠的就是这一点。
	fx.hook = func(key string) {
		// vppctl set interface ip address [del] <iface> <prefix>
		f := strings.Fields(key)
		if len(f) < 7 || f[1] != "set" || f[4] != "address" {
			return
		}
		del := f[5] == "del"
		iface, addr := f[5], f[6]
		if del {
			if len(f) < 8 {
				return
			}
			iface, addr = f[6], f[7]
		}
		p, err := netip.ParsePrefix(addr)
		if err != nil {
			return
		}
		for i := range plane.ifaces {
			if plane.ifaces[i].Name != iface {
				continue
			}
			if del {
				var keep []vppapi.Prefix
				for _, vp := range plane.ifaces[i].IPv4 {
					if np, ok := netipPrefix(vp); !ok || np.Addr() != p.Addr() {
						keep = append(keep, vp)
					}
				}
				plane.ifaces[i].IPv4 = keep
				return
			}
			plane.ifaces[i].IPv4 = append(plane.ifaces[i].IPv4, vppPrefix(p))
			return
		}
	}
	a := newTestApplier(t, t.TempDir(), "/tmp/frr.conf", fx)
	a.VPPAPI = "/run/vpp/api.sock"
	a.DialNAT = func(context.Context, string) (natPlane, error) { return plane, nil }
	a.IfaceAddrs = func(_ context.Context, name string) ([]vppapi.Prefix, error) {
		for _, it := range plane.ifaces {
			if it.Name == name {
				return it.IPv4, nil
			}
		}
		return nil, nil
	}
	return a
}

func natRowDoc(on bool, vpn uint32, in, out string, pool ...string) svcNATRow {
	return svcNATRow{On: on, VPN: vpn, Inside: in, Outside: out, Pool: pool}
}

// 形状 G 的正例：池地址上口、外口地址有身份映射、两条特性都挂上，且顺序是
// identity → range → feature。
func TestNATAppliesShapeG(t *testing.T) {
	fx := &fakeExec{outputs: map[string]string{}}
	plane := newFakeNAT(
		swIf(1, "host-enp0s8"),
		swIf(3, "host-enp0s9", "192.168.1.104/24"),
	)
	a := natTestApplier(t, plane, fx)

	notes, err := a.applyNAT(context.Background(), []svcNATRow{
		natRowDoc(true, 0, "host-enp0s8", "host-enp0s9", "192.168.1.200"),
	}, Installed{})
	if err != nil {
		t.Fatalf("applyNAT: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("notes = %v，期望没有", notes)
	}
	if !plane.on {
		t.Fatal("插件没使能")
	}
	if !plane.ident[netip.MustParseAddr("192.168.1.104")] {
		t.Fatal("外口地址没有身份映射（管理面会失联）")
	}
	if !plane.pool[netip.MustParseAddr("192.168.1.200")] {
		t.Fatal("池地址没下发")
	}
	if plane.feat[1]&vppapi.Nat44FlagIfInside == 0 {
		t.Fatal("内口没有 inside 特性")
	}
	if plane.feat[3]&vppapi.Nat44FlagIfOutside == 0 {
		t.Fatal("外口没有 outside 特性")
	}
	if _, ok := plane.feat[3]; ok && plane.feat[3] != vppapi.Nat44FlagIfOutside {
		t.Fatalf("外口被挂上了别的方向：%d", plane.feat[3])
	}
	// 池地址要挂到外口上（上游要 ARP 它）。
	if fx.index("set interface ip address host-enp0s9 192.168.1.200/24") < 0 {
		t.Fatalf("没有把池地址挂到外口，执行的是: %v", fx.calls)
	}
	// 顺序：身份映射必须在 outside 特性之前。
	if i, j := plane.index("ident+"), plane.index("feature+"); i < 0 || j < 0 || i > j {
		t.Fatalf("顺序不对：identity=%d feature=%d (%v)", i, j, plane.seq)
	}
	// 不能出现 output-interface 那条路（形状 D 的 out of ports）。
	for _, s := range plane.seq {
		if strings.Contains(s, "output") {
			t.Fatalf("不该出现 output 特性：%v", plane.seq)
		}
	}
}

// 幂等：同样的行再跑一遍，VPP 回 VALUE_EXIST / 已使能，不能变成失败。
func TestNATSecondRunIsIdempotent(t *testing.T) {
	fx := &fakeExec{outputs: map[string]string{}}
	plane := newFakeNAT(swIf(1, "host-enp0s8"), swIf(3, "host-enp0s9", "192.168.1.104/24"))
	a := natTestApplier(t, plane, fx)
	rows := []svcNATRow{natRowDoc(true, 0, "host-enp0s8", "host-enp0s9", "192.168.1.200")}
	if _, err := a.applyNAT(context.Background(), rows, Installed{}); err != nil {
		t.Fatalf("第一轮: %v", err)
	}
	before := len(fx.calls)
	if _, err := a.applyNAT(context.Background(), rows, Installed{}); err != nil {
		t.Fatalf("第二轮: %v", err)
	}
	// 第二轮不该再挂地址（已经在口上了）。
	if got := len(fx.calls) - before; got != 0 {
		t.Fatalf("第二轮又跑了 %d 条 vppctl：%v", got, fx.calls[before:])
	}
}

// 上一版有 NAT、这一版没有：特性、池地址、身份映射、插件全退掉，外面的
// secondary 地址也摘掉。
func TestNATTeardownWhenRowsDisappear(t *testing.T) {
	fx := &fakeExec{outputs: map[string]string{
		"vppctl show interface address host-enp0s9": "host-enp0s9 (up):\n  L3 192.168.1.200/24\n",
	}}
	plane := newFakeNAT(swIf(1, "host-enp0s8"), swIf(3, "host-enp0s9", "192.168.1.104/24"))
	a := natTestApplier(t, plane, fx)

	prev := Installed{Services: `{"nat":[{"on":true,"vpn":0,"inside":"host-enp0s8","outside":"host-enp0s9","pool":["192.168.1.200"]}]}`}
	// 先真的把它配上，才有东西可退。
	if _, err := a.applyNAT(context.Background(), []svcNATRow{
		natRowDoc(true, 0, "host-enp0s8", "host-enp0s9", "192.168.1.200"),
	}, Installed{}); err != nil {
		t.Fatalf("准备状态: %v", err)
	}
	if _, err := a.applyNAT(context.Background(), nil, prev); err != nil {
		t.Fatalf("退场: %v", err)
	}
	if plane.on {
		t.Fatal("插件还开着")
	}
	if len(plane.feat) != 0 && (plane.feat[1] != 0 || plane.feat[3] != 0) {
		t.Fatalf("特性没摘干净: %v", plane.feat)
	}
	if len(plane.pool) != 0 {
		t.Fatalf("池地址没删干净: %v", plane.pool)
	}
	if fx.index("set interface ip address del host-enp0s9 192.168.1.200/24") < 0 {
		t.Fatalf("没有摘掉 secondary 地址，执行的是: %v", fx.calls)
	}
}

// 池地址不在外口网段内：报错点名行号，不猜、不替上游补地址。
func TestNATPoolOutsideSubnetFails(t *testing.T) {
	fx := &fakeExec{outputs: map[string]string{}}
	plane := newFakeNAT(swIf(1, "host-enp0s8"), swIf(3, "host-enp0s9", "192.168.1.104/24"))
	a := natTestApplier(t, plane, fx)
	_, err := a.applyNAT(context.Background(), []svcNATRow{
		natRowDoc(true, 0, "host-enp0s8", "host-enp0s9", "10.9.9.10"),
	}, Installed{})
	if err == nil || !strings.Contains(err.Error(), "不在外口") {
		t.Fatalf("err = %v，期望点名网段不匹配", err)
	}
	if plane.on {
		t.Fatal("报错前不该使能插件")
	}
}

// 半行配置（On 但没池）：不落地、要出声，而且不该去连 VPP。
func TestNATHalfRowNotesWithoutTouchingVPP(t *testing.T) {
	fx := &fakeExec{outputs: map[string]string{}}
	a := newTestApplier(t, t.TempDir(), "/tmp/frr.conf", fx)
	a.VPPAPI = "/run/vpp/api.sock"
	dialed := false
	a.DialNAT = func(context.Context, string) (natPlane, error) {
		dialed = true
		return newFakeNAT(), nil
	}
	notes, err := a.applyNAT(context.Background(), []svcNATRow{
		natRowDoc(true, 0, "host-enp0s8", "host-enp0s9"),
	}, Installed{})
	if err != nil {
		t.Fatalf("applyNAT: %v", err)
	}
	if dialed {
		t.Fatal("没有可下发的行却连了 VPP")
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "没有地址池") {
		t.Fatalf("notes = %v，期望一条'没有地址池'的说明", notes)
	}
}

// 身份映射失败必须是硬错误：它保护的是管理面，不能降级成 warning 继续挂特性。
func TestNATIdentityFailureAbortsBeforeFeatures(t *testing.T) {
	fx := &fakeExec{outputs: map[string]string{}}
	plane := newFakeNAT(swIf(1, "host-enp0s8"), swIf(3, "host-enp0s9", "192.168.1.104/24"))
	plane.err["identity"] = errors.New("boom")
	a := natTestApplier(t, plane, fx)
	_, err := a.applyNAT(context.Background(), []svcNATRow{
		natRowDoc(true, 0, "host-enp0s8", "host-enp0s9", "192.168.1.200"),
	}, Installed{})
	if err == nil || !strings.Contains(err.Error(), "身份映射") {
		t.Fatalf("err = %v，期望身份映射失败", err)
	}
	if plane.feat[3]&vppapi.Nat44FlagIfOutside != 0 {
		t.Fatal("身份映射失败后仍然挂了 outside 特性（盒子会失联）")
	}
}
