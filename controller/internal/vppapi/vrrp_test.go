// Byte-level tests for the vrrp half of this package: what we hand VPP, and how we
// read what it hands back.
//
// Two sources of truth, both from the box, and neither of them "the code looks
// right":
//
//   - the message names and CRCs come from /usr/share/vpp/api/plugins/vrrp.api.json
//     and the struct layouts from /usr/include/vpp_plugins/vrrp/vrrp.api_types.h,
//     both read on lab-cpe1 2026-11-02;
//   - the refusals (-7 INVALID_VALUE, -116 ENTRY_ALREADY_EXISTS, -6 NO_SUCH_ENTRY,
//     -73 INVALID_ARGUMENT) and the 74-byte details fixture come from
//     tools/vrrp-probe and live_vrrp_test.go against that same VPP.
//
// Why the fixture matters more here than anywhere else in this package: a
// vrrp_vr_details row is the *only* thing the reconciler reads, so one wrong offset
// becomes "delete the VR with the neighbour's priority" or "the VIP list is empty"
// -- and each of those looks like a config the box accepted. A frame I built by
// hand cannot prove the reader: my builder and my reader would be wrong in the
// same direction and the test would still pass.

package vppapi

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// vrrpFakeCRCs is the plugin half of VPP's registry as lab-cpe1 reports it
// (vl_api_version 0x674aea12, 2026-11-02). It lives in the test, not in
// requiredVersions: a box that does not load vrrp_plugin.so must still pass Dial,
// the same precedent as the sr_wg_plugin-optional messages. Three of the replies
// share CRC e8d4e804 because they are literally the same struct (retval only) --
// the fake keys on the name, so the collision costs nothing, but it is the reason
// add/del/start_stop cannot be told apart by their reply alone.
var vrrpFakeCRCs = map[string]string{
	"vrrp_vr_add_del":          "c5cf15aa",
	"vrrp_vr_add_del_reply":    "e8d4e804",
	"vrrp_vr_update":           "0b51e2f4",
	"vrrp_vr_update_reply":     "5317d608",
	"vrrp_vr_del":              "6029baa1",
	"vrrp_vr_del_reply":        "e8d4e804",
	"vrrp_vr_dump":             "f9e6675e",
	"vrrp_vr_details":          "46edcebd",
	"vrrp_vr_start_stop":       "0662a3b7",
	"vrrp_vr_start_stop_reply": "e8d4e804",
}

// withVRRP is the "plugin loaded" state; without it the fake is a box with no vrrp
// at all, which is a state the callers have to survive too.
func (f *fakeVPP) withVRRP() *fakeVPP {
	for name, crc := range vrrpFakeCRCs {
		f.versions[name] = crc
	}
	f.buildIDs()
	return f
}

func vrrpFixtureParams() VRRPParams {
	return VRRPParams{
		SWIfIndex:  7,
		VRID:       77,
		Priority:   120,
		IntervalCS: 100,
		Preempt:    true,
		Addrs:      []netip.Addr{netip.MustParseAddr("192.0.2.250"), netip.MustParseAddr("192.0.2.251")},
	}
}

// noRequestSent proves a refusal happened *before* the wire: a VR the box half
// created is worse than one it never saw, because the next apply finds it in the
// dump and reads it as someone else's hand-configured router.
func noRequestSent(t *testing.T, f *fakeVPP) {
	t.Helper()
	select {
	case r := <-f.requests:
		t.Fatalf("校验没挡住，还是发出去了: %s %d B", r.name, len(r.body))
	case <-time.After(200 * time.Millisecond):
	}
}

// waitForRequest skips the barrier frames a dump puts on the wire
// (sr_wg_stats_get) and finds the request actually under test.
func waitForRequest(t *testing.T, f *fakeVPP, name string) request {
	t.Helper()
	for i := 0; i < 6; i++ {
		if r := f.lastRequest(t); r.name == name {
			return r
		}
	}
	t.Fatalf("没等到 %s 请求", name)
	return request{}
}

// ==================== 消息表：没装插件的盒子也得能用 ====================

func TestVRRPWithoutPluginDialsButRefusesCalls(t *testing.T) {
	f := newFake(t) // 不注册 vrrp：等价于 vrrp_plugin.so 没加载
	c := f.dial(t)  // Dial 必须成功——vrrp 故意不在 requiredVersions 里
	if _, ok := c.MsgID("vrrp_vr_add_del"); ok {
		t.Fatal("假 VPP 不该有 vrrp 消息")
	}
	ctx, cancel := shortCtx()
	defer cancel()
	err := c.VRRPAdd(ctx, vrrpFixtureParams())
	if err == nil || !strings.Contains(err.Error(), "vrrp_vr_add_del") {
		t.Fatalf("要指名哪条消息没有，got: %v", err)
	}
	noRequestSent(t, f)
}

// ==================== 请求编码（字节级） ====================

// vrrp_vr_add_del 的体长是 14 + 17n：is_add(1) + conf(12) + n_addrs(1) + 地址。
// conf 是 packed 的，所以 vr_id/priority 两个 u8 紧贴 u16 interval 和 u32 flags，
// 中间没有对齐填充——按 u32 数偏移的人会把 interval 读到 flags 里。
func TestVRRPAddEncodesTheWholeVR(t *testing.T) {
	f := newFake(t).withVRRP()
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	p := vrrpFixtureParams()
	p.Accept = true
	if err := c.VRRPAdd(ctx, p); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if r.name != "vrrp_vr_add_del" {
		t.Fatalf("请求名 = %s", r.name)
	}
	if len(r.body) != 14+17*len(p.Addrs) {
		t.Fatalf("体长 = %d，期望 %d（is_add 1 + conf 12 + n_addrs 1 + 17/地址）",
			len(r.body), 14+17*len(p.Addrs))
	}
	if r.body[0] != 1 {
		t.Errorf("is_add = %d，add 这条路必须是 1", r.body[0])
	}
	if got := binary.BigEndian.Uint32(r.body[1:5]); got != 7 {
		t.Errorf("sw_if_index = %d", got)
	}
	if r.body[5] != 77 || r.body[6] != 120 {
		t.Errorf("vr_id/priority = %d/%d，期望 77/120（两个 u8 挨着，偏移一错就互换）", r.body[5], r.body[6])
	}
	if got := binary.BigEndian.Uint16(r.body[7:9]); got != 100 {
		t.Errorf("interval = %d，期望 100（厘秒：这里写 1000 就是通告快 10 倍）", got)
	}
	// VRRP_API_VR_PREEMPT=1 / ACCEPT=2 / UNICAST=4 / IPV6=8，实测自插件自己的
	// enumflags 表。IPV6 那一位永远不许出现：地址族是 VR 上的标志，而这条通路只配 v4。
	if got := binary.BigEndian.Uint32(r.body[9:13]); got != VRRPFlagPreempt|VRRPFlagAccept {
		t.Errorf("flags = %#x，期望 %#x", got, uint32(VRRPFlagPreempt|VRRPFlagAccept))
	}
	if r.body[13] != 2 {
		t.Errorf("n_addrs = %d，期望 2", r.body[13])
	}
	for i, addr := range p.Addrs {
		at := 14 + 17*i
		want := append([]byte{apiAddressFamilyIP4}, padded16(addr.AsSlice())...)
		if !equalBytes(r.body[at:at+17], want) {
			t.Errorf("第 %d 个地址 = %x，期望 %x（vl_api_address_t 是 af+16，v4 后面补 12 个 0）",
				i, r.body[at:at+17], want)
		}
	}
}

func TestVRRPFlagsCarryUnicastAndNeverIPv6(t *testing.T) {
	// 控制器的模型里没有 unicast（基础配置里没有 peer 可发），但 vppapi 留着这一位，
	// 因为读回来的 VR 可能带它——调和靠它把别人手配的 VR 认出来并重建。
	f := newFake(t).withVRRP()
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	p := vrrpFixtureParams()
	p.Unicast = true
	if err := c.VRRPAdd(ctx, p); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if got := binary.BigEndian.Uint32(r.body[9:13]); got != VRRPFlagPreempt|VRRPFlagUnicast {
		t.Errorf("flags = %#x，期望 %#x", got, uint32(VRRPFlagPreempt|VRRPFlagUnicast))
	}
	if got := binary.BigEndian.Uint32(r.body[9:13]); got&8 != 0 {
		t.Errorf("flags 里出现了 IPV6 位（%#x）：这条通路只配 v4", got)
	}
}

func TestVRRPDelByKeyEchoesEveryField(t *testing.T) {
	// is_add=0 也照样验字段：实测 interval=0 的删除回 -7（C10），所以删除必须把整条
	// VR 照原样回送。这既是这个函数的形状，也是 VRRPVR.Params() 存在的理由。
	f := newFake(t).withVRRP()
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	if err := c.VRRPDelByKey(ctx, vrrpFixtureParams()); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if len(r.body) != 14+17*2 {
		t.Fatalf("体长 = %d，删除走的是同一条消息，不该短", len(r.body))
	}
	if r.body[0] != 0 {
		t.Errorf("is_add = %d，删除必须是 0（否则是重复 add，回 -116）", r.body[0])
	}
	if got := binary.BigEndian.Uint16(r.body[7:9]); got != 100 {
		t.Errorf("interval = %d，期望照抄 100", got)
	}
}

func TestVRRPDelByIndexIsFourBytes(t *testing.T) {
	// vrrp_vr_del 只有 vrrp_index 这 4 字节，一个字段都不验。它危险的地方不在编码，
	// 在于那个下标是全局池里回收复用的（C3/C12），所以 internal/apply 不用它。
	f := newFake(t).withVRRP()
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	if err := c.VRRPDelByIndex(ctx, 3); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if r.name != "vrrp_vr_del" || len(r.body) != 4 {
		t.Fatalf("请求 = %s %d B，期望 vrrp_vr_del 4 B", r.name, len(r.body))
	}
	if got := binary.BigEndian.Uint32(r.body); got != 3 {
		t.Errorf("vrrp_index = %d", got)
	}
}

func TestVRRPUpdatePutsTheIndexFirst(t *testing.T) {
	f := newFake(t).withVRRP()
	f.vrrpUpdateIdx = 42
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	p := vrrpFixtureParams()
	idx, err := c.VRRPUpdate(ctx, 3, p)
	if err != nil {
		t.Fatal(err)
	}
	if idx != 42 {
		t.Errorf("回读 vrrp_index = %d，期望 42（应答是 retval(4)+index(4)，顺序反了就读出一个巨大的下标）", idx)
	}
	r := f.lastRequest(t)
	// 4(vrrp_index) + 13(conf 12 与 n_addrs) + 17/地址：add 那条是 1+13 开头，
	// 这里换成 4+13，其余字段一个偏移都不动。
	if len(r.body) != 4+13+17*2 {
		t.Fatalf("体长 = %d，期望 %d", len(r.body), 4+13+17*2)
	}
	if got := binary.BigEndian.Uint32(r.body[0:4]); got != 3 {
		t.Errorf("vrrp_index = %d，期望 3（update 只认它，按键找不到的：实测 -6/-73）", got)
	}
	// 索引后面那 14 字节和 add 的同一段完全同形。
	if binary.BigEndian.Uint32(r.body[4:8]) != 7 || r.body[8] != 77 || r.body[9] != 120 {
		t.Errorf("键与优先级 = %x，期望 口 7 / vr 77 / prio 120", r.body[4:10])
	}
	if binary.BigEndian.Uint16(r.body[10:12]) != 100 || binary.BigEndian.Uint32(r.body[12:16]) != 1 {
		t.Errorf("interval/flags = %x，期望 100cs + preempt", r.body[10:16])
	}
	if r.body[16] != 2 || !equalBytes(r.body[17:34], append([]byte{apiAddressFamilyIP4}, padded16(netip.MustParseAddr("192.0.2.250").AsSlice())...)) {
		t.Errorf("地址段 = %x，期望 n_addrs=2 后接 192.0.2.250", r.body[16:34])
	}
}

func TestVRRPUpdateRefusesReplyWithoutTheIndex(t *testing.T) {
	f := newFake(t).withVRRP()
	f.vrrpUpdateNoIdx = true
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	_, err := c.VRRPUpdate(ctx, 3, vrrpFixtureParams())
	if err == nil || !strings.Contains(err.Error(), "太短") {
		t.Fatalf("应答少了 4 字节必须报错，got: %v", err)
	}
}

func TestVRRPStartStopBody(t *testing.T) {
	f := newFake(t).withVRRP()
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	if err := c.VRRPStartStop(ctx, 7, 77, false, true); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	// vl_api_vrrp_vr_start_stop_t = sw_if_index(4) vr_id(1) is_ipv6(1) is_start(1)。
	// 后三个 u8 顺序写反，start 会变成「is_ipv6=1」而 vr_id 还在原位——于是这条
	// 请求打在一条根本不存在的 IPv6 VR 上。
	if len(r.body) != 7 || r.body[4] != 77 || r.body[5] != 0 || r.body[6] != 1 {
		t.Fatalf("体 = %x，期望 00000007 4d 00 01", r.body)
	}
	if err := c.VRRPStartStop(ctx, 7, 77, false, false); err != nil {
		t.Fatal(err)
	}
	if r = f.lastRequest(t); r.body[6] != 0 {
		t.Errorf("stop 的 is_start = %d，期望 0", r.body[6])
	}
	if err := c.VRRPStartStop(ctx, 7, 0, false, true); err == nil {
		t.Error("vr_id=0 不能发：VPP 的键里没有这一条，start 会打在别人的 VR 上")
	} else {
		noRequestSent(t, f)
	}
}

func TestVRRPAddRefusesBeforeSending(t *testing.T) {
	f := newFake(t).withVRRP()
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	cases := []struct {
		name   string
		mutate func(VRRPParams) VRRPParams
		want   string
	}{
		{"vr_id 0", func(p VRRPParams) VRRPParams { p.VRID = 0; return p }, "vr_id"},
		{"没有地址", func(p VRRPParams) VRRPParams { p.Addrs = nil; return p }, "至少"},
		{"地址是 0.0.0.0", func(p VRRPParams) VRRPParams {
			p.Addrs = []netip.Addr{netip.MustParseAddr("0.0.0.0")}
			return p
		}, "无效"},
		{"零值 Addr", func(p VRRPParams) VRRPParams {
			p.Addrs = []netip.Addr{{}}
			return p
		}, "无效"},
		{"IPv6 地址", func(p VRRPParams) VRRPParams {
			p.Addrs = []netip.Addr{netip.MustParseAddr("2001:db8::1")}
			return p
		}, "IPv4"},
		{"地址多于 255 个", func(p VRRPParams) VRRPParams {
			p.Addrs = make([]netip.Addr, 256)
			for i := range p.Addrs {
				p.Addrs[i] = netip.AddrFrom4([4]byte{192, 0, 2, byte(i)})
			}
			return p
		}, "u8"},
	}
	for _, tc := range cases {
		p := tc.mutate(vrrpFixtureParams())
		err := c.VRRPAdd(ctx, p)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v，期望报错里含「%s」", tc.name, err, tc.want)
		}
		// 三个入口共用同一段校验：任何一条漏了都会发出尺寸不对的帧。
		if err := c.VRRPDelByKey(ctx, p); err == nil {
			t.Errorf("%s: 删除也要验（VPP 在这条路上照样验字段）", tc.name)
		}
		if _, err := c.VRRPUpdate(ctx, 1, p); err == nil {
			t.Errorf("%s: update 也要验", tc.name)
		}
		noRequestSent(t, f)
	}
}

// ==================== retval 必须变成错误，不能变成「没报错」 ====================

// 这四个数都是 2026-11-02 在 lab-cpe1 上量出来的（C1/C2/C3/C10），名字来自
// vnet/error.h。把它们当参数表跑一遍，是因为这个包最坏的失败模式是「VPP 拒绝了，
// 调用方以为配上了」——退出码 0 不算证据（AGENTS.md §1.2）。
func TestVRRPPropagatesMeasuredRetvals(t *testing.T) {
	cases := []struct {
		msg    string
		retval int32
		call   func(*Client, context.Context) error
	}{
		{"vrrp_vr_add_del", -116, func(c *Client, ctx context.Context) error {
			return c.VRRPAdd(ctx, vrrpFixtureParams())
		}},
		{"vrrp_vr_add_del", -7, func(c *Client, ctx context.Context) error {
			// interval=0 是这一版盒子最贵的一条：add 和 del 两条路都回 -7（C1）。
			p := vrrpFixtureParams()
			p.IntervalCS = 0
			return c.VRRPAdd(ctx, p)
		}},
		{"vrrp_vr_add_del", -7, func(c *Client, ctx context.Context) error {
			return c.VRRPDelByKey(ctx, vrrpFixtureParams())
		}},
		{"vrrp_vr_del", -6, func(c *Client, ctx context.Context) error { return c.VRRPDelByIndex(ctx, 9) }},
		{"vrrp_vr_update", -73, func(c *Client, ctx context.Context) error {
			_, err := c.VRRPUpdate(ctx, 9, vrrpFixtureParams())
			return err
		}},
		{"vrrp_vr_start_stop", -6, func(c *Client, ctx context.Context) error {
			return c.VRRPStartStop(ctx, 7, 77, false, true)
		}},
	}
	for _, tc := range cases {
		f := newFake(t).withVRRP()
		f.retvals[tc.msg] = tc.retval
		c := f.dial(t)
		ctx, cancel := shortCtx()
		err := tc.call(c, ctx)
		cancel()
		var apiErr APIError
		if !errors.As(err, &apiErr) {
			t.Errorf("%s retval=%d 必须回 APIError，got %T: %v", tc.msg, tc.retval, err, err)
			continue
		}
		if apiErr.Retval != tc.retval {
			t.Errorf("%s: retval = %d，期望 %d", tc.msg, apiErr.Retval, tc.retval)
		}
		if !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: 错误里要点名消息，got: %v", tc.msg, err)
		}
	}
}

// ==================== 读回：vrrp_vr_details ====================

// realVRRPDetailsFrame 是 lab-cpe1（VPP 26.06-release）2026-11-02 抓回来的**一帧
// 原文**：tap99=#7 上加 vr77/prio120/100cs/preempt/两个 VIP，start 之后等过
// master_down 再 dump（live_vrrp_test.go 打印的 hex 原样抄回）。前 6 字节是应答头
// （msg_id 810 + context）；msg_id 是盒子自己发的号，每台不同，所以用的时候切掉。
//
// 每个字段的值都互不相同，偏移错一位读出的是另一个数而不是另一个 0：state=1、
// adv=100、skew=53、down=353、MAC 尾字节=77、tracking.priority=120。
func realVRRPDetailsFrame() []byte {
	hexstr := "032a00000005" + // msg_id 810, context 5
		"00000007" + // conf.sw_if_index = 7（tap99）
		"4d" + // conf.vr_id = 77
		"78" + // conf.priority = 120
		"0064" + // conf.interval = 100 厘秒
		"00000001" + // conf.flags = PREEMPT
		"00000001" + // runtime.state = BACKUP
		"0064" + // runtime.master_adv_int = 100
		"0035" + // runtime.skew = 53 = (256-120)*100/256
		"0161" + // runtime.master_down_int = 353 = 3*100+53
		"00005e00014d" + // runtime.mac = 00:00:5e:00:01:4d（RFC 5798 的 MAC 前缀 + vr_id）
		"00000000" + // runtime.tracking.interfaces_dec = 0
		"78" + // runtime.tracking.priority = 120
		"02" + // n_addrs
		"00" + "c00002fa" + strings.Repeat("00", 12) + // 192.0.2.250
		"00" + "c00002fb" + strings.Repeat("00", 12) // 192.0.2.251
	out, err := hex.DecodeString(hexstr)
	if err != nil {
		panic("vrrp 夹具本身写坏了: " + err.Error())
	}
	if len(out) != vrrpDetailsFixedLen+17*2 {
		panic(fmt.Sprintf("vrrp 夹具长度写坏了：%d B，期望 %d B", len(out), vrrpDetailsFixedLen+17*2))
	}
	return out
}

func TestVRRPDetailsDecodesFrameFromRealVPP(t *testing.T) {
	frame := realVRRPDetailsFrame()
	f := newFake(t).withVRRP()
	f.vrrpRaw = [][]byte{frame[6:]}
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	got, err := c.VRRPDump(ctx, ^uint32(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("读了 %d 条", len(got))
	}
	v := got[0]
	if v.SWIfIndex != 7 || v.VRID != 77 || v.Priority != 120 || v.IntervalCS != 100 {
		t.Errorf("conf 读回来 = 口#%d vr%d prio=%d int=%dcs", v.SWIfIndex, v.VRID, v.Priority, v.IntervalCS)
	}
	if !v.Preempt || v.Accept || v.Unicast {
		t.Errorf("flags = preempt %v accept %v unicast %v，期望只有 preempt", v.Preempt, v.Accept, v.Unicast)
	}
	if v.State != VRRPStateBackup {
		t.Errorf("state = %s，期望 BACKUP", v.State)
	}
	if v.MasterAdvCS != 100 || v.SkewCS != 53 || v.MasterDownCS != 353 {
		t.Errorf("定时器 = adv %d skew %d down %d，期望 100/53/353（真机在跑的值）",
			v.MasterAdvCS, v.SkewCS, v.MasterDownCS)
	}
	if v.MasterMAC.String() != "00:00:5e:00:01:4d" {
		t.Errorf("虚拟 MAC = %s，期望 00:00:5e:00:01:4d", v.MasterMAC)
	}
	if v.TrackIfDown != 0 || v.TrackPriority != 120 {
		t.Errorf("tracking = %d/%d，期望 0/120（这一段 5 字节，写歪了会吃到 n_addrs）",
			v.TrackIfDown, v.TrackPriority)
	}
	want := []string{"192.0.2.250", "192.0.2.251"}
	if len(v.Addrs) != 2 {
		t.Fatalf("地址读了 %d 条，期望 2（每条 17 字节：af + 16 的 union）", len(v.Addrs))
	}
	for i, a := range v.Addrs {
		if a.String() != want[i] {
			t.Errorf("第 %d 个地址 = %s，期望 %s", i, a, want[i])
		}
	}
}

// 调和删一条 VR 要照抄现状，而现状只有 dump 这一条来源。这一条测的是「读回来的
// 东西再发回去，字节必须和当初 add 发出去的一致」——删不掉、或者删错了别人的，
// 都是从这条断掉开始的。
func TestVRRPDeleteEchoesWhatTheDumpReadBack(t *testing.T) {
	f := newFake(t).withVRRP()
	f.vrrpRaw = [][]byte{realVRRPDetailsFrame()[6:]}
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()

	if err := c.VRRPAdd(ctx, vrrpFixtureParams()); err != nil {
		t.Fatal(err)
	}
	want := waitForRequest(t, f, "vrrp_vr_add_del").body

	live, err := c.VRRPDump(ctx, ^uint32(0))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.VRRPDelByKey(ctx, live[0].Params()); err != nil {
		t.Fatal(err)
	}
	got := waitForRequest(t, f, "vrrp_vr_add_del").body

	want = append([]byte{0}, want[1:]...) // 只有 is_add 该不同
	if !equalBytes(got, want) {
		t.Errorf("删除回送的字节和 add 不一致:\n got %x\nwant %x", got, want)
	}
}

func TestVRRPDumpRequestCarriesTheIndexAskedFor(t *testing.T) {
	f := newFake(t).withVRRP()
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	if _, err := c.VRRPDump(ctx, ^uint32(0)); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if r.name != "vrrp_vr_dump" || len(r.body) != 4 {
		t.Fatalf("请求 = %s %d B，期望 vrrp_vr_dump 4 B", r.name, len(r.body))
	}
	if got := binary.BigEndian.Uint32(r.body); got != ^uint32(0) {
		t.Errorf("sw_if_index = %#x，期望 ~0", got)
	}
	// 插件注释写的是「0,~0 == all」，实测这一版 0 一条都不回（C7），所以「全部」
	// 只能是 ~0。这条测的就是我们没有悄悄改成 0。
	if _, err := c.VRRPDump(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint32(waitForRequest(t, f, "vrrp_vr_dump").body); got != 7 {
		t.Errorf("单口查询发了 %#x，期望 7", got)
	}
}

func TestVRRPDumpKeepsOnlyTheInterfaceAskedFor(t *testing.T) {
	// 假端故意不过滤（真机过滤，C7），所以这条测的是 VRRPDump 自己的兜底：参数是对
	// 调用方的承诺，插件升级后不再过滤时不能悄悄交出别人的行。
	other := realVRRPDetailsFrame()
	other[6], other[7], other[8], other[9] = 0, 0, 0, 8 // sw_if_index = 8
	f := newFake(t).withVRRP()
	f.vrrpRaw = [][]byte{realVRRPDetailsFrame()[6:], other[6:]}
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	got, err := c.VRRPDump(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SWIfIndex != 7 {
		t.Fatalf("要 #7 却读了 %d 条：%+v", len(got), got)
	}
	all, err := c.VRRPDump(ctx, ^uint32(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("~0 应该两条都要：%d 条", len(all))
	}
}

func TestVRRPDumpEmptyBoxIsNotAnError(t *testing.T) {
	// VPP 对空结果一帧都不发（这套 API 的 dump 没有结束标记）。调和把「一条 VR
	// 都没有」当正常状态，所以这里不能报错。
	f := newFake(t).withVRRP()
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	got, err := c.VRRPDump(ctx, ^uint32(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("假 VPP 一帧没发，却读了 %d 条", len(got))
	}
}

func TestVRRPDumpRejectsShortFrame(t *testing.T) {
	raw := realVRRPDetailsFrame()[6:]
	cuts := []int{len(raw) - 1, len(raw) - 20} // 少一字节，和少掉一整个地址
	for _, n := range cuts {
		f := newFake(t).withVRRP()
		f.vrrpRaw = [][]byte{append([]byte(nil), raw[:n]...)}
		c := f.dial(t)
		ctx, cancel := shortCtx()
		_, err := c.VRRPDump(ctx, ^uint32(0))
		cancel()
		if err == nil {
			t.Fatalf("帧长 %d 必须报错（插件结构变了要立刻看见，不是读出一堆 0）", n)
		}
		if !strings.Contains(err.Error(), "vrrp_vr_details") {
			t.Errorf("错误要点名消息，got: %v", err)
		}
	}
}

func TestVRRPDumpRefusesUnreadableAddressFamily(t *testing.T) {
	// 地址族那个字节读不出 v4/v6 时，netip 那边会给成 0.0.0.0 —— 而 0.0.0.0 作为
	// VIP 发下去，VPP 是照单收下的。这一条把「宁可乐也不猜」钉在解析层。
	// payload 的第 34 字节 = 整帧的第 40 字节 = 第一个地址的 af。
	raw := append([]byte(nil), realVRRPDetailsFrame()[6:]...)
	raw[34] = 7
	f := newFake(t).withVRRP()
	f.vrrpRaw = [][]byte{raw}
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	_, err := c.VRRPDump(ctx, ^uint32(0))
	if err == nil || !strings.Contains(err.Error(), "地址族") {
		t.Fatalf("got: %v", err)
	}
}

func TestVRRPDumpRefusesNAddrsBeyondTheFrame(t *testing.T) {
	raw := append([]byte(nil), realVRRPDetailsFrame()[6:]...)
	raw[33] = 3 // n_addrs 谎报：帧里只有 2 个地址
	f := newFake(t).withVRRP()
	f.vrrpRaw = [][]byte{raw}
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	_, err := c.VRRPDump(ctx, ^uint32(0))
	if err == nil || !strings.Contains(err.Error(), "3 个地址") {
		t.Fatalf("got: %v", err)
	}
}
