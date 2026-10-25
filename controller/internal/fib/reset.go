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

// Reset 抹掉一个 VRF 的整份 FIB 状态：表里的每条路由（含 VPP 自动生成的条目）、
// 这个 VRF 的本地标签、表本身。调用前确认这张表归 sr-fib 管。
//
// 只用于显式的 `sr-fib -reset`。正常收敛从不删表（表里可能有本包没管的路由，
// 见 Applier.Apply），所以"把本进程装过的东西清干净"必须由调用方明确要求。
//
// 删除必须走二进制 API：实测 CLI 的 `ip route del` / `mpls local-label del`
// 对 API 源安装的条目无效（retval 仍是 0，条目原样留着），只有 API 删得掉。
func Reset(ctx context.Context, dp DP, vrfs []uint8) (ResetResult, error) {
	var res ResetResult
	seen := map[uint8]bool{}
	for _, vrf := range vrfs {
		if vrf == 0 {
			return res, fmt.Errorf("fib: VRF 0 是 VPP 的默认表，不能当 VPN VRF")
		}
		if seen[vrf] {
			return res, fmt.Errorf("fib: VRF %d 配了两次", vrf)
		}
		seen[vrf] = true

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
	}
	return res, nil
}
