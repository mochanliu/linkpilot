package apply

// What the 7th segment could get wrong without anyone noticing. Each test below
// is one of those, and each one is a behaviour the probe on lab-cpe1 measured
// (2026-11-10), not an opinion:
//
//   - an ACL keyed by index instead of by tag: VPP assigns the index, so the
//     second reconcile would create a second ACL for the same row;
//   - "changing a row" sent as add-with-a-new-ACL: VPP answers -7 and keeps the
//     old policy, so the row looks changed and is not;
//   - deleting a policy that is still attached: VPP dies;
//   - an ABF policy pointing at a colour table sr-fib has not created yet:
//     NO_SUCH_FIB(-3), which is an ordering problem between two processes, not a
//     configuration mistake.
//
// The fake plane is deliberately hostile in those four places.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"testing"

	"sdwan/controller/internal/vppapi"
)

const (
	policyIfA     = "host-eth1"
	policyIfAIdx  = uint32(11)
	policyIfB     = "host-eth2"
	policyIfBIdx  = uint32(12)
	policyVPN     = uint32(10)
	policyColor2  = uint32(2*1000) + policyVPN // 2010
	policyColor3  = uint32(3*1000) + policyVPN // 3010
	policyOtherIf = "host-eth9"
)

type fakeACL struct {
	index uint32
	tag   string
	rules []byte
}

type fakePolicyPlane struct {
	ifaces []vppapi.SwInterface

	acls      map[uint32]*fakeACL
	nextACL   uint32
	policies  map[uint32]vppapi.ABFPolicy
	attaches  map[[2]uint32]vppapi.ABFAttach
	tables    map[uint32]bool
	calls     []string
	closed    int
	tableErr  error
	aclErr    error
	addErr    error
	attachErr error
}

func newFakePolicyPlane() *fakePolicyPlane {
	return &fakePolicyPlane{
		ifaces: []vppapi.SwInterface{
			{SWIfIndex: policyIfAIdx, Name: policyIfA},
			{SWIfIndex: policyIfBIdx, Name: policyIfB},
		},
		acls:     map[uint32]*fakeACL{},
		nextACL:  1,
		policies: map[uint32]vppapi.ABFPolicy{},
		attaches: map[[2]uint32]vppapi.ABFAttach{},
		tables:   map[uint32]bool{},
	}
}

func (p *fakePolicyPlane) DumpInterfaces(context.Context) ([]vppapi.SwInterface, error) {
	return p.ifaces, nil
}

func (p *fakePolicyPlane) IPTableAddDel(_ context.Context, table uint32, add bool) error {
	p.calls = append(p.calls, fmt.Sprintf("table(%d,%v)", table, add))
	if p.tableErr != nil {
		return p.tableErr
	}
	if add {
		p.tables[table] = true // 重复添加幂等（实测）
		return nil
	}
	delete(p.tables, table)
	return nil
}

func (p *fakePolicyPlane) ACLs(context.Context) ([]vppapi.ACLDetail, error) {
	idxs := make([]uint32, 0, len(p.acls))
	for i := range p.acls {
		idxs = append(idxs, i)
	}
	sort.Slice(idxs, func(i, j int) bool { return idxs[i] < idxs[j] })
	out := make([]vppapi.ACLDetail, 0, len(idxs))
	for _, i := range idxs {
		a := p.acls[i]
		out = append(out, vppapi.ACLDetail{Index: a.index, Tag: a.tag, Rules: a.rules})
	}
	return out, nil
}

func (p *fakePolicyPlane) ACLAddReplace(_ context.Context, index uint32, tag string, rules []vppapi.ACLRule) (uint32, error) {
	if p.aclErr != nil {
		return 0, p.aclErr
	}
	if index != 0 {
		if _, ok := p.acls[index]; !ok {
			// 实测：给一个不存在的 index 是 NO_SUCH_ENTRY，index 只能由 VPP 分配。
			p.calls = append(p.calls, fmt.Sprintf("acl-replace-missing(%d)", index))
			return 0, errors.New("retval -6 NO_SUCH_ENTRY")
		}
		p.acls[index].rules = vppapi.ACLRulesBlob(rules)
		p.acls[index].tag = tag
		p.calls = append(p.calls, fmt.Sprintf("acl-replace(%d,%s)", index, tag))
		return index, nil
	}
	idx := p.nextACL
	p.nextACL++
	p.acls[idx] = &fakeACL{index: idx, tag: tag, rules: vppapi.ACLRulesBlob(rules)}
	p.calls = append(p.calls, fmt.Sprintf("acl-add(%d,%s)", idx, tag))
	return idx, nil
}

func (p *fakePolicyPlane) ACLDel(_ context.Context, index uint32) error {
	if _, ok := p.acls[index]; !ok {
		return errors.New("retval -6 NO_SUCH_ENTRY")
	}
	delete(p.acls, index)
	p.calls = append(p.calls, fmt.Sprintf("acl-del(%d)", index))
	return nil
}

func (p *fakePolicyPlane) ABFPolicies(context.Context) ([]vppapi.ABFPolicy, error) {
	ids := make([]uint32, 0, len(p.policies))
	for id := range p.policies {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]vppapi.ABFPolicy, 0, len(ids))
	for _, id := range ids {
		out = append(out, p.policies[id])
	}
	return out, nil
}

func (p *fakePolicyPlane) ABFPolicyAddDel(_ context.Context, pol vppapi.ABFPolicy, add bool) error {
	if add {
		if p.addErr != nil {
			return p.addErr
		}
		if old, ok := p.policies[pol.ID]; ok {
			if old.ACLIndex != pol.ACLIndex || len(old.Paths) != len(pol.Paths) ||
				(len(pol.Paths) == 1 && old.Paths[0] != pol.Paths[0]) {
				// 实测：同 id 换 ACL/路径的 add 被拒成 -7，旧策略原封不动。
				p.calls = append(p.calls, fmt.Sprintf("policy-add-conflict(%d)", pol.ID))
				return errors.New("retval -7 INVALID_VALUE")
			}
			p.calls = append(p.calls, fmt.Sprintf("policy-add-noop(%d)", pol.ID))
			return nil
		}
		for _, pa := range pol.Paths {
			if !p.tables[pa.Table] {
				// 实测：指着一张不存在的表是 NO_SUCH_FIB(-3)。
				p.calls = append(p.calls, fmt.Sprintf("policy-add-nofib(%d,%d)", pol.ID, pa.Table))
				return errors.New("retval -3 NO_SUCH_FIB")
			}
		}
		p.policies[pol.ID] = pol
		p.calls = append(p.calls, fmt.Sprintf("policy-add(%d,acl=%d,table=%d)", pol.ID, pol.ACLIndex, pol.Paths[0].Table))
		return nil
	}
	if _, ok := p.policies[pol.ID]; !ok {
		return errors.New("retval -6 NO_SUCH_ENTRY")
	}
	for k := range p.attaches {
		if k[0] == pol.ID {
			// 实测：删一条还挂着的策略会让 VPP 进程直接死。这里把它变成一个
			// 测试失败，而不是默默通过。
			p.calls = append(p.calls, fmt.Sprintf("policy-del-while-attached(%d)", pol.ID))
			return errors.New("would crash VPP: policy still attached")
		}
	}
	delete(p.policies, pol.ID)
	p.calls = append(p.calls, fmt.Sprintf("policy-del(%d)", pol.ID))
	return nil
}

func (p *fakePolicyPlane) ABFAttaches(context.Context) ([]vppapi.ABFAttach, error) {
	keys := make([][2]uint32, 0, len(p.attaches))
	for k := range p.attaches {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	out := make([]vppapi.ABFAttach, 0, len(keys))
	for _, k := range keys {
		out = append(out, p.attaches[k])
	}
	return out, nil
}

func (p *fakePolicyPlane) ABFAttachAddDel(_ context.Context, at vppapi.ABFAttach, add bool) error {
	k := [2]uint32{at.PolicyID, at.SWIfIndex}
	if add {
		if p.attachErr != nil {
			return p.attachErr
		}
		if _, ok := p.policies[at.PolicyID]; !ok {
			return errors.New("retval -6 NO_SUCH_ENTRY")
		}
		p.attaches[k] = at
		p.calls = append(p.calls, fmt.Sprintf("attach(%d->#%d,prio=%d)", at.PolicyID, at.SWIfIndex, at.Priority))
		return nil
	}
	if _, ok := p.attaches[k]; !ok {
		return errors.New("retval -6 NO_SUCH_ENTRY")
	}
	delete(p.attaches, k)
	p.calls = append(p.calls, fmt.Sprintf("detach(%d->#%d)", at.PolicyID, at.SWIfIndex))
	return nil
}

func (p *fakePolicyPlane) Close() error {
	p.closed++
	return nil
}

func (p *fakePolicyPlane) sawCall(prefix string) bool {
	for _, c := range p.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// policySegJSON is one row spelled the way the controller renders it, so the
// decoder under test sees production bytes.
type policyRowJSON struct {
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

func policySegment(t *testing.T, rows []policyRowJSON, fallback ...map[string]uint32) string {
	t.Helper()
	if rows == nil {
		rows = []policyRowJSON{}
	}
	doc := map[string]any{"rows": rows, "fallback": fallback}
	if fallback == nil {
		doc["fallback"] = []map[string]uint32{}
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("segment: %v", err)
	}
	return string(data)
}

func policyRowOf(id int, vpn uint32, color uint8, dst string, attach ...string) policyRowJSON {
	return policyRowJSON{ID: id, VPN: vpn, Color: color, Dst: dst, Attach: attach}
}

func newPolicyApplier(t *testing.T, plane *fakePolicyPlane) *Applier {
	t.Helper()
	a := New(Store{Root: t.TempDir()})
	a.Logf = func(string, ...any) {}
	a.VPPAPI = "/run/vpp/api.sock"
	a.DialPolicy = func(_ context.Context, socket string) (policyPlane, error) {
		if socket != "/run/vpp/api.sock" {
			t.Errorf("dialled %q, want the api.sock", socket)
		}
		return plane, nil
	}
	return a
}

func installedPolicy(seg string) Installed {
	return Installed{Boot: "true\n", Policy: seg}
}

func TestPolicySegmentRequiresTheRowsKey(t *testing.T) {
	for _, seg := range []string{`{}`, `{"rows":null}`, `[]`, `not json`} {
		if _, err := decodePolicySegment(seg); err == nil {
			t.Errorf("decodePolicySegment(%s) accepted a segment with no rows key", seg)
		}
	}
	doc, err := decodePolicySegment(`{"rows":[]}`)
	if err != nil || len(*doc.Rows) != 0 {
		t.Fatalf("an empty list must decode as zero rows: %v", err)
	}
}

func TestPolicyApplierWithoutASegmentTouchesNothing(t *testing.T) {
	// A bundle published before policy existed carries no 7th segment. It must not
	// dial the API and must not delete the ACLs that are steering traffic now.
	dialed := false
	a := New(Store{Root: t.TempDir()})
	a.Logf = func(string, ...any) {}
	a.VPPAPI = "/run/vpp/api.sock"
	a.DialPolicy = func(context.Context, string) (policyPlane, error) {
		dialed = true
		return nil, errors.New("this test must not connect")
	}
	if err := a.applyPolicy(context.Background(), installedPolicy("")); err != nil {
		t.Fatalf("no segment: %v", err)
	}
	if dialed {
		t.Fatal("dialled VPP for a bundle with no 7th segment")
	}
}

func TestPolicyDryRunDoesNotDial(t *testing.T) {
	plane := newFakePolicyPlane()
	a := newPolicyApplier(t, plane)
	a.DryRun = true
	seg := policySegment(t, []policyRowJSON{policyRowOf(1, policyVPN, 2, "192.0.2.0/24", policyIfA)})
	if err := a.applyPolicy(context.Background(), installedPolicy(seg)); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(plane.calls) != 0 || plane.closed != 0 {
		t.Fatalf("dry run touched the box: %v", plane.calls)
	}
}

// 一行策略要挂到它 VPN 的每一个客户口上，并且要建出目标颜色表 —— 否则 ABF 会在
// 一张还不存在的表上失败（那多半是 sr-fib 还没跑，不是配置错）。
func TestPolicyAttachesToEveryPortAndCreatesTheTable(t *testing.T) {
	plane := newFakePolicyPlane()
	a := newPolicyApplier(t, plane)
	seg := policySegment(t, []policyRowJSON{
		policyRowOf(1, policyVPN, 2, "192.0.2.0/24", policyIfA, policyIfB),
	})
	if err := a.applyPolicy(context.Background(), installedPolicy(seg)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !plane.tables[policyColor2] {
		t.Fatalf("目标颜色表 %d 没被建出来: %v", policyColor2, plane.calls)
	}
	if len(plane.attaches) != 2 {
		t.Fatalf("应挂到 2 个口上，实际 %v", plane.attaches)
	}
	for _, idx := range []uint32{policyIfAIdx, policyIfBIdx} {
		if _, ok := plane.attaches[[2]uint32{1001, idx}]; !ok {
			t.Errorf("policy 1001 没挂到口 #%d: %v", idx, plane.attaches)
		}
	}
	if got := plane.attaches[[2]uint32{1001, policyIfAIdx}].Priority; got != 1001 {
		t.Errorf("挂点优先级 = %d，应为 1000+行号", got)
	}
	// 建表必须发生在建策略之前，否则第一条策略就 -3。
	var tableAt, addAt = -1, -1
	for i, c := range plane.calls {
		if strings.HasPrefix(c, fmt.Sprintf("table(%d,true)", policyColor2)) && tableAt < 0 {
			tableAt = i
		}
		if strings.HasPrefix(c, "policy-add(") && addAt < 0 {
			addAt = i
		}
	}
	if tableAt < 0 || addAt < 0 || tableAt > addAt {
		t.Fatalf("建表应在建策略之前（table@%d, add@%d）: %v", tableAt, addAt, plane.calls)
	}
}

// 再跑一遍不能有任何写动作 —— 否则每次 apply（开机、心跳）都会把策略拆了重装。
func TestPolicyReconcileIsIdempotent(t *testing.T) {
	plane := newFakePolicyPlane()
	a := newPolicyApplier(t, plane)
	seg := policySegment(t, []policyRowJSON{
		policyRowOf(1, policyVPN, 2, "192.0.2.0/24", policyIfA, policyIfB),
		policyRowOf(2, policyVPN, 3, "", policyIfB),
	}, map[string]uint32{"vpn": policyVPN, "color": 2})
	if err := a.applyPolicy(context.Background(), installedPolicy(seg)); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	first := len(plane.calls)
	if err := a.applyPolicy(context.Background(), installedPolicy(seg)); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	// 第二遍唯一允许的写是"保证目标表在"（重复添加幂等，且它会在 sr-fib 之前跑）。
	added := plane.calls[first:]
	for _, c := range added {
		if !strings.HasPrefix(c, "table(") {
			t.Fatalf("第二遍不该有写动作，实际 %v（第一遍 %v）", added, plane.calls[:first])
		}
	}
	if len(plane.policies) != 2 || len(plane.attaches) != 3 {
		t.Fatalf("第二遍之后策略/挂点数不对: policies=%v attaches=%v", plane.policies, plane.attaches)
	}
}

// 改一行 = 换 ACL + 换表。VPP 对"同 id 换 ACL 的 add"答 -7 并保留旧策略，所以
// 必须 delete + add；而删之前必须先把挂点摘掉，否则 VPP 进程会死。
func TestPolicyRowChangeDeletesBeforeAdding(t *testing.T) {
	plane := newFakePolicyPlane()
	a := newPolicyApplier(t, plane)
	before := policySegment(t, []policyRowJSON{
		policyRowOf(1, policyVPN, 2, "192.0.2.0/24", policyIfA),
	})
	if err := a.applyPolicy(context.Background(), installedPolicy(before)); err != nil {
		t.Fatalf("apply before: %v", err)
	}
	aclBefore := plane.policies[1001].ACLIndex

	plane.calls = nil
	after := policySegment(t, []policyRowJSON{
		policyRowOf(1, policyVPN, 3, "192.0.2.0/24", policyIfA),
	})
	if err := a.applyPolicy(context.Background(), installedPolicy(after)); err != nil {
		t.Fatalf("apply after: %v", err)
	}
	if plane.sawCall("policy-add-conflict") {
		t.Fatalf("换表时走了 add-with-new-ACL（VPP 会答 -7 并保留旧策略）: %v", plane.calls)
	}
	if plane.sawCall("policy-del-while-attached") {
		t.Fatalf("删策略之前没摘挂点（真机上会让 VPP 退出）: %v", plane.calls)
	}
	detachAt, delAt := -1, -1
	for i, c := range plane.calls {
		if strings.HasPrefix(c, "detach(") && detachAt < 0 {
			detachAt = i
		}
		if strings.HasPrefix(c, "policy-del(") && delAt < 0 {
			delAt = i
		}
	}
	if detachAt < 0 || delAt < 0 || detachAt > delAt {
		t.Fatalf("顺序应是先摘挂点再删策略（detach@%d, del@%d）: %v", detachAt, delAt, plane.calls)
	}
	if got := plane.policies[1001].Paths[0].Table; got != policyColor3 {
		t.Fatalf("改完之后指向表 %d，应为 %d", got, policyColor3)
	}
	// ACL 内容变了但 tag 没变：应原地替换（index 不变），而不是新建一个。
	if got := plane.policies[1001].ACLIndex; got != aclBefore {
		t.Fatalf("ACL 应原地替换（index %d → %d），否则每次改行都要多建一个 ACL", aclBefore, got)
	}
	if len(plane.acls) != 1 {
		t.Fatalf("应只剩 1 个 ACL，实际 %v", plane.acls)
	}
}

// 删掉最后一行之后，ACL 与策略都必须消失：空策略是"清掉"，不是"什么都别动"。
func TestPolicyEmptyRowsRemovesEverything(t *testing.T) {
	plane := newFakePolicyPlane()
	a := newPolicyApplier(t, plane)
	seg := policySegment(t, []policyRowJSON{
		policyRowOf(1, policyVPN, 2, "192.0.2.0/24", policyIfA),
		policyRowOf(2, policyVPN, 3, "", policyIfB),
	})
	if err := a.applyPolicy(context.Background(), installedPolicy(seg)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := a.applyPolicy(context.Background(), installedPolicy(policySegment(t, nil))); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if len(plane.policies) != 0 || len(plane.attaches) != 0 || len(plane.acls) != 0 {
		t.Fatalf("清空之后还有残留: policies=%v attaches=%v acls=%v", plane.policies, plane.attaches, plane.acls)
	}
}

func TestPolicyRefusesRowsTheBoxCannotAttach(t *testing.T) {
	cases := []struct {
		name string
		row  policyRowJSON
		want string
	}{
		{"未知接口", policyRowOf(1, policyVPN, 2, "", policyOtherIf), "不存在"},
		{"没有入口口", policyRowOf(1, policyVPN, 2, ""), "一个入口口都没有"},
		{"行号越界", policyRowOf(0, policyVPN, 2, "", policyIfA), "行号"},
		{"颜色越界", policyRowOf(1, policyVPN, 9, "", policyIfA), "颜色"},
		{"协议不认识", policyRowJSON{ID: 1, VPN: policyVPN, Color: 2, Proto: 47, Attach: []string{policyIfA}}, "协议"},
		{"icmp 带端口", policyRowJSON{ID: 1, VPN: policyVPN, Color: 2, Proto: 1, DstPort: "443", Attach: []string{policyIfA}}, "端口"},
		{"前端不合法", policyRowJSON{ID: 1, VPN: policyVPN, Color: 2, Src: "10.0.0.0/33", Attach: []string{policyIfA}}, "源前缀"},
		{"端口区间颠倒", policyRowJSON{ID: 1, VPN: policyVPN, Color: 2, Proto: 6, SrcPort: "90-10", Attach: []string{policyIfA}}, "颠倒"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plane := newFakePolicyPlane()
			a := newPolicyApplier(t, plane)
			err := a.applyPolicy(context.Background(), installedPolicy(policySegment(t, []policyRowJSON{c.row})))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v，应含「%s」", err, c.want)
			}
			if len(plane.calls) != 0 {
				t.Fatalf("整段被拒时不该对 VPP 做任何写动作: %v", plane.calls)
			}
		})
	}
}

// 兜底那半不归这里执行，但这一段必须能把清单交出去（sr-agent 用）。
func TestPolicyFallbacksAreReadable(t *testing.T) {
	seg := policySegment(t, nil, map[string]uint32{"vpn": 10, "color": 3})
	fb, err := PolicyFallbacks(seg)
	if err != nil {
		t.Fatalf("PolicyFallbacks: %v", err)
	}
	if len(fb) != 1 || fb[0].VPN != 10 || fb[0].Color != 3 {
		t.Fatalf("兜底清单 = %+v", fb)
	}
	if fb, err := PolicyFallbacks(""); err != nil || len(fb) != 0 {
		t.Fatalf("空段（老配置包）应返回空表: %+v %v", fb, err)
	}
	// 颜色 0 的兜底没有意义：颜色 0 的落点就是主表本身。
	if _, err := PolicyFallbacks(policySegment(t, nil, map[string]uint32{"vpn": 10, "color": 0})); err == nil {
		t.Fatal("颜色 0 的兜底应被拒")
	}
	if _, err := PolicyFallbacks("not json"); err == nil {
		t.Fatal("损坏的段应报错，而不是静默返回空（兜底会悄悄消失）")
	}
}

// 颜色表建不出来时，错误必须点名是哪张表 —— 下一步 ABF 只会答 -3，那句话不
// 足以让人找到方向。
func TestPolicyTableFailureNamesTheTable(t *testing.T) {
	plane := newFakePolicyPlane()
	plane.tableErr = errors.New("retval -7 INVALID_VALUE")
	a := newPolicyApplier(t, plane)
	seg := policySegment(t, []policyRowJSON{policyRowOf(1, policyVPN, 2, "", policyIfA)})
	err := a.applyPolicy(context.Background(), installedPolicy(seg))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(policyColor2)) {
		t.Fatalf("err = %v，应点名表 %d", err, policyColor2)
	}
}

// 一行里同一个前缀写两遍（attach 重复）不该变成两条挂点。
func TestPolicyDuplicateAttachIsCollapsed(t *testing.T) {
	plane := newFakePolicyPlane()
	a := newPolicyApplier(t, plane)
	seg := policySegment(t, []policyRowJSON{
		policyRowOf(1, policyVPN, 2, "", policyIfA, policyIfA, policyIfB),
	})
	if err := a.applyPolicy(context.Background(), installedPolicy(seg)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(plane.attaches) != 2 {
		t.Fatalf("重复的入口口没有被合并: %v", plane.attaches)
	}
}

// 上一版留下的多余 ACL（比如行号整体后移之后）要被清掉，别人的 ACL 一根都不能动。
func TestPolicyDeletesOnlyOurOrphanACLs(t *testing.T) {
	plane := newFakePolicyPlane()
	// 别人的 ACL：没有 sr-pol- 前缀。
	plane.acls[77] = &fakeACL{index: 77, tag: "someone-else", rules: vppapi.ACLRulesBlob([]vppapi.ACLRule{{
		Action:  vppapi.ACLPermit,
		Src:     netip.MustParsePrefix("0.0.0.0/0"),
		Dst:     netip.MustParsePrefix("0.0.0.0/0"),
		SrcPort: vppapi.AnyPorts(), DstPort: vppapi.AnyPorts(),
	}})}
	plane.nextACL = 100
	// 我们自己留下的孤儿：行 5 已经不在这份配置里了。
	plane.acls[101] = &fakeACL{index: 101, tag: "sr-pol-5", rules: plane.acls[77].rules}

	a := newPolicyApplier(t, plane)
	seg := policySegment(t, []policyRowJSON{policyRowOf(1, policyVPN, 2, "", policyIfA)})
	if err := a.applyPolicy(context.Background(), installedPolicy(seg)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, ok := plane.acls[101]; ok {
		t.Fatalf("孤儿 ACL sr-pol-5 没被清掉: %v", plane.acls)
	}
	if _, ok := plane.acls[77]; !ok {
		t.Fatalf("别人的 ACL 被删了（只该动 sr-pol- 前缀的那些）: %v", plane.acls)
	}
}
