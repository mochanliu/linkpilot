// sr-stunprobe -- 手工验证 STUN 链路的最小探针。
//
// 它走的是产品里同一套代码（stun.Prober：一条复用的内核 UDP socket），所以它证明的
// 不只是"reflector 观测到什么、客户端能解码什么"，还证明了设备侧那半：Local ——
// 这条 socket 真正用的本地源地址。NAT 后面 Local 与观测地址必然不同，而出口口判定
// 靠的就是 Local，所以这里把它一起打出来。
//
// 数据面不参与：探针从内核出去，不回写 VPP 任何表。
//
//	./sr-stunprobe -reflector 10.10.0.5:3478
//	./sr-stunprobe -reflector 10.10.0.5:3479 -local :51820   # 固定源端口再探一次
//	./sr-stunprobe -reflector 10.10.0.5:3478,10.10.0.5:3479  # 两个证人 ⇒ 对称判定
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"sdwan/controller/internal/stun"
)

func main() {
	var (
		reflector = flag.String("reflector", "", "reflector 地址 ip:port，逗号分隔可给两个（必填）")
		local     = flag.String("local", ":0", "本地绑定地址；:0 = 让内核选源端口")
		tries     = flag.Int("tries", 5, "每个 reflector 的探测次数")
		gap       = flag.Duration("gap", 300*time.Millisecond, "单次等待与重试间隔")
	)
	flag.Parse()

	if strings.TrimSpace(*reflector) == "" {
		log.Fatal("-reflector ip:port 必填")
	}
	refs := strings.Split(*reflector, ",")
	for i := range refs {
		refs[i] = strings.TrimSpace(refs[i])
	}

	p, err := stun.NewProber(*local)
	if err != nil {
		log.Fatalf("bind %s: %v", *local, err)
	}
	defer p.Close()

	// 一个目的地时保持原来的输出形状（harness 逐字段解析这一行）；
	// 两个及以上才多打一行判定 —— 对称与否必须在真 NAT 现场量，不能靠推。
	// 每轮是"等 gap + 睡 gap"，所以一个目的地最多 2*tries*gap。
	perRound := time.Duration(*tries) * 2 * *gap
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(len(refs))*perRound+5*time.Second)
	defer cancel()
	v, err := p.Discover(ctx, refs, *tries, *gap)
	if err != nil {
		fmt.Printf("UNAVAILABLE: %d 次探测都没在 %v 内收到匹配回包（reflector=%s）\n",
			*tries, *gap, *reflector)
		fmt.Printf("  %v\n", err)
		os.Exit(1)
	}
	for _, m := range v.Mappings {
		fmt.Printf("txid=%s local=%s reflector=%s observed=%s:%d\n",
			m.TxID, m.Local, m.Reflector, m.Addr, m.Port)
	}
	for _, f := range v.Failed {
		fmt.Printf("unanswered: %s\n", f)
	}
	switch {
	case !v.SymmetricKnown:
		fmt.Printf("verdict=unknown（只收到 %d 个观测；对称 NAT 判定至少要两个不同目的地）\n", len(v.Mappings))
	case v.Symmetric:
		fmt.Printf("verdict=symmetric（同一 socket 在不同目的地得到不同映射 ⇒ 按映射打洞的隧道建不起来）\n")
	default:
		fmt.Printf("verdict=asymmetric（映射与目的地无关 ⇒ 可以按观测地址建隧道）\n")
	}
	// 没有 NAT 时观测端口必然等于源端口；不等就是有人在改写，这条要看得见。
	if len(v.Mappings) > 0 && v.Mappings[0].Port != p.LocalPort() {
		fmt.Printf("note=本地端口 %d 与观测端口 %d 不同（前面有 NAT 改写源端口）\n",
			p.LocalPort(), v.Mappings[0].Port)
	}
}
