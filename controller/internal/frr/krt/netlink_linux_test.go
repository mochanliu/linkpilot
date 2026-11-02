//go:build linux

package krt

import (
	"context"
	"errors"
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
	// 只建不覆盖：再来一次必须回 ErrRouteExists，不能悄悄替换。
	if err := c.RouteAdd(ctx, r); !errors.Is(err, frr.ErrRouteExists) {
		t.Fatalf("重复加应回 ErrRouteExists，实际 %v", err)
	}
	if ok, err := c.Owns(ctx, r); err != nil || !ok {
		t.Fatalf("刚写的路由应认成我们的: ok=%v err=%v", ok, err)
	}
	other := frr.Route{Prefix: netip.MustParsePrefix("10.255.254.0/24"), Table: 198, OIF: lo}
	if ok, err := c.Owns(ctx, other); err != nil || ok {
		t.Fatalf("不存在的路由不该认成我们的: ok=%v err=%v", ok, err)
	}
	if err := c.RouteDelete(ctx, r); err != nil {
		t.Fatalf("删: %v", err)
	}
	// 已经删掉了；内核回 ESRCH，RouteDelete 必须当成功。
	if err := c.RouteDelete(ctx, r); err != nil {
		t.Fatalf("重复删应容忍 ESRCH: %v", err)
	}
}

// TestLocalSubnets 确认能读出盒子自己配了地址的前缀（VPN 0 的网段）。
func TestLocalSubnets(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	subs := c.LocalSubnets(254)
	if len(subs) == 0 {
		t.Fatal("默认 VRF 至少应有 lo 的 127.0.0.0/8")
	}
	if !subs[netip.MustParsePrefix("127.0.0.0/8")] {
		t.Fatalf("lo 的 127.0.0.0/8 不在结果里: %v", subs)
	}
	for p := range subs {
		if p != p.Masked() {
			t.Fatalf("应返回规范前缀: %s", p)
		}
	}
	if got := c.LocalSubnets(199); len(got) != 0 {
		t.Fatalf("不存在的 vpn199 不该有地址: %v", got)
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
