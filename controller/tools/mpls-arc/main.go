// mpls-arc 读回每张接口在 mpls-input 弧上的**真实**状态，用来回答一个问题：
// 这条隧道的口到底开没开成 MPLS。
//
// 为什么需要它：`vppctl show mpls interface` 读的是 VPP 内部那份按 sw_if_index
// 记的引用计数，而计数**不随接口删除清零** —— 一张没先 disable 就被删掉的口，
// 它腾出来的号重建下一条隧道后照样被列成 "MPLS enabled"，弧上却还挂着
// mpls-not-enabled，带标签的流量全丢（2026-10-30 实测，
// docs/overlay-design-draft.md §2.8）。诚实的只有一个地方：feature 弧本身，
// 也就是 vppapi.MPLSEnabled 读的那段 `show interface features`。它和 agent 装完
// 隧道后的核验是同一条读回路径 —— 所以这条输出就是 agent 眼中的世界。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"sdwan/controller/internal/vppapi"
)

func main() {
	socket := flag.String("socket", "/run/vpp/api.sock", "VPP 二进制 API socket")
	want := flag.String("if", "", "只看这些接口号（逗号分隔，默认全看）")
	flag.Parse()

	filter := map[uint32]bool{}
	if *want != "" {
		for _, s := range strings.Split(*want, ",") {
			var idx uint32
			if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &idx); err != nil {
				fmt.Fprintf(os.Stderr, "mpls-arc: -if %q: %v\n", s, err)
				os.Exit(1)
			}
			filter[idx] = true
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// VPP 不回帧的时候连接不会自己醒：必须带截止时间的 ctx，否则这里不是报错而是
	// 干等（第一次手测就撞上了）。
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	vc, err := vppapi.Dial(ctx, *socket, "mpls-arc")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mpls-arc: 连 VPP: %v\n", err)
		os.Exit(1)
	}
	defer vc.Close()

	ifs, err := vc.DumpInterfaces(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mpls-arc: 读接口表: %v\n", err)
		os.Exit(1)
	}
	bad, unread := 0, 0
	for _, i := range ifs {
		if len(filter) > 0 && !filter[i.SWIfIndex] {
			continue
		}
		// 弧读不到（这张口没有 mpls-input 段）不等于"没开"：CLI 把错误吞成
		// retval=0，猜错的方向正好相反。所以它单独计数，不混进"开不了 MPLS"。
		enabled, err := vc.MPLSEnabled(ctx, i.SWIfIndex)
		switch {
		case err != nil:
			unread++
			fmt.Printf("%-4d %-16s => 读不到弧: %v\n", i.SWIfIndex, i.Name, err)
		case !enabled:
			bad++
			fmt.Printf("%-4d %-16s => 开不了 MPLS（弧上还挂着 mpls-not-enabled）\n", i.SWIfIndex, i.Name)
		default:
			fmt.Printf("%-4d %-16s => MPLS 可用\n", i.SWIfIndex, i.Name)
		}
	}
	if len(filter) > 0 && bad+unread > 0 {
		// 只看被点名的接口时，"有口开不了 MPLS"是要让脚本看见的失败。
		os.Exit(1)
	}
}
