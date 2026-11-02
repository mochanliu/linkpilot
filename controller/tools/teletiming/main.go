// teletiming 诊断工具：分别计时 agent 遥测用到的两条 VPP 二进制 API dump。
//
// 环路实验台（docs/loop-test-report.md §4）量到的现象：四台盒子的 BFD 收包计数
// 会同时停摆 ~1.7–9.8 s → 会话判 Down → sr-fib 撤路径 → 客户流量掉窗口。
// 对照实验（同一时刻在没有 VPP、没有 agent 的 lp-rr 里也停）已经把**触发源**定在宿主；
// 写这个工具时的另一条线索——"把 sr-agent SIGSTOP 之后同一场景 0% 丢包"——
// **没有任何留档**（报告 D-16），所以它现在只是待复验的假设：盒内这 10 s 一次的 dump
// 到底在黑洞里占多大权重，要看报告 §8.2 R-5 的两遍对照。这个工具就是 R-5 的采集手段：
// 把两条 dump 拆开单独跑，用同一个口径看是哪一条（或都是）停摆。
//
//	teletiming -mode=probes -n 30      # 只跑 sr_wg_probe_dump（本插件）+ 栅栏
//	teletiming -mode=bfd -n 30         # 只跑 bfd_udp_sessions_dump（core）+ 栅栏
//	teletiming -mode=ifaces -n 30      # 只跑 sw_interface_dump（core，对照：别的 dump 是否也停摆）
//	teletiming -mode=both -i 10s       # 复刻 metricsLoop 的调用顺序
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"sdwan/controller/internal/vppapi"
)

func main() {
	sock := flag.String("sock", "/run/vpp/api.sock", "VPP 二进制 API socket")
	mode := flag.String("mode", "both", "probes|bfd|rpc|both")
	n := flag.Int("n", 10, "跑几轮")
	every := flag.Duration("i", time.Second, "两轮之间隔多久")
	flag.Parse()

	ctx := context.Background()
	c, err := vppapi.Dial(ctx, *sock, "teletiming")
	if err != nil {
		fmt.Fprintf(os.Stderr, "teletiming: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()

	for i := 0; i < *n; i++ {
		t0 := time.Now()
		var note string
		switch *mode {
		case "probes":
			p, err := c.Probes(ctx)
			note = fmt.Sprintf("probes=%d %v", len(p), err)
		case "bfd":
			s, err := c.BFDSessions(ctx)
			note = fmt.Sprintf("bfd=%d %v", len(s), err)
		case "ifaces":
			ifs, err := c.Interfaces(ctx, false)
			note = fmt.Sprintf("ifaces=%d %v", len(ifs), err)
		default:
			p, err := c.Probes(ctx)
			s, err2 := c.BFDSessions(ctx)
			note = fmt.Sprintf("probes=%d/%v bfd=%d/%v", len(p), err, len(s), err2)
		}
		fmt.Printf("%.3f %-8s %-10s %s\n", float64(time.Now().UnixNano())/1e9, *mode,
			time.Since(t0).Round(time.Millisecond), note)
		os.Stdout.Sync()
		if i+1 < *n {
			time.Sleep(*every)
		}
	}
}
