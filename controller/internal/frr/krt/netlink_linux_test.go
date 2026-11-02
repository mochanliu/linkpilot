//go:build linux

package krt

import (
	"context"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"sdwan/controller/internal/frr"
)

func TestIndexLo(t *testing.T) {
	got, err := Index(0)
	if err != nil {
		t.Fatal(err)
	}
	ifi, err := net.InterfaceByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if got != ifi.Index {
		t.Fatalf("VPN 0 的出口 ifindex=%d，lo 实际是 %d", got, ifi.Index)
	}
}

// TestRouteRoundTrip 在 scratch 表 198 里写一条再删掉。需要 CAP_NET_ADMIN：
// 在 lab 容器里以 root 跑，宿主机上普通用户会 skip。
func TestRouteRoundTrip(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("需要 root/CAP_NET_ADMIN")
	}
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	lo, err := Index(0)
	if err != nil {
		t.Fatal(err)
	}
	r := frr.Route{Prefix: netip.MustParsePrefix("10.255.255.0/24"), Table: 198, OIF: lo}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.RouteAdd(ctx, r); err != nil {
		t.Fatalf("加: %v", err)
	}
	if err := c.RouteDelete(ctx, r); err != nil {
		t.Fatalf("删: %v", err)
	}
	// 已经删掉了；内核回 ESRCH，RouteDelete 必须当成功。
	if err := c.RouteDelete(ctx, r); err != nil {
		t.Fatalf("重复删应容忍 ESRCH: %v", err)
	}
}

func TestEncodeRouteRejects(t *testing.T) {
	lo := 1
	tests := map[string]frr.Route{
		"IPv6":    {Prefix: netip.MustParsePrefix("2001:db8::/32"), Table: 198, OIF: lo},
		"掩码外有有效位": {Prefix: netip.MustParsePrefix("10.1.0.1/24"), Table: 198, OIF: lo},
		"表号为 0":   {Prefix: netip.MustParsePrefix("10.1.0.0/24"), OIF: lo},
		"表号超 255": {Prefix: netip.MustParsePrefix("10.1.0.0/24"), Table: 256, OIF: lo},
		"出口为 0":   {Prefix: netip.MustParsePrefix("10.1.0.0/24"), Table: 198},
		"无效前缀":    {Prefix: netip.Prefix{}, Table: 198, OIF: lo},
	}
	for name, r := range tests {
		if _, err := encodeRoute(rtmNewRoute, nlmFRequest, r); err == nil {
			t.Errorf("%s: 期望报错，实际通过了", name)
		}
	}
}
