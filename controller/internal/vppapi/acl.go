// ACL：静态策略的匹配器（acl-plugin 的二进制 API）。
//
// 为什么不用 CLI：`set acl-plugin acl ...` 在 26.06 上**返回成功但什么都没建**
// （2026-11-10 在 CPE1 实测：rc=0、`show acl-plugin acl` 空、而 `abf policy add`
// 照样接受一个不存在的 acl index）。本包已有的那条结论在这里原样成立——CLI 把
// 失败吞掉，二进制 API 才会把 retval 带回来。
//
// 字节布局取自机箱上装的 /usr/share/vpp/api/plugins/acl.api.json（26.06）：
//
//	acl_add_replace      acl_index(4) tag(64) count(4) r[count]
//	acl_rule             48 B = action(1) src(18) dst(18) proto(1)
//	                             sport_first(2) sport_last(2) dport_first(2) dport_last(2)
//	                             tcp_flags_mask(1) tcp_flags_value(1)
//
// **action 与 proto 是 1 字节枚举**（acl.api.json 里两者都带 enumtype u8），
// 只有 fib_path 的 type/flags/nh_proto 才是 4 字节。写成 4 字节会让后面每个字段
// 偏 3 个字节，而 VPP 报的是 "Invalid src address"(-57) —— 2026-11-10 第一次
// 真机试装就是这么挂了，改回来立刻通过。
//
//	acl_add_replace_reply acl_index(4) retval(4)   ← 与别的回包不同：retval 在 index 之后
//
// prefix 是 packed 18 字节（address = af u8 + union[16]，再一个 len u8），与
// fib_path 里那个 prefix 同一形状，复用 appendIPv4Prefix。
package vppapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
)

// ACLAction 是 vl_api_acl_action_t 的取值。只暴露计划里用得到的两个：
// 本系统不做 reflect（会建会话表，与"无状态静态策略"的语义不符）。
type ACLAction uint32

const (
	ACLDeny   ACLAction = 0
	ACLPermit ACLAction = 1
)

// ACLNew 是 ACLAddReplace 的"新建、索引由 VPP 分配"标记。
//
// **不能拿 0 当这个标记**：VPP 给第一张 ACL 分配的索引就是 0。2026-11-10 在 VM
// 实验台 CPE1 首次下发策略时实测到这一点 —— `show acl-plugin acl` 打出
// `acl-index 0 tag {sr-pol-1}` 与 `acl-index 1 tag {sr-pol-2}`，而本包当时把
// "0" 当哨兵，于是"建好了 ACL"和"没有 ACL"在返回值上无法区分：策略创建那步
// 被判成"没有 index"，整段 apply 以 `ABF 策略 1001 需要一个 ACL index` 失败。
//
// 用 0 当哨兵的第二种坏法更隐蔽：替换 0 号 ACL 会被改写成"新建"，于是每轮都对账
// 出一张新 ACL，索引一路往上涨，旧的一直留着。
const ACLNew = ^uint32(0)

const (
	aclTagLen     = 64
	aclRuleLen    = 48
	aclPortAnyEnd = 0xffff // 端口字段的"任意"是 0..65535，不是 0
)

// PortRange 是一个闭区间。零值不是"任意"——要任意就写 AnyPorts()，
// 让"忘了填"变成 0..0（只匹配端口 0），而不是静默放过全部端口。
type PortRange struct {
	First uint16
	Last  uint16
}

// AnyPorts 是"任意端口"。ICMP 时这两个字段是 type/code，语义见 ACLRule。
func AnyPorts() PortRange { return PortRange{First: 0, Last: aclPortAnyEnd} }

func (p PortRange) valid() bool { return p.First <= p.Last }

// ACLRule 是一条五元组规则。Proto=0 表示任意协议；此时四个端口字段必须都是
// AnyPorts（VPP 对 ICMP 才把端口位解释成 type/code，别的协议上写具体端口没有意义）。
type ACLRule struct {
	Action  ACLAction
	Src     netip.Prefix
	Dst     netip.Prefix
	Proto   uint8
	SrcPort PortRange
	DstPort PortRange
}

func (r ACLRule) validate() error {
	if r.Action != ACLPermit && r.Action != ACLDeny {
		return fmt.Errorf("vppapi: ACL 规则动作 %d 不认识", r.Action)
	}
	for _, p := range []netip.Prefix{r.Src, r.Dst} {
		if !p.Addr().Is4() || p != p.Masked() {
			return fmt.Errorf("vppapi: ACL 前缀 %q 必须是已归一的 IPv4 前缀", p)
		}
	}
	if !r.SrcPort.valid() || !r.DstPort.valid() {
		return fmt.Errorf("vppapi: ACL 端口区间起止颠倒（%v / %v）", r.SrcPort, r.DstPort)
	}
	if r.Proto == 0 {
		if r.SrcPort != AnyPorts() || r.DstPort != AnyPorts() {
			return fmt.Errorf("vppapi: proto=any 的规则不该带端口（%v / %v）", r.SrcPort, r.DstPort)
		}
	}
	return nil
}

// appendACLRule 写一条 vl_api_acl_rule_t（54 字节）。
//
// action 与 proto 都是 **4 字节枚举**（和 fib_path 的 type 一样），不是 u8：
// 按 u8 写会让后面每个字段都偏 3 个字节，而 VPP 只会在第一次匹配时表现成
// "规则对不上"，不会有任何报错。
func appendACLRule(b []byte, r ACLRule) []byte {
	b = append(b, byte(r.Action))
	b = appendIPv4Prefix(b, r.Src)
	b = appendIPv4Prefix(b, r.Dst)
	b = append(b, r.Proto)
	b = appendU16(b, r.SrcPort.First)
	b = appendU16(b, r.SrcPort.Last)
	b = appendU16(b, r.DstPort.First)
	b = appendU16(b, r.DstPort.Last)
	b = append(b, 0) // tcp_flags_mask：不匹配标志位（掩码 0 = 全不关心）
	b = append(b, 0) // tcp_flags_value
	return b
}

// ACLAddReplace 建或整条替换一个 ACL，返回 VPP 侧的 acl_index。
//
// index = ACLNew 表示新建（内部发 ~0）；给任何别的值就是整条替换那个索引 ——
// 包括 0，因为 0 是 VPP 会给出来的真实索引（见 ACLNew 的注释）。这是本系统要的
// 语义：策略行集合变了，ACL 必须是新那一份，不是增量叠加。
func (c *Client) ACLAddReplace(ctx context.Context, index uint32, tag string, rules []ACLRule) (uint32, error) {
	if len(rules) == 0 {
		return 0, fmt.Errorf("vppapi: ACL 至少要有 1 条规则")
	}
	if len(tag) >= aclTagLen {
		return 0, fmt.Errorf("vppapi: ACL tag %q 超过 %d 字节", tag, aclTagLen-1)
	}
	for i, r := range rules {
		if err := r.validate(); err != nil {
			return 0, fmt.Errorf("第 %d 条规则: %w", i+1, err)
		}
	}

	body := make([]byte, 0, 4+aclTagLen+4+len(rules)*aclRuleLen)
	body = appendU32(body, index)
	tagBuf := make([]byte, aclTagLen)
	copy(tagBuf, tag)
	body = append(body, tagBuf...)
	body = appendU32(body, uint32(len(rules)))
	for _, r := range rules {
		body = appendACLRule(body, r)
	}

	frame, err := c.call(ctx, "acl_add_replace", "acl_add_replace_reply", body)
	if err != nil {
		return 0, err
	}
	if len(frame) < 14 {
		return 0, fmt.Errorf("vppapi: acl_add_replace_reply 太短（%d 字节）", len(frame))
	}
	got := binary.BigEndian.Uint32(frame[6:10])
	if rv := int32(binary.BigEndian.Uint32(frame[10:14])); rv != 0 {
		return 0, APIError{Op: "acl_add_replace", Retval: rv}
	}
	return got, nil
}

// ACLDetail is what a reconcile needs back from acl_dump: the index VPP assigned,
// the tag this system put on it, and the rules byte-for-byte as VPP holds them.
//
// Rules is the raw wire image (count*48 bytes) on purpose. Comparing it with
// aclRulesBlob(desired) is the only honest "same rules?" test available: the
// plugin does not promise that its dump echoes prefixes the way they were sent,
// so a decoded comparison would have to guess at normalisation, while a byte
// comparison against VPP's own output proves the bytes it holds are the bytes we
// meant.
type ACLDetail struct {
	Index uint32
	Tag   string
	Rules []byte
}

// ACLRulesBlob is the wire encoding of a rule list (no tag, no count). It is
// exported because the reconciler compares it against acl_details' raw rule
// bytes -- see ACLDetail.
func ACLRulesBlob(rules []ACLRule) []byte {
	b := make([]byte, 0, len(rules)*aclRuleLen)
	for _, r := range rules {
		b = appendACLRule(b, r)
	}
	return b
}

// ACLs reads every ACL on the box. The tag is the identity a reconcile keys on:
// an acl_index is assigned by VPP and can never be chosen by the caller
// (acl_add_replace with an index that does not exist answers NO_SUCH_ENTRY -6,
// measured on lab-cpe1 2026-11-10), so "policy row 7" has to be spelled as a tag.
func (c *Client) ACLs(ctx context.Context) ([]ACLDetail, error) {
	// acl_index = ~0 means "every ACL". An empty body would be read as index 0,
	// and ACL 0 never exists (indices start at 1 in practice), so the dump would
	// answer nothing at all -- measured on lab-cpe1, 2026-11-10.
	body := appendU32(nil, ^uint32(0))
	frames, err := c.dump(ctx, "acl_dump", "acl_details", body)
	if err != nil {
		return nil, err
	}
	out := make([]ACLDetail, 0, len(frames))
	for _, f := range frames {
		d, err := parseACLDetails(f)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// parseACLDetails 解一帧 acl_details：header 6 + acl_index 4 + tag(64 定长) +
// count 4 + r[count]（每条 48 字节）。
//
// tag 在这里是**定长 64 字节**字段（acl.api.json 里写作 ["string","tag",64]），
// 不是"长度前缀 + 内容"那种变长 string：按变长读会让 count 落在错误的位置，
// 而错位的 count 看上去只是一个离谱的数字。
func parseACLDetails(f []byte) (ACLDetail, error) {
	const head = 6 + 4 + aclTagLen + 4
	if len(f) < head {
		return ACLDetail{}, fmt.Errorf("vppapi: acl_details 太短（%d 字节）", len(f))
	}
	d := ACLDetail{Index: binary.BigEndian.Uint32(f[6:10])}
	tag := f[10 : 10+aclTagLen]
	if i := bytes.IndexByte(tag, 0); i >= 0 {
		tag = tag[:i]
	}
	d.Tag = string(tag)
	n := int(binary.BigEndian.Uint32(f[10+aclTagLen : head]))
	if len(f) < head+n*aclRuleLen {
		return ACLDetail{}, fmt.Errorf("vppapi: acl_details 声称 %d 条规则但只有 %d 字节", n, len(f))
	}
	d.Rules = f[head : head+n*aclRuleLen]
	return d, nil
}

// ACLDel 删掉一个 ACL。删除不存在的 ACL 会拿到 NO_SUCH_ENTRY(-6)，调用方按
// "已经是期望态"处理即可（reconcile 的常态）。
func (c *Client) ACLDel(ctx context.Context, index uint32) error {
	body := make([]byte, 0, 4)
	body = appendU32(body, index)
	frame, err := c.call(ctx, "acl_del", "acl_del_reply", body)
	if err != nil {
		return err
	}
	return c.retval("acl_del", frame)
}
