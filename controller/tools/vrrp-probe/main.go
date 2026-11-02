// vrrp-probe 量 VPP 的 vrrp 插件"配下去之后到底是什么行为"上的未知数。
//
// 为什么需要它：vrrp 在 lab 这一版 VPP（26.06-release）上只有二进制 API，CLI 全是
// unknown input（2026-11-02 在 lab-cpe1 实测），所以 internal/apply/vrrp.go 那段调和
// 逻辑不能靠猜。这里逐条量出来的答案会写进那个文件的注释；改调和策略之前先重跑。
//
// -iface 必须是实验口。加一个 VR 就是往那张口的特性弧上挂东西，lab-cpe1 上别的口都在
// 跑客户流量，不能拿别人的业务当实验品。
//
// 用法（容器里，先把实验口准备好）：
//
//	vppctl create tap id 99
//	vppctl set interface state tap99 up
//	vppctl set interface ip address tap99 192.0.2.1/24
//	go run ./tools/vrrp-probe -iface tap99 -vip 192.0.2.250 -vip-outside 198.51.100.7
//	go run ./tools/vrrp-probe -dump           # 只读全表，一条配置不动
//	go run ./tools/vrrp-probe -live -iface tap99 -priority 100 -interval 1000
//	                                          # 建一条跑着的 VR 然后退出，留给数据面实验
//	go run ./tools/vrrp-probe -clean -iface tap99
//	                                          # 把这张口上的 VR 删干净
//
// tap99 的内核一端在同一容器的 netns 里叫 tap0（实测：给它 192.0.2.2/24 后 ping VPP 侧
// 的 192.0.2.1 通，邻居表里是 VPP 的 MAC）。数据面实验就在容器里对 tap0 发包。
//
// 三轮量下来的答案（2026-11-02，lab-cpe1，VPP 26.06-release，tap99=#7：admin/link up、
// 192.0.2.1/24、MTU 9000）。retval 的名字这一版没装 vnet/error.h，是照 FDio/govpp 里
// 抄自同一份表的清单核对的：-6 NO_SUCH_ENTRY / -7 INVALID_VALUE / -73 INVALID_ARGUMENT /
// -116 ENTRY_ALREADY_EXISTS。逐条编号对应下面代码里的 C。
//
//   - C1 interval=0 -> -7 INVALID_VALUE。add 和 del 两条路都验字段，所以第 6 段每一行
//     都必须带一个真 interval，控制器不给"缺省 0"的余地。
//   - C2 同键第二次 add -> -116 ENTRY_ALREADY_EXISTS，原 VR 一字未改 -> add 不是幂等覆盖。
//   - C3/C4/C12/C13 vrrp_index 是**全局 VR 池的下标**：不存在的下标 -> -6；下标上有 VR
//     但键(口, vr_id)不是它 -> -73；删掉之后同一个下标会被下一条新 VR 复用（第三轮
//     index=0 在 loop0 那条被删后分给了 tap99 新建的那条，update 直接改动了它）。
//     update 只认这个下标，绝不按 (口, vr_id) 找 -> 调和读不到它（dump 不带 index），
//     所以改参数只能删了重建，见 internal/apply/vrrp.go。
//   - C5 网段外的 VIP -> ok 收下 -> "VIP 必须落在口的网段里"是我们控制器的策略，不是 VPP 的硬限制。
//   - C6 up 口上 start -> 立刻 BACKUP，等到 master_down_int 才变 MASTER（100cs 间隔实测 4s；
//     200cs 间隔 6.98s，5s 时还在 BACKUP，对得上）。定时器是 VPP 自己算的，三个样本都符合
//     skew=(256-prio)*interval/256、master_down=3*interval+skew。
//     VIP 始终不进接口地址表：start 后 ip_address_dump(#7) 只有 192.0.2.1/24。
//   - C7 dump(0) 0 条 / dump(#7) 1 条 / dump(~0) 全部 -> ~0 在 vrrp 这条上是好的，
//     和 ip_address_dump 不一样（那条实测坏，2026-10-28）。调和可以一次 dump 拿全表。
//   - C8/C14 口 down -> INTF_DOWN；口 up -> 回 BACKUP，但**会不会自己回 MASTER 不确定**：
//     三个样本里一个 ~4s 自己回了，两个 8~9s 还卡在 BACKUP；stop+start 之后每次都稳定地
//     在 master_down 时间内回 MASTER。口的抖动不归本调和器管（它不盯着事件），这条留给
//     双机实验（#204）看它在有对端时意味着什么。
//   - C9 对已经跑着的 VR 重复 start -> ok，state 不变 -> 幂等，调和可以无脑补 start。
//   - C10 del by key 字段照 dump 回送 -> ok，真删掉（第一轮"只给键+interval=0"被 -7 挡）。
//   - C11 跑着（MASTER/BACKUP）直接 del by key、不先 stop -> ok。
//   - C12 del by index -> 只有在本进程刚拿到那个下标时可用；-6/-73 都会踩。
//
// -live 那几条不是 API 行为，是把 VR 留在设备上、从内核一端发包量出来的数据面事实
// （2026-11-02，tap99/vrp1，两张口的 VPP 侧都自己有 /24 地址）：
//
//   - VIP 会回 ARP，而且回的是虚拟 MAC（00:00:5e:00:01:63），BACKUP 态就回，
//     accept 位开不开都回；VIP 始终不进 ip_address_dump。前提是这个口的 VPP 侧自己有
//     一个同网段地址（vrp1 只给内核端配了地址时，VIP 的 ARP 都不回）。
//   - VIP 的 ICMP 不回：BACKUP 态、accept=true/false 四种组合各 ping 3 个包，全是
//     0 received。"到 VIP 自己的包终不终止"留给双机实验（#204）在真 MASTER 上量——
//     我们的 HA 里客户是把 VIP 当下一跳用的，帧的 dst MAC 是虚拟 MAC、目的 IP 不是 VIP。
//   - **纯 add+start 常常选不上 MASTER**：8 个干净样本里只有 2 个在 4 秒内变 MASTER，
//     其余 8 秒还在 BACKUP（master_down 只有 3.45~3.6 秒）。而 stop+start、或一次链路
//     down/up 事件之后，3 个样本每次都稳定在 master_down 时间内变 MASTER。
//     这条直接决定 #204 要量什么：双机时两台会不会一起卡在 BACKUP。
package main

import (
	"context"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sdwan/controller/internal/vppapi"
)

const (
	probeVRID = 99 // 主实验 VR
	spareVRID = 98 // 只用来量"网段外的 VIP 收不收"
)

func main() {
	socket := flag.String("socket", "/run/vpp/api.sock", "VPP 二进制 API socket")
	ifaceName := flag.String("iface", "", "实验口名字（必填，见文件头）")
	vip := flag.String("vip", "192.0.2.250", "实验用虚拟地址，要落在 -iface 自己的网段里")
	vipOutside := flag.String("vip-outside", "", "和 -iface 网段不同的地址；空=跳过 C5")
	keep := flag.Bool("keep", false, "跑完不删 VR（排查用；默认删干净）")
	dumpOnly := flag.Bool("dump", false, "只把全 VPP 的 VR 表打出来，一条配置都不动")
	live := flag.Bool("live", false, "只在这张口上建一条 VR 并 start，等它选上 MASTER 就退出，不删（数据面实验用）")
	prio := flag.Int("priority", 100, "-live 用的优先级")
	intervalMS := flag.Int("interval", 1000, "-live 用的通告间隔（毫秒，必须是 10 的整数倍）")
	preempt := flag.Bool("preempt", true, "-live 建的 VR 是否抢占")
	accept := flag.Bool("accept", false, "-live 建的 VR 是否带 accept 位（VIP 的包收不收）")
	clean := flag.Bool("clean", false, "只把 -iface 上的 VR 全删掉然后退出")
	flag.Parse()

	if *dumpOnly {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		dumpAll(ctx, *socket)
		return
	}

	if *ifaceName == "" {
		fmt.Fprintln(os.Stderr, "vrrp-probe: -iface 必填（不许拿客户的口做实验，也不许临时建口：这一版 VPP 的 loopback/tap 删除 CLI 全是 unknown input，建了就还不回去）")
		os.Exit(1)
	}
	addr, err := netip.ParseAddr(*vip)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vrrp-probe: -vip %q: %v\n", *vip, err)
		os.Exit(1)
	}
	var outside netip.Addr
	if *vipOutside != "" {
		outside, err = netip.ParseAddr(*vipOutside)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vrrp-probe: -vip-outside %q: %v\n", *vipOutside, err)
			os.Exit(1)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	vc, err := vppapi.Dial(ctx, *socket, "vrrp-probe")
	if err != nil {
		fmt.Fprintf(os.Stderr, "vrrp-probe: 连 VPP: %v\n", err)
		os.Exit(1)
	}
	defer vc.Close()

	swIfIndex, ok := func() (uint32, bool) {
		ifs, err := vc.DumpInterfaces(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vrrp-probe: 读接口表: %v\n", err)
			os.Exit(1)
		}
		for _, i := range ifs {
			if i.Name == *ifaceName {
				fmt.Printf("[实验口] %s = #%d admin_up=%v link_up=%v mtu=%d 地址=%v\n",
					i.Name, i.SWIfIndex, i.AdminUp, i.LinkUp, i.L3MTU, i.IPv4)
				return i.SWIfIndex, true
			}
		}
		return 0, false
	}()
	if !ok {
		fmt.Fprintf(os.Stderr, "vrrp-probe: 接口表里没有 %s\n", *ifaceName)
		os.Exit(1)
	}

	// 开场先把这张口上残留的 VR 删干净：上一轮跑挂了留着东西的话，每条 C 的起始状态
	// 就不确定，整份报告作废。删的时候把它自己报的字段原样回送（这正是 C10 量的写法）。
	if n := cleanIface(vc, ctx, swIfIndex); *clean {
		fmt.Printf("[clean] %s=#%d 上删掉了 %d 条 VR\n", *ifaceName, swIfIndex, n)
		return
	}

	base := vppapi.VRRPParams{SWIfIndex: swIfIndex, VRID: probeVRID, Addrs: []netip.Addr{addr}}
	with := func(p vppapi.VRRPParams, prio uint8, intervalCS uint16) vppapi.VRRPParams {
		p.Priority, p.IntervalCS = prio, intervalCS
		return p
	}

	// -live：建一条、start、等它选上 MASTER，然后退出不删。数据面实验（VIP 到底响不响
	// ARP、ICMP 通不通）要的是一个"正在跑的 VR"，跑完整个 C1..C14 矩阵反而把它带走了。
	if *live {
		if *intervalMS%10 != 0 || *intervalMS < 10 {
			fmt.Fprintf(os.Stderr, "vrrp-probe: -interval %d 毫秒不是 10 的整数倍（VPP 的间隔单位是厘秒）\n", *intervalMS)
			os.Exit(1)
		}
		p := with(base, uint8(*prio), uint16(*intervalMS/10))
		p.Preempt, p.Accept = *preempt, *accept
		if err := vc.VRRPAdd(ctx, p); err != nil {
			fmt.Fprintf(os.Stderr, "vrrp-probe: -live add: %v\n", err)
			os.Exit(1)
		}
		if err := vc.VRRPStartStop(ctx, swIfIndex, probeVRID, false, true); err != nil {
			fmt.Fprintf(os.Stderr, "vrrp-probe: -live start: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("[live] 已 add+start：%s=#%d vr%d prio=%d interval=%dms preempt=%v accept=%v vip=%s\n",
			*ifaceName, swIfIndex, probeVRID, *prio, *intervalMS, *preempt, *accept, addr)
		settle(vc, ctx, swIfIndex, "live")
		dumpAll(ctx, *socket)
		fmt.Printf("[live] 这条 VR 留在设备上，清理办法：再跑一次不带 -live 的 probe（开场就把残留删干净）\n")
		return
	}

	// C1 interval=0 在 up 口上还是不是被拒（决定第 6 段能不能带"缺省 interval"下去）。
	p := with(base, 100, 0)
	fmt.Printf("\n[C1 add interval=0] %v\n", label(vc.VRRPAdd(ctx, p)))
	dump(vc, ctx, swIfIndex, "C1 之后")

	// C2 同键第二次 add：拒绝，还是原地改掉？决定调和时"改参数"要不要先删。
	fmt.Printf("[C2 首次 add prio=100 interval=100cs] %v\n",
		label(vc.VRRPAdd(ctx, with(base, 100, 100))))
	dump(vc, ctx, swIfIndex, "C2 首次之后")
	fmt.Printf("[C2 同键再 add prio=150 interval=300cs] %v\n",
		label(vc.VRRPAdd(ctx, with(base, 150, 300))))
	dump(vc, ctx, swIfIndex, "C2 重复之后")

	// C3 update 传一个不存在的 vrrp_index、但 (口, vr_id) 是对的：
	// 成功=VPP 按键找，调和可以原地改；失败=必须先拿到真 index（只有 update 的回复给）。
	got, err := vc.VRRPUpdate(ctx, 9999, with(base, 120, 250))
	fmt.Printf("[C3 update index=9999 键正确] 回 index=%d %v\n", got, label(err))
	dump(vc, ctx, swIfIndex, "C3 之后")

	// C4 用 C3 回的那个 index 再改一次：量回复里的 index 到底能不能拿回去用。
	p4 := with(base, 130, 200)
	idx := got
	if idx == 9999 {
		idx = 0 // C3 失败时它回了 9999，换个值再量一次，别把同一个数当成答案
	}
	got4, err := vc.VRRPUpdate(ctx, idx, p4)
	fmt.Printf("[C4 update index=%d prio=130] 回 index=%d %v\n", idx, got4, label(err))
	dump(vc, ctx, swIfIndex, "C4 之后")

	// C5 网段外的 VIP：VPP 收不收？收，就说明"VIP 必须落在口的网段里"是我们控制器的
	// 策略（因为邻居/ARP 实际不工作），不是 VPP 的硬限制。措辞要照这个来。
	if outside.IsValid() {
		s := vppapi.VRRPParams{SWIfIndex: swIfIndex, VRID: spareVRID, Priority: 100,
			IntervalCS: 100, Addrs: []netip.Addr{outside}}
		fmt.Printf("[C5 add vr%d 网段外 VIP %s] %v\n", spareVRID, outside, label(vc.VRRPAdd(ctx, s)))
		dump(vc, ctx, swIfIndex, "C5 之后")
		fmt.Printf("[C5 收尾 删 vr%d] %v\n", spareVRID, label(vc.VRRPDelByKey(ctx, s)))
	}

	// C6 在 up 口上 start：state、mac、三个定时器只有在真跑起来才有值；顺带复核
	// "VIP 进不进接口地址表"（第一轮在 down 口上量的，那条结论要看口状态）。
	fmt.Printf("[C6 start] %v\n", label(vc.VRRPStartStop(ctx, swIfIndex, probeVRID, false, true)))
	dump(vc, ctx, swIfIndex, "C6 start 立刻")
	for i := 1; i <= 5; i++ {
		time.Sleep(time.Second)
		dump(vc, ctx, swIfIndex, fmt.Sprintf("C6 +%ds", i))
	}
	ips, err := vc.DumpIPv4Addresses(ctx, swIfIndex)
	fmt.Printf("[C6 start 后接口地址表] %v addrs=%v\n", label(err), ips)

	// C7 dump 的三种写法：0 / 本口 / ~0。ip_address_dump 的 ~0 在这一版是坏的
	// （2026-10-28 实测），vrrp 的要单独量：调和能不能一次 dump 拿全部。
	for _, q := range []struct {
		label string
		idx   uint32
	}{{"dump(0)", 0}, {fmt.Sprintf("dump(#%d)", swIfIndex), swIfIndex}, {"dump(~0)", ^uint32(0)}} {
		rs, err := vc.VRRPDump(ctx, q.idx)
		line := ""
		for _, v := range rs {
			line += fmt.Sprintf(" [#%d vr%d state=%s]", v.SWIfIndex, v.VRID, v.State)
		}
		fmt.Printf("[C7 %s] -> %d 条 %v%s\n", q.label, len(rs), label(err), line)
	}

	// C8 链路抖动：down 之后 state 是什么，up 之后会不会自己回 MASTER（preempt 是开着的）。
	// 调和路径关心的是"口的抖动要不要重新 start"，这条量完才有答案。
	fmt.Printf("[C8 set interface state %s down] %v\n", *ifaceName,
		oneLine(cli(vc, ctx, fmt.Sprintf("set interface state %s down", *ifaceName))))
	time.Sleep(1500 * time.Millisecond)
	dump(vc, ctx, swIfIndex, "C8 口 down 后")
	fmt.Printf("[C8 set interface state %s up] %v\n", *ifaceName,
		oneLine(cli(vc, ctx, fmt.Sprintf("set interface state %s up", *ifaceName))))
	for _, d := range []int{1, 3, 5} {
		time.Sleep(time.Duration(d) * time.Second)
		dump(vc, ctx, swIfIndex, fmt.Sprintf("C8 口 up 后 ~%ds", d))
	}

	// C9 start 一个已经跑着的 VR / stop 一个已经停着的：幂等还是报错？决定调和要不要
	// 自己判重（把 INIT->start 和 MASTER->start 走同一条代码）。
	fmt.Printf("[C9 重复 start] %v\n", label(vc.VRRPStartStop(ctx, swIfIndex, probeVRID, false, true)))
	dump(vc, ctx, swIfIndex, "C9 之后")

	// C10 删除走 is_add=0（键），字段照 dump 回送：第一轮里"只给键+interval=0"被 -7 挡了，
	// 这条量"字段给全"能不能删掉——调和的删除原语到底可用不可用。
	cur := dumpOne(vc, ctx, swIfIndex, probeVRID)
	if cur != nil {
		fmt.Printf("[C10 del by key 字段回送] %v\n", label(vc.VRRPDelByKey(ctx, paramsOf(*cur))))
		dump(vc, ctx, swIfIndex, "C10 之后")
	} else {
		fmt.Println("[C10] 跳过：这张口上没有 vr99 了（前面哪条把它删了，看上面的 dump）")
	}

	// C11 跑着的时候删（不先 stop）：调和删一行就是这条路径。
	fmt.Printf("[C11 重新 add + start] add=%v start=%v\n",
		label(vc.VRRPAdd(ctx, with(base, 100, 100))),
		label(vc.VRRPStartStop(ctx, swIfIndex, probeVRID, false, true)))
	dump(vc, ctx, swIfIndex, "C11 跑起来")
	fmt.Printf("[C11 不 stop 直接 del by key] %v\n",
		label(vc.VRRPDelByKey(ctx, with(base, 100, 100))))
	dump(vc, ctx, swIfIndex, "C11 之后")

	// C12 vrrp_vr_del（要 vrrp_index）：只有先 update 才拿得到 index，所以它只能当备选。
	// 这条量它到底认哪个数：C3/C4 回复里的 index 是真 index，还是 VPP 把键回显出来了。
	got12, err := vc.VRRPUpdate(ctx, 0, with(base, 110, 100))
	fmt.Printf("[C12 update 取 index] 回 index=%d %v\n", got12, label(err))
	fmt.Printf("[C12 del by index=%d] %v\n", got12, label(vc.VRRPDelByIndex(ctx, got12)))
	dump(vc, ctx, swIfIndex, "C12 之后")

	// C13 vrrp_index 会不会被复用：C12 删掉的 index=0 是第一轮留在 loop0 上的那条 VR。
	// 现在本口新建一条，再拿 index=0 去 update：改成别的口=0 号还被占着；改成本口=0 号
	// 重新分给了新 VR。调和绝不能把 index 当身份缓存——它只在"刚发过 update"这一瞬间可信。
	fmt.Printf("\n[C13 重新 add prio=100 interval=100cs] %v\n", label(vc.VRRPAdd(ctx, with(base, 100, 100))))
	got13, err := vc.VRRPUpdate(ctx, 0, with(base, 140, 100))
	fmt.Printf("[C13 update index=0 键=本口] 回 index=%d %v\n", got13, label(err))
	dump(vc, ctx, swIfIndex, "C13 本口")
	dumpAll(ctx, *socket)

	// C14 链路抖了以后怎么回 MASTER。调和路径将来碰到口抖动时该走哪条，只能量：
	// (a) 口 up 之后自己会不会回 MASTER；(b) stop+start 救不救得回来；
	// (c) 都不行就删了重建（del+add+start）。每一步都读回 state。
	fmt.Printf("\n[C14 start 并等选举] start=%v\n", label(vc.VRRPStartStop(ctx, swIfIndex, probeVRID, false, true)))
	settle(vc, ctx, swIfIndex, "C14 起步")
	flap(vc, ctx, *ifaceName, "down")
	time.Sleep(1500 * time.Millisecond)
	dump(vc, ctx, swIfIndex, "C14 口 down")
	flap(vc, ctx, *ifaceName, "up")
	settle(vc, ctx, swIfIndex, "C14 口 up")
	fmt.Printf("[C14 (b) stop+start] stop=%v start=%v\n",
		label(vc.VRRPStartStop(ctx, swIfIndex, probeVRID, false, false)),
		label(vc.VRRPStartStop(ctx, swIfIndex, probeVRID, false, true)))
	settle(vc, ctx, swIfIndex, "C14 stop+start 后")
	cur14 := dumpOne(vc, ctx, swIfIndex, probeVRID)
	if cur14 != nil {
		fmt.Printf("[C14 (c) 删了重建] del=%v add=%v start=%v\n",
			label(vc.VRRPDelByKey(ctx, paramsOf(*cur14))),
			label(vc.VRRPAdd(ctx, with(base, 100, 100))),
			label(vc.VRRPStartStop(ctx, swIfIndex, probeVRID, false, true)))
		settle(vc, ctx, swIfIndex, "C14 重建后")
	}

	final, err := vc.VRRPDump(ctx, swIfIndex)
	if *keep {
		fmt.Printf("\n[收尾] -keep：#%d 上的 %d 条 VR 都留着\n", swIfIndex, len(final))
		return
	}
	for _, v := range final {
		fmt.Printf("[收尾] 删 vr%d: %v\n", v.VRID, label(vc.VRRPDelByKey(ctx, paramsOf(v))))
	}
	rest, err := vc.VRRPDump(ctx, swIfIndex)
	fmt.Printf("\n[收尾] %s=#%d 上还剩 %d 条 VR (err=%v)\n", *ifaceName, swIfIndex, len(rest), label(err))
}

// cleanIface 删掉这张口上所有 VR，用的是 C10 量过的写法（字段照 dump 回送）。
// 返回删了几条；读不到就 os.Exit——起始状态不确定时整轮结果都没意义。
func cleanIface(vc *vppapi.Client, ctx context.Context, swIfIndex uint32) int {
	left, err := vc.VRRPDump(ctx, swIfIndex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vrrp-probe: 读残留 VR: %v\n", err)
		os.Exit(1)
	}
	for _, v := range left {
		err := vc.VRRPDelByKey(ctx, paramsOf(v))
		fmt.Printf("[清残留] 删 #%d vr%d: err=%v\n", v.SWIfIndex, v.VRID, label(err))
	}
	return len(left)
}

// paramsOf 把 dump 读回来的一条 VR 还原成发送用的参数——"照 VPP 自己报的字段回送"，
// 调和删一行时就是这个写法。
func paramsOf(v vppapi.VRRPVR) vppapi.VRRPParams {
	return vppapi.VRRPParams{
		SWIfIndex:  v.SWIfIndex,
		VRID:       v.VRID,
		Priority:   v.Priority,
		IntervalCS: v.IntervalCS,
		Preempt:    v.Preempt,
		Accept:     v.Accept,
		Unicast:    v.Unicast,
		Addrs:      v.Addrs,
	}
}

func dumpOne(vc *vppapi.Client, ctx context.Context, swIfIndex uint32, vrID uint8) *vppapi.VRRPVR {
	rs, err := vc.VRRPDump(ctx, swIfIndex)
	if err != nil {
		fmt.Printf("    dump err=%v\n", err)
		return nil
	}
	for i, v := range rs {
		if v.VRID == vrID {
			return &rs[i]
		}
	}
	return nil
}

// dump 打这张口上所有 VR 的全字段。
func dump(vc *vppapi.Client, ctx context.Context, swIfIndex uint32, what string) {
	rs, err := vc.VRRPDump(ctx, swIfIndex)
	if err != nil {
		fmt.Printf("    %s: dump err=%v\n", what, err)
		return
	}
	if len(rs) == 0 {
		fmt.Printf("    %s: 空\n", what)
		return
	}
	for _, v := range rs {
		fmt.Printf("    %s: %s\n", what, vrLine(v))
	}
}

// settle 每 2 秒读一次、最多 8 秒，直到某条 VR 变成 MASTER。一次选举本来就要等
// Master_Down 定时器（interval=100cs 时实测 4 秒），一次都不等就下结论会把它读成"没恢复"。
func settle(vc *vppapi.Client, ctx context.Context, swIfIndex uint32, what string) {
	for i := 1; i <= 4; i++ {
		time.Sleep(2 * time.Second)
		rs, err := vc.VRRPDump(ctx, swIfIndex)
		if err != nil {
			fmt.Printf("    %s +%ds: dump err=%v\n", what, 2*i, err)
			return
		}
		master := false
		for _, v := range rs {
			fmt.Printf("    %s +%ds: %s\n", what, 2*i, vrLine(v))
			if v.State == vppapi.VRRPStateMaster {
				master = true
			}
		}
		if len(rs) == 0 {
			fmt.Printf("    %s +%ds: 空\n", what, 2*i)
		}
		if master {
			return
		}
	}
}

func flap(vc *vppapi.Client, ctx context.Context, name, state string) {
	out, err := vc.CLI(ctx, fmt.Sprintf("set interface state %s %s", name, state))
	fmt.Printf("[C14 set interface state %s %s] %v %s\n", name, state, label(err), oneLine(out))
}

// vrLine 是一条 VR 的全字段。抓表要的是"读回的和发出去的对不对得上"，所以一个字段都不省。
func vrLine(v vppapi.VRRPVR) string {
	return fmt.Sprintf("#%d vr%d prio=%d interval=%dcs state=%s preempt=%v accept=%v unicast=%v master_adv=%dcs skew=%dcs down=%dcs mac=%s 跟踪掉%d prio=%d addrs=%v",
		v.SWIfIndex, v.VRID, v.Priority, v.IntervalCS, v.State,
		v.Preempt, v.Accept, v.Unicast, v.MasterAdvCS, v.SkewCS, v.MasterDownCS,
		v.MasterMAC, v.TrackIfDown, v.TrackPriority, v.Addrs)
}

// dumpAll 是 -dump：只读一遍全 VPP 的 VR 表，接口名一并打出来。抓表和排查用它，
// 它不动任何配置。
func dumpAll(ctx context.Context, socket string) {
	vc, err := vppapi.Dial(ctx, socket, "vrrp-probe")
	if err != nil {
		fmt.Fprintf(os.Stderr, "vrrp-probe: 连 VPP: %v\n", err)
		os.Exit(1)
	}
	defer vc.Close()
	names := map[uint32]string{}
	ifs, err := vc.DumpInterfaces(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vrrp-probe: 读接口表: %v\n", err)
		os.Exit(1)
	}
	for _, i := range ifs {
		names[i.SWIfIndex] = i.Name
	}
	rs, err := vc.VRRPDump(ctx, ^uint32(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "vrrp-probe: dump VR: %v\n", err)
		os.Exit(1)
	}
	if len(rs) == 0 {
		fmt.Println("[dump] VPP 里没有 VR")
		return
	}
	for _, v := range rs {
		n := names[v.SWIfIndex]
		if n == "" {
			n = "?"
		}
		fmt.Printf("  %s %s\n", n, vrLine(v))
	}
}

func cli(vc *vppapi.Client, ctx context.Context, cmd string) string {
	out, err := vc.CLI(ctx, cmd)
	if err != nil {
		return fmt.Sprintf("err=%v out=%s", err, out)
	}
	return out
}

// label 把 nil 打成 "ok"：报告里"err=<nil>"和"err=ok"读起来一样，但 ok 更不容易看漏。
func label(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

func oneLine(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' {
			r = ' '
		}
		out = append(out, r)
	}
	return string(out)
}
