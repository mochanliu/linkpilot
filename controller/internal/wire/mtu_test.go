package wire

import "testing"

// 这个文件守的是**契约两侧的同一把尺子**。控制器按预算组路径、agent 按预算拒配置，
// 两边的数必须完全一致 —— 不一致的表现不是报错，是一条"能通但会黑洞"的隧道，
// 所以宁可在这里把每个常数钉死。
//
// 每个被钉住的数都来自实机测量（VPP 26.06 + sr_wg，实验室 srwg-* 三节点），
// test/scripts/test-mtu.sh 会在真数据面上重测并断言同样的关系。谁改了这里的常数，
// 那个脚本就会红。

func TestSRHeaderBytesMatchesTheCAuthority(t *testing.T) {
	// vpp-plugin/sr_header.h: SR_HDR_LEN(n) = 24 + n*6，且 n=0 时**根本不带头**。
	// 段数是插件的上界（SR_MAX_SEGS=8）决定了最坏情况，所以两头都要钉住。
	cases := []struct {
		segs uint8
		want int
	}{
		{0, 0},
		{1, 30},
		{2, 36},
		{8, 72},
	}
	for _, tc := range cases {
		if got := SRHeaderBytes(tc.segs); got != tc.want {
			t.Errorf("SRHeaderBytes(%d) = %d，应是 %d（sr_header.h 的 24 + n*6）", tc.segs, got, tc.want)
		}
	}
	if MaxSRSegments != 8 {
		t.Errorf("最大段数应与 SR_MAX_SEGS 一致（8），实际 %d", MaxSRSegments)
	}
	// 超过上界时预算只会更小，不会算出"合法但更危险"的值：这里记录的是方向，
	// 真数据面对 >8 的路径是直接拒绝组路的。
	if SRHeaderBytes(MaxSRSegments) != SRFixedBytes+MaxSRSegments*SRSegmentBytes {
		t.Error("最大段数的头长应与公式一致")
	}
}

func TestTunnelMTUBudget(t *testing.T) {
	cases := []struct {
		underlay uint16
		segs     uint8
		want     uint16
	}{
		{1500, 0, 1440}, // 1500-28-32-0
		{1500, 1, 1410}, // 再减一段 = 30 B
		{1500, 2, 1404}, // 实测过的一组：内层 136 → UDP payload 204
		{1500, 8, 1368}, // 最坏情况；默认 1360 就是照它选的
		{1450, 8, 1318}, // PPPoE/CGNAT 常见的 1450：默认值在这里就**超预算**
		{296, 8, 164},   // 小 underlay 照样按公式减，不做"至少留一点"的安慰
		{132, 8, 0},     // 132-28-32-72 = 0：一个内层字节都装不下 → 调用方必须拒配
		{131, 8, 0},     // 减到负数也被夹回 0，不会回一个绕了一圈的大 uint16
		{100, 8, 0},
	}
	for _, tc := range cases {
		if got := TunnelMTUBudget(tc.underlay, tc.segs); got != tc.want {
			t.Errorf("TunnelMTUBudget(%d, %d) = %d，应是 %d", tc.underlay, tc.segs, got, tc.want)
		}
	}
}

// 预算的**定义性**性质：按预算取内层尺寸，外层正好铺满 underlay 的 MTU，
// 一片不多。这条等式是整个链条的一致性检查 —— 任何一侧（WG 开销、SR 头、外层
// IPv4+UDP）改了常数却没同步，这里先炸。
func TestBudgetIsExactlyTheUnderlayMinusOverhead(t *testing.T) {
	for _, underlay := range []uint16{1500, 1450, 1400, 9000} {
		for segs := uint8(0); segs <= MaxSRSegments; segs++ {
			budget := TunnelMTUBudget(underlay, segs)
			if budget == 0 {
				continue
			}
			outer := int(budget) + WGDataOverheadBytes + SRHeaderBytes(segs) + OuterUnderlayBytes
			if outer != int(underlay) {
				t.Fatalf("underlay %d / %d 段：预算 %d 拼回外层是 %d，不等于 %d",
					underlay, segs, budget, outer, underlay)
			}
		}
	}
}

// 默认值之所以是"默认"，靠的是它在**这个 underlay 的最坏路径**上仍安全。
// 两头都要钉：1500 上不出事，1450 上出事（否则它就该是编译期常量，不必下发）。
func TestDefaultTunnelMTUIsSafeWorstCase(t *testing.T) {
	if b := TunnelMTUBudget(1500, MaxSRSegments); DefaultTunnelMTU > b {
		t.Errorf("默认 MTU %d 在 1500 underlay、%d 段时超预算 %d", DefaultTunnelMTU, MaxSRSegments, b)
	}
	if b := TunnelMTUBudget(1450, MaxSRSegments); DefaultTunnelMTU <= b {
		t.Errorf("默认 MTU %d 竟在 1450 underlay 上也安全（预算 %d）—— 那它就不该是下发字段",
			DefaultTunnelMTU, b)
	}
	// 默认值必须落在区间内，否则控制器一启动就被自己的校验拦死。
	if DefaultTunnelMTU < MinTunnelMTU || DefaultTunnelMTU > MaxTunnelMTU {
		t.Errorf("默认 MTU %d 出界 %d..%d", DefaultTunnelMTU, MinTunnelMTU, MaxTunnelMTU)
	}
}

// 区间常数不能与预算脱钩：下界 576 是最小的合规 IPv4 重组尺寸，所以**最小的
// 可用 underlay** 也必须容得下它，否则 MinTunnelMTU 是个永远无法满足的数。
func TestTunnelMTUBoundsAreReachable(t *testing.T) {
	if MinTunnelMTU >= MaxTunnelMTU {
		t.Fatal("MTU 区间是空的")
	}
	// 1500 的 underlay 即使 8 段也给得起 576。
	if b := TunnelMTUBudget(1500, MaxSRSegments); b < MinTunnelMTU {
		t.Errorf("1500 underlay 的预算 %d 连下界 %d 都容不下", b, MinTunnelMTU)
	}
	// 反过来说：最小的 underlay 也得有个数能让默认值过 —— 1360 需要
	// 1360+32+72+28 = 1492，脚本用这个数验证实验室配置。
	need := int(DefaultTunnelMTU) + WGDataOverheadBytes + SRHeaderBytes(MaxSRSegments) + OuterUnderlayBytes
	if need > 1500 {
		t.Errorf("默认 MTU 在最坏情况下外层要 %d B，已超过 1500：默认值不安全", need)
	}
}

// MSS 与 MTU 的关系是契约，不是 agent 的实现细节：控制器要把这个数报给运维，
// agent 要把它写进数据面，两边必须同一套算术（2026-10-24 定案：MTU - 44，
// 多出的 4 B 是 VPN VRF 路由压入的 MPLS 标签）。
func TestTCPMSSForTunnelMTU(t *testing.T) {
	for _, tc := range []struct {
		mtu, want uint16
	}{
		{DefaultTunnelMTU, DefaultTunnelMTU - 44}, // 1360 → 1316
		{1500, 1456},
		{MinTunnelMTU, MinTunnelMTU - 44},
		{MaxTunnelMTU, MaxTunnelMTU - 44},
		// 小于 44 的 MTU 在契约里已经出界；这里只保证不 uint16 回绕成一个巨大 MSS。
		{44, 0},
		{0, 0},
	} {
		if got := TCPMSSForTunnelMTU(tc.mtu); got != tc.want {
			t.Errorf("TCPMSSForTunnelMTU(%d) = %d，期望 %d", tc.mtu, got, tc.want)
		}
	}
}
