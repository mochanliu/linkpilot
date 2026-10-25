package frr

import (
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"testing"
)

func TestEncodeHello(t *testing.T) {
	got := encodeHello(0)
	want := "0012fe060000000000131800000000000000"
	if enc := hex.EncodeToString(got); enc != want {
		t.Fatalf("HELLO 编码不对:\n got %s\nwant %s", enc, want)
	}
}

// TestEncodeRouteGolden 把 spike 里被 zebra 接受的字节序列固定下来：
// 任何字段顺序或宽度变化都会在这里暴露，而不是等到 FRR 拒收。
func TestEncodeRouteGolden(t *testing.T) {
	r := Route{
		Prefix:   netip.MustParsePrefix("10.1.0.0/24"),
		TableID:  1000,
		Distance: 1,
		Metric:   20,
	}
	msg, err := encodeRoute(cmdRouteAdd, r)
	if err != nil {
		t.Fatal(err)
	}
	want := "002dfe06000000000009" +
		"10" + "0000" + "00000841" + "00000107" +
		"01" + "02" + "18" + "0a0100" +
		"0001" + "00000000" + "06" + "00" + "01" +
		"01" + "00000014" + "000003e8"
	if enc := hex.EncodeToString(msg); enc != want {
		t.Fatalf("ROUTE_ADD 编码不对:\n got %s\nwant %s", enc, want)
	}

	del, err := encodeRoute(cmdRouteDelete, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(del) != len(msg) {
		t.Fatalf("删除报文长度不同: %d vs %d", len(del), len(msg))
	}
	if binary.BigEndian.Uint16(del[8:10]) != cmdRouteDelete {
		t.Fatalf("删除报文命令号 %d", binary.BigEndian.Uint16(del[8:10]))
	}
}

// decodeRoutePayload 是测试用的最小解码器，用来检查不同前缀长度下的布局。
func decodeRoutePayload(t *testing.T, msg []byte) (plen int, pfx []byte, rest []byte) {
	t.Helper()
	if len(msg) < zapiHeaderSize {
		t.Fatalf("报文太短: %d", len(msg))
	}
	if int(binary.BigEndian.Uint16(msg[0:2])) != len(msg) {
		t.Fatalf("头部长度 %d 与实际 %d 不符", binary.BigEndian.Uint16(msg[0:2]), len(msg))
	}
	p := msg[zapiHeaderSize:]
	// type(1) instance(2) flags(4) message(4) safi(1) family(1) plen(1)
	plen = int(p[13])
	n := (plen + 7) / 8
	return plen, p[14 : 14+n], p[14+n:]
}

func TestEncodeRoutePrefixSizes(t *testing.T) {
	tests := []struct {
		prefix string
		plen   int
		bytes  string
	}{
		{"0.0.0.0/0", 0, ""},
		{"10.0.0.0/8", 8, "0a"},
		{"10.1.0.0/24", 24, "0a0100"},
		{"10.1.2.3/32", 32, "0a010203"},
	}
	for _, tc := range tests {
		msg, err := encodeRoute(cmdRouteAdd, Route{
			Prefix: netip.MustParsePrefix(tc.prefix), TableID: 1000, Distance: 1,
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.prefix, err)
		}
		plen, pfx, rest := decodeRoutePayload(t, msg)
		if plen != tc.plen {
			t.Errorf("%s: plen=%d want %d", tc.prefix, plen, tc.plen)
		}
		if hex.EncodeToString(pfx) != tc.bytes {
			t.Errorf("%s: 前缀字节 %x want %s", tc.prefix, pfx, tc.bytes)
		}
		// 前缀之后必须是 uint16 的下一跳数量 = 1，然后是黑洞下一跳。
		if len(rest) < 3 || binary.BigEndian.Uint16(rest[0:2]) != 1 {
			t.Fatalf("%s: 下一跳数量不对: %x", tc.prefix, rest)
		}
		if rest[6] != nhTypeBlackhole || rest[8] != bhTypeNull {
			t.Errorf("%s: 下一跳不是黑洞: %x", tc.prefix, rest[:12])
		}
	}
}

func TestEncodeRouteRejects(t *testing.T) {
	tests := map[string]Route{
		"IPv6":    {Prefix: netip.MustParsePrefix("2001:db8::/32"), TableID: 1000},
		"掩码外有有效位": {Prefix: netip.MustParsePrefix("10.1.0.1/24"), TableID: 1000},
		"表号为 0":   {Prefix: netip.MustParsePrefix("10.1.0.0/24")},
		"无效前缀":    {Prefix: netip.Prefix{}, TableID: 1000},
		"4in6":    {Prefix: netip.MustParsePrefix("::ffff:10.1.0.0/120"), TableID: 1000},
	}
	for name, r := range tests {
		if _, err := encodeRoute(cmdRouteAdd, r); err == nil {
			t.Errorf("%s: 期望报错，实际通过了", name)
		}
	}
}
