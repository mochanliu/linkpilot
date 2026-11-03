package fib

import (
	"context"
	"fmt"
)

// ResetResult 是 Reset 撤掉了什么。
type ResetResult struct {
	RoutesRemoved int
	LabelsRemoved int
	TablesRemoved int
}

// Reset 抹掉一个 VRF 的整份 FIB 状态：主表里的每条路由（含 VPP 自动生成的条目）、
// 这个 VRF 的本地标签、表本身，以及这个 VRF 的颜色表（§4.2，只撤真存在的那几张）。
// 调用前确认这张表归 sr-fib 管。
//
// 只用于显式的 `sr-fib -reset`。正常收敛从不删表（表里可能有本包没管的路由，
// 见 Applier.Apply），所以"把本进程装过的东西清干净"必须由调用方明确要求。
//
// 删除必须走二进制 API：实测 CLI 的 `ip route del` / `mpls local-label del`
// 对 API 源安装的条目无效（retval 仍是 0，条目原样留着），只有 API 删得掉。
func Reset(ctx context.Context, dp DP, vrfs []uint8) (ResetResult, error) {
	var res ResetResult
	// 先把整份清单验完，再动手删。这几条检查原来写在删除循环里，于是 {100, 250}
	// 会先把 VPN 100 连表带路由撤掉、再在 250 上报错：一次"拒绝"留下半删的状态，
	// 报错点还跟着清单顺序变。reset 是显式的破坏性操作，要么一条都不删，要么按清单删。
	seen := map[uint8]bool{}
	for _, vrf := range vrfs {
		if vrf == 0 {
			return res, fmt.Errorf("fib: VRF 0 是 global 表（underlay 也在这张表里），reset 会连它一起删，拒绝")
		}
		// -reset 读的是磁盘上的 cfg.VRFs，不经过 PlanFrom，所以配置页和 PlanFrom 那两道
		// 250 闸门在这里都挡不到它：漏进来就会把隧道所在的 overlay 表连表带路由删掉，
		// 顺带撤走颜色段的 1250/2250… —— 客户流量当场断。
		if vrf == OverlayTable {
			return res, fmt.Errorf("fib: VRF %d 是 overlay 保留表（隧道与对外下一跳），不是 VPN，reset 拒绝", vrf)
		}
		if seen[vrf] {
			return res, fmt.Errorf("fib: VRF %d 配了两次", vrf)
		}
		seen[vrf] = true
	}

	for _, vrf := range vrfs {
		table := uint32(vrf)
		routes, err := dp.FIBRoutes(ctx, table)
		if err != nil {
			return res, fmt.Errorf("fib: 读表 %d: %w", table, err)
		}
		for _, r := range routes {
			if err := dp.IPRouteAddDel(ctx, table, r.Prefix, r.Paths, false); err != nil {
				return res, fmt.Errorf("fib: reset 撤 %s（表 %d）: %w", r.Prefix, table, err)
			}
			res.RoutesRemoved++
		}
		if err := dp.MPLSLabelDel(ctx, LabelFor(vrf)); err != nil {
			return res, fmt.Errorf("fib: reset 撤本地标签 %d: %w", LabelFor(vrf), err)
		}
		res.LabelsRemoved++
		if err := dp.IPTableAddDel(ctx, table, false); err != nil {
			return res, fmt.Errorf("fib: reset 撤表 %d: %w", table, err)
		}
		res.TablesRemoved++

		// 颜色表（§4.2）也要撤：sr-fib 会为真被装进路由的 (色,主表) 组合建表，
		// 只清主表就把带标签的路由留在 颜色*1000+t 里，下一轮测试对不上账。
		// 从颜色 1 起循环：颜色 0 折出来就是主表本身，上面已经连表带路由撤过一遍，
		// 再撤一次会把同一张表的引用计数减两回 —— 下面那条 len==0 闸门挡不住它，
		// 因为"表还在但没人装路由"并不返回空。
		// 这里**不猜哪张表存在**，靠 dump 的空非空来判 —— 实测这两件事：
		//   - 表不存在：ip_route_dump 一帧都不回，retval 0，不报错；
		//   - 表存在但没人装路由：也**不是空的**，VPP 自动带 5 条 special
		//     （0.0.0.0/0、0.0.0.0/32、224.0.0.0/4、240.0.0.0/4、
		//     255.255.255.255/32，下一跳是 invalid、preference=0）。
		// 所以 len(routes)==0 就是"没这张表"，可靠；把"没这张表"和"真失败"混进
		// retval 反而更难查。留下的空表是惰性的：没有标签进它、policy 阶段之前
		// 没人引流，下一轮 IPTableAddDel(add) 对已存在的表也只是引用计数加一。
		for c := uint8(1); c <= MaxColor; c++ {
			ct, err := ColorTable(c, table)
			if err != nil {
				return res, fmt.Errorf("fib: reset 算颜色表（色 %d 主表 %d）: %w", c, table, err)
			}
			routes, err := dp.FIBRoutes(ctx, ct)
			if err != nil {
				return res, fmt.Errorf("fib: reset 读颜色表 %d: %w", ct, err)
			}
			if len(routes) == 0 {
				continue
			}
			for _, r := range routes {
				if err := dp.IPRouteAddDel(ctx, ct, r.Prefix, r.Paths, false); err != nil {
					return res, fmt.Errorf("fib: reset 撤 %s（颜色表 %d）: %w", r.Prefix, ct, err)
				}
				res.RoutesRemoved++
			}
			if err := dp.IPTableAddDel(ctx, ct, false); err != nil {
				return res, fmt.Errorf("fib: reset 撤颜色表 %d: %w", ct, err)
			}
			res.TablesRemoved++
		}
	}
	return res, nil
}
