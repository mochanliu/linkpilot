// ABF：把"某个 ACL 匹配到的流"引流到"某张 FIB 表"（abf-plugin 的二进制 API）。
//
// 这就是静态策略的数据面落点。为什么是"引流到一张表"而不是"引流到一个下一跳"：
// 本系统已经把每个颜色准备成一张独立的 FIB 表（表号 = 颜色*1000 + 主表号，
// 颜色 0 折出来就是主表本身，见 internal/fib.ColorTable），而 sr-fib 每轮把
// "这个颜色的可用路径"装进那张表。策略只说"这类流属于哪个颜色"，路径的增删
// 完全由 sr-fib 按 BFD/隧道状态自己收敛 —— 策略不必随链路事件改写，
// 这就是 owner 定下的形状（2026-11-10）。
//
// 每条 path 编成 deag（dest-address-encoded graph）路径：sw_if_index=~0 +
// table_id=<目标表>。同一形状 VPP 自己也用于 `mpls ... via ip4-lookup-in-table`，
// 机箱上 `show ip fib` 把它渲染成 "dst-address,unicast lookup in ipv4-VRF:<id>"
// （2026-11-10 在 lp-cpe1 实测）。**CLI 的 `ip route/add` 不接受这个形状**
// （`via table`/`lookup-in-table` 都是 parse error），所以策略只能走二进制 API。
//
// 布局（/usr/share/vpp/api/plugins/abf.api.json，26.06）：
//
//	abf_policy_add_del   is_add(1) policy{policy_id(4) acl_index(4) n_paths(1) paths[n]}
//	abf_itf_attach_add_del is_add(1) attach{policy_id(4) sw_if_index(4) priority(4) is_ipv6(1)}
//	fib_path             167 B（与 fib.go 里那份同一个结构）
//
// 两点实测事实决定了调用方的写法（2026-11-10 在 lp-cpe1 实测，详见
// ABFPolicyAddDel 的注释）：
//   - "改一条策略"只有一条可靠路径：delete(n_paths=0) + add。同 id 带新 ACL
//     再 add 会被拒成 -7，旧策略原封不动 —— 看上去"改成功了"，实际没改；
//   - 删除一个不存在的策略返回 NO_SUCH_ENTRY(-6)，reconcile 要把它当"已经是
//     期望态"，不是错误。
package vppapi

import (
	"context"
	"encoding/binary"
	"fmt"
)

// ABFPath 是策略里的一条路径：查哪张表、第几优先。
//
// Preference 只在"同一策略里有多条路径"时才有意义。静态策略的常态是一条
// 路径（查颜色表），兜底不在这里做——兜底是颜色表内的一条默认路由，
// 由 sr-fib 装（见 internal/fib）。保留这个字段是为了以后做"颜色内多表"时
// 不用改 ABI。
type ABFPath struct {
	Table      uint32
	Preference uint8
}

// ABFPolicy 是一条策略：ACL 命中后按 Paths 依次查表。
type ABFPolicy struct {
	ID       uint32
	ACLIndex uint32
	Paths    []ABFPath
}

// ABFAttach 是"把策略挂到某个入口接口"。priority 越小越先匹配。
type ABFAttach struct {
	PolicyID  uint32
	SWIfIndex uint32
	Priority  uint32
}

// deagPath 把"查表"编成一条 FIB 路径。sw_if_index=~0 是 VPP 表达
// "这条路径不指向接口，而是换表重查"的唯一方式。
func deagPath(p ABFPath) FIBPath {
	return FIBPath{SWIfIndex: ^uint32(0), TableID: p.Table, Preference: p.Preference}
}

// ABFPolicyAddDel 建/改/删一条策略。
//
// 三种意图，VPP 对它们的回答不一样（2026-11-10 在 lp-cpe1 逐条实测）：
//
//   - 新建（id 不存在）：add + 1..255 条路径；
//   - **改（id 已存在）：必须删了重建。** 同 id 再 add 一条换了 ACL 的策略会被
//     拒成 INVALID_VALUE(-7)，而 VPP 里那条旧策略原封不动 —— 也就是说"同 id
//     覆盖"只对"同一 ACL、只改路径"成立，换 ACL 就不成立。与其记住这条半规则，
//     不如把"改"写成 delete(n_paths=0) + add：代价是这条策略上的一瞬间空窗，
//     换来的是任何改法都只有一条代码路径；
//   - 删：paths 为空即整条删除（acl_index 用 0 占位，VPP 不看它）。
func (c *Client) ABFPolicyAddDel(ctx context.Context, p ABFPolicy, add bool) error {
	if p.ID == 0 {
		return fmt.Errorf("vppapi: ABF 策略 id 不能是 0（0 在 attach 里表示'没有策略'）")
	}
	if add {
		// 这里**不能**把 ACLIndex==0 当成"没填"：VPP 给第一张 ACL 分配的索引就是 0
		// （2026-11-10 VM 台实测）。用 0 当哨兵会让"配了第一条策略"这种最常见的
		// 情况在下发时被自己拒掉。
		if len(p.Paths) == 0 || len(p.Paths) > 255 {
			return fmt.Errorf("vppapi: ABF 策略 %d 需要 1..255 条路径，给了 %d", p.ID, len(p.Paths))
		}
	}

	body := make([]byte, 0, 1+4+4+1+len(p.Paths)*fibPathLen)
	body = append(body, boolByte(add))
	body = appendU32(body, p.ID)
	body = appendU32(body, p.ACLIndex)
	body = append(body, byte(len(p.Paths)))
	for _, path := range p.Paths {
		body = appendFIBPath(body, deagPath(path))
	}
	frame, err := c.call(ctx, "abf_policy_add_del", "abf_policy_add_del_reply", body)
	if err != nil {
		return err
	}
	return c.retval("abf_policy_add_del", frame)
}

// ABFAttachAddDel 把策略挂到/摘离一个入口接口。同一接口上多策略靠 priority 排。
func (c *Client) ABFAttachAddDel(ctx context.Context, a ABFAttach, add bool) error {
	if a.PolicyID == 0 || a.SWIfIndex == 0 {
		return fmt.Errorf("vppapi: ABF attach 需要 policy_id 与 sw_if_index（收到 %d/%d）", a.PolicyID, a.SWIfIndex)
	}
	body := make([]byte, 0, 1+4+4+4+1)
	body = append(body, boolByte(add))
	body = appendU32(body, a.PolicyID)
	body = appendU32(body, a.SWIfIndex)
	body = appendU32(body, a.Priority)
	body = append(body, 0) // is_ipv6：本系统没有客户 IPv6
	frame, err := c.call(ctx, "abf_itf_attach_add_del", "abf_itf_attach_add_del_reply", body)
	if err != nil {
		return err
	}
	return c.retval("abf_itf_attach_add_del", frame)
}

// ABFPolicies 读回 VPP 里现有的全部策略。reconcile 用它判"要不要写"，
// 控制台上报也用它——不猜、不缓存。
func (c *Client) ABFPolicies(ctx context.Context) ([]ABFPolicy, error) {
	frames, err := c.dump(ctx, "abf_policy_dump", "abf_policy_details", nil)
	if err != nil {
		return nil, err
	}
	out := make([]ABFPolicy, 0, len(frames))
	for _, f := range frames {
		p, err := parseABFPolicyDetails(f)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// parseABFPolicyDetails 解一帧 abf_policy_details：header 6 + policy_id 4 +
// acl_index 4 + n_paths 1 + paths[n]。
func parseABFPolicyDetails(f []byte) (ABFPolicy, error) {
	if len(f) < 15 {
		return ABFPolicy{}, fmt.Errorf("vppapi: abf_policy_details 太短（%d 字节）", len(f))
	}
	p := ABFPolicy{
		ID:       binary.BigEndian.Uint32(f[6:10]),
		ACLIndex: binary.BigEndian.Uint32(f[10:14]),
	}
	n := int(f[14])
	if len(f) < 15+n*fibPathLen {
		return ABFPolicy{}, fmt.Errorf("vppapi: abf_policy_details 声称 %d 条路径但只有 %d 字节", n, len(f))
	}
	for i := 0; i < n; i++ {
		off := 15 + i*fibPathLen
		// fib_path：sw_if_index(4) table_id(4) rpf_id(4) weight(1) preference(1) …
		p.Paths = append(p.Paths, ABFPath{
			Table:      binary.BigEndian.Uint32(f[off+4 : off+8]),
			Preference: f[off+13],
		})
	}
	return p, nil
}

// ABFAttaches 读回全部挂点。
func (c *Client) ABFAttaches(ctx context.Context) ([]ABFAttach, error) {
	frames, err := c.dump(ctx, "abf_itf_attach_dump", "abf_itf_attach_details", nil)
	if err != nil {
		return nil, err
	}
	out := make([]ABFAttach, 0, len(frames))
	for _, f := range frames {
		a, err := parseABFAttachDetails(f)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// parseABFAttachDetails 解一帧 abf_itf_attach_details：header 6 + policy_id 4 +
// sw_if_index 4 + priority 4 + is_ipv6 1。
func parseABFAttachDetails(f []byte) (ABFAttach, error) {
	if len(f) < 6+13 {
		return ABFAttach{}, fmt.Errorf("vppapi: abf_itf_attach_details 太短（%d 字节）", len(f))
	}
	return ABFAttach{
		PolicyID:  binary.BigEndian.Uint32(f[6:10]),
		SWIfIndex: binary.BigEndian.Uint32(f[10:14]),
		Priority:  binary.BigEndian.Uint32(f[14:18]),
	}, nil
}
