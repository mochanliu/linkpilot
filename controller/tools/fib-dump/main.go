// fib-dump 读回一张 VRF 表里的路由，把每条路径的 preference 打出来。
//
// 存在的理由只有一句话：`vppctl show ip fib` 不打印 preference。颜色有没有真
// 写进 VPP（10+颜色），只能从 ip_route_details 里读 —— 也就是 O1 给 vppapi
// 补上的那条读回路径。转发行为看不出来：两个 bucket 无论同色还是异色，
// `buckets:` 都是 2，所以这条工具是"颜色落地"唯一的直接证据。
package main

import (
	"context"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"os/signal"
	"syscall"

	"sdwan/controller/internal/fib"
	"sdwan/controller/internal/vppapi"
)

func main() {
	socket := flag.String("socket", "/run/vpp/api.sock", "VPP 二进制 API socket")
	table := flag.Uint("table", 0, "要读回的 VRF 表号")
	prefix := flag.String("prefix", "", "只看这个前缀（默认整张表）")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	vc, err := vppapi.Dial(ctx, *socket, "fib-dump")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fib-dump: 连 VPP: %v\n", err)
		os.Exit(1)
	}
	defer vc.Close()

	routes, err := vc.FIBRoutes(ctx, uint32(*table))
	if err != nil {
		fmt.Fprintf(os.Stderr, "fib-dump: 读表 %d: %v\n", *table, err)
		os.Exit(1)
	}
	ifs, err := vc.DumpInterfaces(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fib-dump: 读接口表: %v\n", err)
		os.Exit(1)
	}
	name := map[uint32]string{}
	for _, i := range ifs {
		name[i.SWIfIndex] = i.Name
	}

	want := ""
	if *prefix != "" {
		p, err := netip.ParsePrefix(*prefix)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fib-dump: -prefix %q: %v\n", *prefix, err)
			os.Exit(1)
		}
		want = p.Masked().String()
	}

	found := 0
	for _, r := range routes {
		if want != "" && r.Prefix.String() != want {
			continue
		}
		found++
		fmt.Printf("%s  table=%d  paths=%d\n", r.Prefix, r.TableID, len(r.Paths))
		for _, p := range r.Paths {
			labels := "无"
			if len(p.Labels) > 0 {
				labels = fmt.Sprint(p.Labels)
			}
			iface := fmt.Sprintf("#%d", p.SWIfIndex)
			if n, ok := name[p.SWIfIndex]; ok {
				iface = n
			}
			fmt.Printf("  via %-15s %-8s 标签=%s preference=%d（颜色 %s）\n",
				p.NextHop, iface, labels, p.Preference, colorOf(p.Preference))
		}
	}
	if want != "" && found == 0 {
		fmt.Fprintf(os.Stderr, "fib-dump: 表 %d 里没有 %s\n", *table, want)
		os.Exit(1)
	}
}

// colorOf 反推 10+颜色。preference 落在 10+0..7 之外说明这条路径不是我们的
// 加载器写的（例如 FRR 经 lcp-rt 装进来的路由），把它和真颜色分开来才不骗人。
func colorOf(pref uint8) string {
	if pref < fib.PrefBase || pref > fib.PrefBase+fib.MaxColor {
		return "非本加载器"
	}
	return fmt.Sprint(int(pref - fib.PrefBase))
}
