// The 7th artifact: static SD-WAN policy (VPP ABF + ACL).
//
// 一条策略行 = "这类流属于哪个颜色"。落地方式不是"引流到某个下一跳"，而是
// "引流到那张颜色的 FIB 表"：颜色表由 sr-fib 按 BFD/隧道状态自己收敛路径，
// 所以链路抖动不需要重写策略，策略也不必知道隧道现在通不通（owner 2026-11-10）。
//
// 和 VRRP（第 6 段）同一个形状：这不是一个盒子要保存的文件，而是 VPP 内存里的
// 状态，所以这一段是**对账指令** —— 盒子读 VPP 自己的 dump，把差异补齐。三个
// 对象必须一起对账，因为它们在 VPP 里互相引用：
//
//	ACL（匹配五元组） ← acl_index ← ABF 策略（查哪张表） ← policy_id ← 挂点（入口口）
//
// 三条实测事实决定了这里的顺序和写法（lp-cpe1 / VPP 26.06，2026-11-10）：
//
//  1. **acl_index 选不了**。`acl_add_replace` 带一个不存在的 index 会答
//     NO_SUCH_ENTRY(-6)，index 只能由 VPP 分配。所以策略行的身份是 ACL 的
//     **tag**（"sr-pol-<id>"）：先 dump 出 tag→index，已存在就原地替换，
//     不存在才新建。tag 是这段代码里唯一稳定的键。
//  2. **换 ACL 的"改策略"必须删了重建**。同 id 带新 acl 再 add 被拒成
//     INVALID_VALUE(-7)，旧策略原封不动 —— 看上去成功，实际没改。
//  3. **删一条还挂着的策略会让 VPP 进程直接死**（实测：删除后 api 连接 broken
//     pipe，`vppctl` 全部 connection refused）。所以顺序是**先摘挂点，再删策略**，
//     这条不是风格问题，是能不能把盒子弄挂的问题。
//
// 因此整轮对账的顺序固定为：摘多余/变更的挂点 → 删多余/变更的策略 → 建/改 ACL
// → 补出目标颜色表 → 建/改策略 → 补挂点 → 复检。中途失败就是一个失败的 apply：
// 错误里点名是哪一行，上一版的策略仍在（VPP 不会因为一次半途而废的对账变成半套
// 策略，除非删了没建回来，那种情况下一轮会继续补）。
//
// 兜底（"允许 fallback 到颜色 0"）不在这里：它是颜色表里的一条默认路由，归
// sr-fib 装（见 internal/fib.PlanFrom 的 fallbacks 参数）。这一段只把这份清单
// 原样记在 policy.json 里，供 sr-agent 派生给 sr-fib —— 同一个开关只有一个来源
// （控制台那一页），两个执行者各取自己那一半。
package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"sdwan/controller/internal/fib"
	"sdwan/controller/internal/vppapi"
)

const (
	// policyTagPrefix + <row id> 是 ACL 的身份。tag 定长 64 字节（VPP 的
	// acl_details 里是 char[64]），这里给 id 留到 999 也只用 11 个字符。
	policyTagPrefix = "sr-pol-"
	// policyABFBase + <row id> 是 ABF 的 policy_id。控制器只发 1..999 的行号，
	// 盒子自己折算，契约里少一个"两边都得算对"的字段。
	policyABFBase = 1000
	// policyAttachBase + <row id> 是挂点优先级（小的先匹配）。行号即顺序，
	// 页面上的顺序就是匹配顺序。
	policyAttachBase = 1000
	// policyMaxRowID 与控制器侧的约束一致（行号进 tag 与 policy_id）。
	policyMaxRowID = 999
)

// policySegmentDoc 是第 7 段的形状。Rows 必填（可以为空数组：策略清空也是一种
// 指令）；Fallback 是"每 VRF 每颜色"的兜底开关，只带勾上的那些。
type policySegmentDoc struct {
	Rows     *[]policyRowDoc  `json:"rows"`
	Fallback []policyFBRowDoc `json:"fallback"`
}

type policyRowDoc struct {
	ID      int      `json:"id"`
	VPN     uint32   `json:"vpn"`
	Color   uint8    `json:"color"`
	Src     string   `json:"src,omitempty"`
	Dst     string   `json:"dst,omitempty"`
	Proto   uint8    `json:"proto,omitempty"`
	SrcPort string   `json:"src_port,omitempty"`
	DstPort string   `json:"dst_port,omitempty"`
	Attach  []string `json:"attach"`
}

type policyFBRowDoc struct {
	VPN   uint32 `json:"vpn"`
	Color uint8  `json:"color"`
}

// Fallback 是兜底开关的一行（VPN + 颜色）。sr-agent 把它派生给 sr-fib，
// 后者在颜色表里装一条 0.0.0.0/0 → 主表的默认路由。
type Fallback struct {
	VPN   uint8
	Color uint8
}

// PolicyFallbacks 读第 7 段里的兜底清单。空段（老配置包）返回空表，不报错。
//
// 这里**只认 fallback 键**：Rows 的合法性在 applyPolicy 里判，而 sr-fib 只需要
// 兜底那一半。段损坏（不是 JSON）仍然报错 —— 静默返回空会让兜底悄悄消失。
func PolicyFallbacks(seg string) ([]Fallback, error) {
	if strings.TrimSpace(seg) == "" {
		return nil, nil
	}
	var doc policySegmentDoc
	if err := json.Unmarshal([]byte(seg), &doc); err != nil {
		return nil, fmt.Errorf("配置包第 7 段不是 JSON 对象: %w", err)
	}
	out := make([]Fallback, 0, len(doc.Fallback))
	for _, f := range doc.Fallback {
		if f.Color == 0 || f.Color > fib.MaxColor {
			return nil, fmt.Errorf("兜底：颜色 %d 不在这套系统的颜色范围 1..%d 里（颜色 0 的落点就是主表，没有兜底）", f.Color, fib.MaxColor)
		}
		if f.VPN > uint32(fib.MaxVRFID) {
			return nil, fmt.Errorf("兜底：VPN %d 超出 0..%d", f.VPN, fib.MaxVRFID)
		}
		out = append(out, Fallback{VPN: uint8(f.VPN), Color: f.Color})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].VPN != out[j].VPN {
			return out[i].VPN < out[j].VPN
		}
		return out[i].Color < out[j].Color
	})
	return out, nil
}

func decodePolicySegment(seg string) (policySegmentDoc, error) {
	var doc policySegmentDoc
	if err := json.Unmarshal([]byte(seg), &doc); err != nil {
		return doc, fmt.Errorf("配置包第 7 段不是 JSON 对象: %w", err)
	}
	if doc.Rows == nil {
		return doc, errors.New("配置包第 7 段没有 rows 键（一条策略都没有也要发空数组，空数组和「没有这个键」是两件事）")
	}
	return doc, nil
}

// policyPlane 是这一段用到的 VPP API。测试用假端，真实现是 *vppapi.Client。
type policyPlane interface {
	DumpInterfaces(context.Context) ([]vppapi.SwInterface, error)
	// IPTableAddDel 只用来在 ABF 之前把目标颜色表补出来：颜色表的 owner 是
	// sr-fib，但它是另一个进程，这一轮跑的时候那张表可能还没被建出来。
	IPTableAddDel(context.Context, uint32, bool) error
	ACLs(context.Context) ([]vppapi.ACLDetail, error)
	ACLAddReplace(context.Context, uint32, string, []vppapi.ACLRule) (uint32, error)
	ACLDel(context.Context, uint32) error
	ABFPolicies(context.Context) ([]vppapi.ABFPolicy, error)
	ABFPolicyAddDel(context.Context, vppapi.ABFPolicy, bool) error
	ABFAttaches(context.Context) ([]vppapi.ABFAttach, error)
	ABFAttachAddDel(context.Context, vppapi.ABFAttach, bool) error
	Close() error
}

// desiredRow 是一行策略解开之后的样子：VPP 需要的三样东西都在这里。
type desiredRow struct {
	label  string
	aclTag string
	abfID  uint32
	rules  []vppapi.ACLRule
	table  uint32
	ifaces []uint32
	prio   uint32
}

// applyPolicy 把 VPP 里的 ACL/ABF/挂点对账到第 7 段。段不存在时什么都不动
// （老配置包）："没有这一段"与"策略清空"是两件事，前者不该把现存的策略删掉。
func (a *Applier) applyPolicy(ctx context.Context, inst Installed) error {
	if inst.Policy == "" {
		a.Logf("policy: 这份配置包没有第 7 段，VPP 里的 ACL/ABF 一条没动")
		return nil
	}
	doc, err := decodePolicySegment(inst.Policy)
	if err != nil {
		return err
	}
	rows := *doc.Rows
	if a.DryRun {
		a.Logf("policy: 会把 %d 条策略与 VPP 现状对账（dry-run 不连 api.sock）", len(rows))
		return nil
	}
	plane, err := a.openPolicyPlane(ctx)
	if err != nil {
		return err
	}
	defer plane.Close()

	desired, err := planPolicyRows(ctx, plane, rows)
	if err != nil {
		return err
	}

	// 现状：三个 dump 各读一次，之后所有"要不要写"都以它们为准。
	liveACLs, err := plane.ACLs(ctx)
	if err != nil {
		return fmt.Errorf("policy: 读 VPP 现有 ACL: %w", err)
	}
	livePols, err := plane.ABFPolicies(ctx)
	if err != nil {
		return fmt.Errorf("policy: 读 VPP 现有 ABF 策略: %w", err)
	}
	liveAtts, err := plane.ABFAttaches(ctx)
	if err != nil {
		return fmt.Errorf("policy: 读 VPP 现有挂点: %w", err)
	}

	tagIndex := map[string]uint32{} // 我们的 tag → VPP 分配的 acl_index
	rulesOf := map[uint32][]byte{}  // acl_index → 规则字节
	for _, d := range liveACLs {
		if !strings.HasPrefix(d.Tag, policyTagPrefix) {
			continue // 不是我们建的 ACL，不碰
		}
		tagIndex[d.Tag] = d.Index
		rulesOf[d.Index] = d.Rules
	}
	polByID := map[uint32]vppapi.ABFPolicy{}
	for _, p := range livePols {
		if !oursPolicyID(p.ID) {
			continue
		}
		polByID[p.ID] = p
	}
	type attKey struct {
		policy uint32
		ifidx  uint32
	}
	attByKey := map[attKey]vppapi.ABFAttach{}
	for _, at := range liveAtts {
		if !oursPolicyID(at.PolicyID) {
			continue
		}
		attByKey[attKey{at.PolicyID, at.SWIfIndex}] = at
	}

	// 一轮对账要写什么，先算清楚，然后按固定顺序执行：先摘挂点、再删策略、
	// 再写 ACL、再建策略、最后挂回去。
	//
	// aclStat：tag 在 VPP 里的现状。missing/replace 的区分只影响日志与写动作；
	// 两种情况都要写一次 ACL。
	aclMissing := map[string]bool{}
	aclReplace := map[string]bool{}
	for _, d := range desired {
		idx, ok := tagIndex[d.aclTag]
		switch {
		case !ok:
			aclMissing[d.aclTag] = true
		case !bytes.Equal(rulesOf[idx], vppapi.ACLRulesBlob(d.rules)):
			aclReplace[d.aclTag] = true
		}
	}
	// recreate：VPP 里那条策略必须删了重建 —— 它引用的 ACL 要新建，或者它指的
	// 表/ACL 已经不是这一版要的。
	recreate := map[uint32]bool{}
	for _, d := range desired {
		live, ok := polByID[d.abfID]
		if !ok {
			continue
		}
		if aclMissing[d.aclTag] {
			recreate[d.abfID] = true
			continue
		}
		if !policyUpToDate(d, live, tagIndex) {
			recreate[d.abfID] = true
		}
	}

	// 1. 先摘挂点：多出来的，以及策略要重建的那些。
	//    （删一条还挂着的策略会让 VPP 进程死，见文件头第 3 条。）
	wantAtt := map[attKey]bool{}
	for _, d := range desired {
		for _, idx := range d.ifaces {
			wantAtt[attKey{d.abfID, idx}] = true
		}
	}
	var detach []vppapi.ABFAttach
	for k, at := range attByKey {
		if !wantAtt[k] || recreate[k.policy] {
			detach = append(detach, at)
		}
	}
	detach = dedupeAttaches(detach)
	for _, at := range detach {
		if err := plane.ABFAttachAddDel(ctx, at, false); err != nil {
			return fmt.Errorf("policy: 摘挂点 policy %d → 口 #%d 失败（不摘就删策略会让 VPP 挂掉，这一轮到此为止）: %w",
				at.PolicyID, at.SWIfIndex, err)
		}
		a.Logf("policy: 摘挂点 policy %d → 口 #%d", at.PolicyID, at.SWIfIndex)
	}

	// 2. 删多余/要重建的策略。删的时候必须把 VPP 里那份 acl 与路径原样回显：
	//    带一条不存在的路径会被拒成 -7，而"什么都没有"的删除也不是合法请求。
	wantPolicy := map[uint32]bool{}
	for _, d := range desired {
		wantPolicy[d.abfID] = true
	}
	byID := make([]uint32, 0, len(polByID))
	for id := range polByID {
		byID = append(byID, id)
	}
	sort.Slice(byID, func(i, j int) bool { return byID[i] < byID[j] })
	for _, id := range byID {
		if wantPolicy[id] && !recreate[id] {
			continue
		}
		p := polByID[id]
		if err := plane.ABFPolicyAddDel(ctx, p, false); err != nil {
			return fmt.Errorf("policy: 删策略 %d 失败: %w", id, err)
		}
		a.Logf("policy: 删策略 %d（acl=%d paths=%v）", p.ID, p.ACLIndex, p.Paths)
		delete(polByID, id)
	}

	// 3. 写 ACL：已存在的原地替换（index 不变，挂在上面的策略不用动），
	//    没有的新建（index 由 VPP 分配）。这里的"新建"必须用 ACLNew 表示：
	//    VPP 分配的第一个索引就是 0，拿 0 当哨兵会把新建写成"替换 0 号"。
	for _, d := range desired {
		if !aclMissing[d.aclTag] && !aclReplace[d.aclTag] {
			continue
		}
		idx, exists := tagIndex[d.aclTag]
		if !exists {
			idx = vppapi.ACLNew
		}
		got, err := plane.ACLAddReplace(ctx, idx, d.aclTag, d.rules)
		if err != nil {
			return fmt.Errorf("policy: %s 的 ACL 写不进去: %w", d.label, err)
		}
		tagIndex[d.aclTag] = got
		verb := "替换"
		if !exists {
			verb = "新建"
		}
		a.Logf("policy: %s ACL %s %s -> index %d（%d 条规则）", d.label, d.aclTag, verb, got, len(d.rules))
	}

	// 4. 建策略之前先保证目标表在。
	//
	// 颜色表是 sr-fib 建的，但 sr-fib 是另一个进程、按自己的节拍收敛：这一轮跑到
	// 这里时，那张表可能还没被建出来（首配，或者刚加了一条指向新颜色的策略）。
	// ABF 指着一张不存在的表会被 VPP 拒成 NO_SUCH_FIB(-3)，可那不是"策略写错了"，
	// 是两个进程的先后问题 —— 所以这一半自己补一次，表已经在了就是空操作
	// （ip_table_add_del 重复添加幂等，实测 2026-11-10）。
	for _, tbl := range policyTables(desired) {
		if err := plane.IPTableAddDel(ctx, tbl, true); err != nil {
			return fmt.Errorf("policy: 目标表 %d 建不出来（这一行的颜色表要先存在才能让 ABF 指过去）: %w", tbl, err)
		}
	}

	// 5. 建策略：不存在或刚被删掉的。
	for _, d := range desired {
		if _, ok := polByID[d.abfID]; ok {
			a.Logf("policy: %s 已存在且一致（policy %d → 表 %d），不动", d.label, d.abfID, d.table)
			continue
		}
		idx, ok := tagIndex[d.aclTag]
		if !ok {
			return fmt.Errorf("policy: %s 的 ACL 没有 index（上一步没建成？）", d.label)
		}
		p := vppapi.ABFPolicy{ID: d.abfID, ACLIndex: idx, Paths: []vppapi.ABFPath{{Table: d.table}}}
		if err := plane.ABFPolicyAddDel(ctx, p, true); err != nil {
			return fmt.Errorf("policy: %s 的策略建不出来（查表 %d）: %w", d.label, d.table, err)
		}
		a.Logf("policy: %s 建策略 %d → 表 %d（acl index %d）", d.label, d.abfID, d.table, idx)
	}

	// 6. 补挂点：这一版要的、现在不在的那些。
	for _, d := range desired {
		for _, idx := range d.ifaces {
			_, live := attByKey[attKey{d.abfID, idx}]
			if live && !containsAttach(detach, d.abfID, idx) {
				continue
			}
			at := vppapi.ABFAttach{PolicyID: d.abfID, SWIfIndex: idx, Priority: d.prio}
			if err := plane.ABFAttachAddDel(ctx, at, true); err != nil {
				return fmt.Errorf("policy: %s 挂到口 #%d 失败: %w", d.label, idx, err)
			}
			a.Logf("policy: %s 挂到口 #%d（priority %d）", d.label, idx, d.prio)
		}
	}

	// 7. 清掉不再属于任何策略的 ACL（只动我们 tag 前缀的那些，别人的 ACL 不碰）。
	keepTag := map[string]bool{}
	for _, d := range desired {
		keepTag[d.aclTag] = true
	}
	for tag, idx := range tagIndex {
		if keepTag[tag] {
			continue
		}
		if err := plane.ACLDel(ctx, idx); err != nil {
			return fmt.Errorf("policy: 删 ACL %s（index %d）失败: %w", tag, idx, err)
		}
		a.Logf("policy: 删 ACL %s（index %d）", tag, idx)
	}
	return a.verifyPolicy(ctx, plane, desired)
}

// openPolicyPlane dials the VPP api socket for the 7th segment. ACL has no usable
// CLI at all on this build, so a box without an api socket cannot do policy --
// that is an error, not a fallback.
func (a *Applier) openPolicyPlane(ctx context.Context) (policyPlane, error) {
	if a.VPPAPI == "" {
		return nil, errors.New("第 7 段要配策略，但这个 applier 没有 VPP api.sock（--vpp-api / -vpp-api）：" +
			"ACL 的 CLI 在 26.06 上返回成功但什么都不建（实测 2026-11-10）")
	}
	plane, err := a.DialPolicy(ctx, a.VPPAPI)
	if err != nil {
		return nil, fmt.Errorf("连 VPP api.sock %s: %w", a.VPPAPI, err)
	}
	return plane, nil
}

// planPolicyRows 把行变成 VPP 能用的东西：ACL 规则、目标表、入口口 index。
// 任何一行不合法都整段拒绝 —— 半套策略比没有策略更难查。
func planPolicyRows(ctx context.Context, plane policyPlane, rows []policyRowDoc) ([]desiredRow, error) {
	var byName map[string]uint32
	needIfaces := false
	for _, r := range rows {
		if len(r.Attach) > 0 {
			needIfaces = true
		}
	}
	if needIfaces {
		ifs, err := plane.DumpInterfaces(ctx)
		if err != nil {
			return nil, fmt.Errorf("policy: 读 VPP 接口清单: %w", err)
		}
		byName = make(map[string]uint32, len(ifs))
		for _, i := range ifs {
			byName[i.Name] = i.SWIfIndex
		}
	}
	out := make([]desiredRow, 0, len(rows))
	seenID := map[int]bool{}
	for _, r := range rows {
		label := fmt.Sprintf("行 %d（VPN %d，颜色 %d）", r.ID, r.VPN, r.Color)
		if r.ID < 1 || r.ID > policyMaxRowID {
			return nil, fmt.Errorf("policy: %s 的行号必须在 1..%d 之间（它要进 ACL tag 与 policy_id）", label, policyMaxRowID)
		}
		if seenID[r.ID] {
			return nil, fmt.Errorf("policy: 行号 %d 重复：一个行号只能有一条策略", r.ID)
		}
		seenID[r.ID] = true
		if r.VPN > uint32(fib.MaxVRFID) || r.VPN == uint32(fib.OverlayTable) {
			return nil, fmt.Errorf("policy: %s 的 VPN 号不合法（0..%d，%d 留给 overlay）", label, fib.MaxVRFID, fib.OverlayTable)
		}
		if r.Color > fib.MaxColor {
			return nil, fmt.Errorf("policy: %s 的颜色超出 0..%d", label, fib.MaxColor)
		}
		table, err := fib.ColorTable(r.Color, r.VPN)
		if err != nil {
			return nil, fmt.Errorf("policy: %s: %w", label, err)
		}
		rules, err := aclRulesFor(r)
		if err != nil {
			return nil, fmt.Errorf("policy: %s: %w", label, err)
		}
		d := desiredRow{
			label:  label,
			aclTag: policyTagPrefix + strconv.Itoa(r.ID),
			abfID:  policyABFBase + uint32(r.ID),
			rules:  rules,
			table:  table,
			prio:   policyAttachBase + uint32(r.ID),
		}
		if len(r.Attach) == 0 {
			return nil, fmt.Errorf("policy: %s 一个入口口都没有（这个 VPN 下没有接口？）：装了也不会匹配任何流量", label)
		}
		seenIf := map[uint32]bool{}
		for _, name := range r.Attach {
			idx, ok := byName[name]
			if !ok {
				return nil, fmt.Errorf("policy: %s 要挂在 VPP 口 %s 上，但这个口现在不存在（第 1/3 段没把它建出来？）", label, name)
			}
			if seenIf[idx] {
				continue
			}
			seenIf[idx] = true
			d.ifaces = append(d.ifaces, idx)
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].abfID < out[j].abfID })
	return out, nil
}

// aclRulesFor 把一行策略折成一条 permit 规则。一个 0.0.0.0/0 + proto any 的
// 规则就是"全部流量"，这正是"某颜色承载所有流量"的写法。
func aclRulesFor(r policyRowDoc) ([]vppapi.ACLRule, error) {
	src, err := prefixOrAny(r.Src)
	if err != nil {
		return nil, fmt.Errorf("源前缀「%s」: %w", r.Src, err)
	}
	dst, err := prefixOrAny(r.Dst)
	if err != nil {
		return nil, fmt.Errorf("目的前缀「%s」: %w", r.Dst, err)
	}
	switch r.Proto {
	case 0, 1, 6, 17:
	default:
		return nil, fmt.Errorf("协议 %d 不认识（0=任意 / 1=icmp / 6=tcp / 17=udp）", r.Proto)
	}
	if r.Proto != 6 && r.Proto != 17 && (r.SrcPort != "" || r.DstPort != "") {
		return nil, fmt.Errorf("协议 %d 上不该有端口（端口只在 tcp/udp 上有意义；icmp 的那两个字段是 type/code，本系统不按它匹配）", r.Proto)
	}
	sport, err := portRange(r.SrcPort)
	if err != nil {
		return nil, fmt.Errorf("源端口「%s」: %w", r.SrcPort, err)
	}
	dport, err := portRange(r.DstPort)
	if err != nil {
		return nil, fmt.Errorf("目的端口「%s」: %w", r.DstPort, err)
	}
	return []vppapi.ACLRule{{
		Action:  vppapi.ACLPermit,
		Src:     src,
		Dst:     dst,
		Proto:   r.Proto,
		SrcPort: sport,
		DstPort: dport,
	}}, nil
}

func prefixOrAny(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "any" {
		return netip.MustParsePrefix("0.0.0.0/0"), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	if !p.Addr().Is4() {
		return netip.Prefix{}, errors.New("只支持 IPv4")
	}
	return p.Masked(), nil
}

func portRange(s string) (vppapi.PortRange, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "any" {
		return vppapi.AnyPorts(), nil
	}
	lo, hi, cut := strings.Cut(s, "-")
	first, err := strconv.ParseUint(strings.TrimSpace(lo), 10, 16)
	if err != nil {
		return vppapi.PortRange{}, fmt.Errorf("不是端口号: %w", err)
	}
	last := first
	if cut {
		v, err := strconv.ParseUint(strings.TrimSpace(hi), 10, 16)
		if err != nil {
			return vppapi.PortRange{}, fmt.Errorf("区间上界不是端口号: %w", err)
		}
		last = v
	}
	if last < first {
		return vppapi.PortRange{}, fmt.Errorf("区间起止颠倒（%d-%d）", first, last)
	}
	return vppapi.PortRange{First: uint16(first), Last: uint16(last)}, nil
}

func oursPolicyID(id uint32) bool {
	return id > policyABFBase && id <= policyABFBase+policyMaxRowID
}

// policyTables 列出这一版要保证存在的目标表（去重、升序）。表 0 不在里面：
// 它是 global 表，本来就存在，建不得。
func policyTables(desired []desiredRow) []uint32 {
	seen := map[uint32]bool{}
	out := make([]uint32, 0, len(desired))
	for _, d := range desired {
		if d.table == 0 || seen[d.table] {
			continue
		}
		seen[d.table] = true
		out = append(out, d.table)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// policyUpToDate 判"这一行在 VPP 里已经是对的样子"：ACL index 对得上，
// 路径就是那一条查颜色表。ACL 的**内容**不在这里比 —— 它由上一步的
// aclNeedsWrite 决定，内容变了但 index 不变时原地替换即可，不必重建策略。
func policyUpToDate(d desiredRow, live vppapi.ABFPolicy, tagIndex map[string]uint32) bool {
	idx, ok := tagIndex[d.aclTag]
	if !ok || live.ACLIndex != idx {
		return false
	}
	return policyPathsEqual(d, live)
}

func policyPathsEqual(d desiredRow, live vppapi.ABFPolicy) bool {
	return len(live.Paths) == 1 && live.Paths[0].Table == d.table
}

func dedupeAttaches(in []vppapi.ABFAttach) []vppapi.ABFAttach {
	seen := map[[2]uint32]bool{}
	out := in[:0]
	for _, a := range in {
		k := [2]uint32{a.PolicyID, a.SWIfIndex}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, a)
	}
	return out
}

func containsAttach(list []vppapi.ABFAttach, policy, idx uint32) bool {
	for _, a := range list {
		if a.PolicyID == policy && a.SWIfIndex == idx {
			return true
		}
	}
	return false
}

// verifyPolicy 读回三个对象，逐项对；不一致就报错（退出码不是证据）。
func (a *Applier) verifyPolicy(ctx context.Context, plane policyPlane, desired []desiredRow) error {
	acls, err := plane.ACLs(ctx)
	if err != nil {
		return fmt.Errorf("policy: 复检时读不到 ACL: %w", err)
	}
	pols, err := plane.ABFPolicies(ctx)
	if err != nil {
		return fmt.Errorf("policy: 复检时读不到 ABF 策略: %w", err)
	}
	atts, err := plane.ABFAttaches(ctx)
	if err != nil {
		return fmt.Errorf("policy: 复检时读不到挂点: %w", err)
	}
	tagIndex := map[string]uint32{}
	rulesOf := map[uint32][]byte{}
	for _, d := range acls {
		if strings.HasPrefix(d.Tag, policyTagPrefix) {
			tagIndex[d.Tag] = d.Index
			rulesOf[d.Index] = d.Rules
		}
	}
	polByID := map[uint32]vppapi.ABFPolicy{}
	for _, p := range pols {
		if oursPolicyID(p.ID) {
			polByID[p.ID] = p
		}
	}
	attSet := map[[2]uint32]bool{}
	for _, at := range atts {
		attSet[[2]uint32{at.PolicyID, at.SWIfIndex}] = true
	}
	var problems []string
	wantID := map[uint32]bool{}
	for _, d := range desired {
		wantID[d.abfID] = true
		idx, ok := tagIndex[d.aclTag]
		if !ok {
			problems = append(problems, d.label+"：VPP 里没有它的 ACL")
		} else if !bytes.Equal(rulesOf[idx], vppapi.ACLRulesBlob(d.rules)) {
			problems = append(problems, fmt.Sprintf("%s：ACL index %d 的规则和要配的不一样", d.label, idx))
		}
		p, hasPol := polByID[d.abfID]
		if !hasPol {
			problems = append(problems, fmt.Sprintf("%s：VPP 里没有策略 %d", d.label, d.abfID))
		} else {
			if ok && p.ACLIndex != idx {
				problems = append(problems, fmt.Sprintf("%s：策略 %d 指的 ACL 是 %d，应该是 %d", d.label, d.abfID, p.ACLIndex, idx))
			}
			if !policyPathsEqual(d, p) {
				problems = append(problems, fmt.Sprintf("%s：策略 %d 的路径是 %v，应该是查表 %d", d.label, d.abfID, p.Paths, d.table))
			}
		}
		for _, ifidx := range d.ifaces {
			if !attSet[[2]uint32{d.abfID, ifidx}] {
				problems = append(problems, fmt.Sprintf("%s：策略 %d 没挂到口 #%d", d.label, d.abfID, ifidx))
			}
		}
	}
	for id := range polByID {
		if !wantID[id] {
			problems = append(problems, fmt.Sprintf("策略 %d 不在这一版清单里，但 VPP 里还在", id))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("policy: 复检不一致（%d 处）：%s", len(problems), strings.Join(problems, "；"))
	}
	for _, d := range desired {
		a.Logf("policy: 复检 %s -> 表 %d，挂 %d 个口", d.label, d.table, len(d.ifaces))
	}
	return nil
}
