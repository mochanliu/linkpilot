// The 6th artifact: the virtual routers this box must run.
//
// Unlike segments 1-5 this is not a file the box keeps. VRRP lives in VPP's
// memory and is configured only through the binary API (the vrrp CLI is
// "unknown input" on this build -- measured 2026-11-02 on lab-cpe1), so what the
// controller sends is a *reconciliation instruction*: the list of VRs that must
// exist on this box, nothing more and nothing less. The box compares VPP's own
// answer (vrrp_vr_dump) against that list and fixes the difference, which is
// also what makes boot replay the same code path as a hot apply.
//
// The reconciliation rules are not invented here; each one is the only shape the
// measured box behaviour allows (tools/vrrp-probe, 2026-11-02, three rounds):
//
//   - The dump answers ~0 with every VR on the box, so one read is enough -- no
//     need to enumerate interfaces the way ip_address_dump forces.
//   - A row is keyed by (sw_if_index, vr_id), and the config carries the port
//     NAME, so the index is resolved from a fresh interface dump every apply: an
//     index is box-local and changes when VPP restarts, which is exactly the
//     event this segment has to survive.
//   - Missing row -> add then start. A VR that was added but never started sits
//     in INIT and advertises nothing, so add without start is not "done".
//   - Same key, same parameters -> do not re-add: add answers
//     ENTRY_ALREADY_EXISTS and leaves the original untouched.
//   - Same key, different parameters -> delete then add. In-place update is
//     addressed only by vrrp_index, which the dump does not return and which VPP
//     recycles for the next VR it creates, so a reconciler cannot name it. The
//     delete needs the live fields echoed back (interval=0 is refused), which is
//     why VRRPVR.Params exists.
//   - Extra rows are deleted with the fields read from the dump; VPP allows
//     deleting a VR that is running, no stop first.
//   - start is idempotent: repeating it on a running VR answers ok and leaves the
//     state alone, so "make sure it is started" needs no state check.
//
// Two consequences for a live site, both stated because AGENTS.md §1.4 requires
// the interruptions to be named rather than discovered:
//
//   - Changing a VR's parameters takes that VR down for one del+add+start cycle,
//     and the new VR has to win the election again. The advertised-interval
//     timers measured on the box put that window at master_down ~= 3x interval
//     when the peer box is present; the real failover/takeover time is measured
//     in #204, not guessed here.
//   - This segment runs LAST, after sr-frr. A box whose config only half landed
//     must not win the VIP: if an earlier segment failed, the peer keeps the
//     address and the customer's first hop stays exactly where it was working.
//
// Nothing here restarts anything, and a failed reconcile is simply a failed
// apply: the previous version's VRs stay as they are, and the error says which
// row did not make it.

package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"sdwan/controller/internal/vppapi"
)

// vrrpPlane is the slice of VPP's API this reconciler uses. It is a seam so the
// ordering and the four cases above can be tested without a VPP; *vppapi.Client
// satisfies it as-is.
type vrrpPlane interface {
	DumpInterfaces(context.Context) ([]vppapi.SwInterface, error)
	VRRPDump(context.Context, uint32) ([]vppapi.VRRPVR, error)
	VRRPAdd(context.Context, vppapi.VRRPParams) error
	VRRPDelByKey(context.Context, vppapi.VRRPParams) error
	VRRPStartStop(context.Context, uint32, uint8, bool, bool) error
	Close() error
}

// vrrpRow is one desired VR as the controller renders it. The port is named, not
// numbered, for the reason in the header: sw_if_index does not survive a VPP
// restart, the name does.
//
// There is no unicast switch. VPP only sends unicast adverts to peers listed by
// vrrp_vr_set_peers, and the basic config has no peer addresses to put in that
// list -- a VR with the unicast bit and no peers advertises to nobody, which is
// worse than not offering the option.
//
// The reconciler compares the dump's `conf` half against these rows, and that
// comparison is trustworthy for a VR that has never been started: measured on the
// probe, a fresh add reads back with its priority, interval and flags intact and
// only the runtime timers zero.
type vrrpRow struct {
	VPP        string   `json:"vpp"`
	VRID       uint8    `json:"vr_id"`
	Priority   uint8    `json:"priority"`
	IntervalMS uint16   `json:"interval_ms"`
	Preempt    bool     `json:"preempt"`
	Accept     bool     `json:"accept"`
	Addrs      []string `json:"addrs"`
}

func decodeVRRPSegment(seg string) ([]vrrpRow, error) {
	var doc struct {
		VRRP *[]vrrpRow `json:"vrrp"`
	}
	if err := json.Unmarshal([]byte(seg), &doc); err != nil {
		return nil, fmt.Errorf("配置包第 6 段不是 JSON 对象: %w", err)
	}
	if doc.VRRP == nil {
		return nil, errors.New("配置包第 6 段没有 vrrp 键（一条 VR 都没有也要发空数组，空数组和「没有这个键」是两件事）")
	}
	return *doc.VRRP, nil
}

// vrKey is VPP's own key for a virtual router.
type vrKey struct {
	swIfIndex uint32
	vrID      uint8
}

func keyOfVR(v vppapi.VRRPVR) vrKey { return vrKey{v.SWIfIndex, v.VRID} }

// desiredVR is one row after its port name has been resolved to an index.
type desiredVR struct {
	label  string
	key    vrKey
	params vppapi.VRRPParams
}

// applyVRRP reconciles VPP's virtual routers with the 6th segment.
func (a *Applier) applyVRRP(ctx context.Context, inst Installed) error {
	if inst.Vrrp == "" {
		a.Logf("vrrp: 这份配置包没有第 6 段，VPP 里的 VR 一条没动")
		return nil
	}
	rows, err := decodeVRRPSegment(inst.Vrrp)
	if err != nil {
		return err
	}
	if a.DryRun {
		a.Logf("vrrp: 会把 %d 条 VR 与 VPP 现状对账（dry-run 不连 api.sock）", len(rows))
		return nil
	}
	plane, err := a.openVRRPPlane(ctx)
	if err != nil {
		return err
	}
	defer plane.Close()

	desired, err := planVRRP(ctx, plane, rows)
	if err != nil {
		return err
	}
	live, err := plane.VRRPDump(ctx, ^uint32(0))
	if err != nil {
		return fmt.Errorf("vrrp: 读 VPP 现有 VR: %w", err)
	}
	liveByKey := make(map[vrKey]vppapi.VRRPVR, len(live))
	for _, v := range live {
		liveByKey[keyOfVR(v)] = v
	}
	desiredByKey := make(map[vrKey]vppapi.VRRPParams, len(desired))
	for _, d := range desired {
		desiredByKey[d.key] = d.params
	}

	for _, d := range desired {
		v, ok := liveByKey[d.key]
		switch {
		case !ok:
			if err := addAndStart(ctx, plane, d); err != nil {
				return err
			}
			a.Logf("vrrp: %s 新建并 start（prio=%d int=%dcs）", d.label, d.params.Priority, d.params.IntervalCS)
		case vrrpParamsEqual(d.params, v.Params()):
			// Already right. Only INIT needs fixing: a VR sitting in INIT has
			// never been started, and start on a running VR is a no-op (measured).
			if v.State == vppapi.VRRPStateInit {
				if err := plane.VRRPStartStop(ctx, d.key.swIfIndex, d.key.vrID, false, true); err != nil {
					return fmt.Errorf("vrrp: %s 已存在但 start 失败: %w", d.label, err)
				}
				a.Logf("vrrp: %s 参数一致，状态 INIT，已补 start", d.label)
				continue
			}
			a.Logf("vrrp: %s 参数一致（状态 %s），不动", d.label, v.State)
		default:
			// The only change path the box offers. Cost: this VR re-elections --
			// see the header.
			if err := plane.VRRPDelByKey(ctx, v.Params()); err != nil {
				return fmt.Errorf("vrrp: %s 想改成 prio=%d int=%dcs，但删掉旧的失败: %w",
					d.label, d.params.Priority, d.params.IntervalCS, err)
			}
			if err := addAndStart(ctx, plane, d); err != nil {
				return fmt.Errorf("vrrp: %s 已删掉旧的，重建失败（这个 VR 现在是断的，下一次 apply 会重试）: %w", d.label, err)
			}
			a.Logf("vrrp: %s 参数变了（%d/%dcs -> %d/%dcs），已删了重建并 start：这个口会重新选一次主备",
				d.label, v.Priority, v.IntervalCS, d.params.Priority, d.params.IntervalCS)
		}
	}
	for _, v := range live {
		if _, want := desiredByKey[keyOfVR(v)]; want {
			continue
		}
		if err := plane.VRRPDelByKey(ctx, v.Params()); err != nil {
			return fmt.Errorf("vrrp: 删掉控制器没配的 VR（口 #%d vr%d）失败: %w", v.SWIfIndex, v.VRID, err)
		}
		a.Logf("vrrp: 口 #%d vr%d 不在控制器这份清单里，已删除", v.SWIfIndex, v.VRID)
	}
	return a.verifyVRRP(ctx, plane, desired)
}

// openVRRPPlane dials the box's VPP api socket. The vrrp CLI does not exist on
// this build, so there is no fallback to vppctl and none is wanted: a box that
// cannot be reached over the API is a box whose VRs were not configured.
func (a *Applier) openVRRPPlane(ctx context.Context) (vrrpPlane, error) {
	if a.VPPAPI == "" {
		return nil, errors.New("第 6 段要配 VRRP，但这个 applier 没有 VPP api.sock（--vpp-api / -vpp-api）：" +
			"vrrp 只有二进制 API，这一版 VPP 的 CLI 全是 unknown input（实测 2026-11-02）")
	}
	plane, err := a.DialVRRP(ctx, a.VPPAPI)
	if err != nil {
		return nil, fmt.Errorf("连 VPP api.sock %s: %w", a.VPPAPI, err)
	}
	return plane, nil
}

// planVRRP turns rows into VRs with real indices: every port name is resolved
// against the interface dump, milliseconds become centiseconds, and two rows
// with the same key are refused instead of silently becoming one.
func planVRRP(ctx context.Context, plane vrrpPlane, rows []vrrpRow) ([]desiredVR, error) {
	var byName map[string]uint32
	if len(rows) > 0 {
		ifs, err := plane.DumpInterfaces(ctx)
		if err != nil {
			return nil, fmt.Errorf("vrrp: 读 VPP 接口清单: %w", err)
		}
		byName = make(map[string]uint32, len(ifs))
		for _, i := range ifs {
			byName[i.Name] = i.SWIfIndex
		}
	}
	out := make([]desiredVR, 0, len(rows))
	seen := map[vrKey]string{}
	for _, r := range rows {
		label := fmt.Sprintf("%s vr%d", r.VPP, r.VRID)
		idx, ok := byName[r.VPP]
		if !ok {
			return nil, fmt.Errorf("vrrp: %s 挂在 VPP 口 %s 上，但这个口现在不存在（第 1/3 段没把它建出来？）", label, r.VPP)
		}
		key := vrKey{idx, r.VRID}
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("vrrp: %s 重复定义（先前那条是 %s）：一个口上的一个 vr_id 只能有一条", label, prev)
		}
		seen[key] = label
		p, err := r.params(idx)
		if err != nil {
			return nil, fmt.Errorf("vrrp: %s: %w", label, err)
		}
		out = append(out, desiredVR{label: label, key: key, params: p})
	}
	return out, nil
}

// params converts one row to what VPP is asked to create. The interval unit is
// the dangerous one: VPP counts centiseconds, so a value that is not a whole
// centisecond would advertise at a rounded rate, and 0 is refused outright
// (measured retval=-7). Both are errors here, not silent conversions.
func (r vrrpRow) params(swIfIndex uint32) (vppapi.VRRPParams, error) {
	if r.IntervalMS == 0 || r.IntervalMS%10 != 0 {
		return vppapi.VRRPParams{}, fmt.Errorf("通告间隔 %d ms 不是整厘秒（VPP 这一层按厘秒计数，0 会被 VPP 直接拒绝）", r.IntervalMS)
	}
	addrs := make([]netip.Addr, 0, len(r.Addrs))
	for _, s := range r.Addrs {
		addr, err := netip.ParseAddr(strings.TrimSpace(s))
		if err != nil {
			return vppapi.VRRPParams{}, fmt.Errorf("虚拟地址「%s」不是合法 IP: %w", s, err)
		}
		addrs = append(addrs, addr)
	}
	return vppapi.VRRPParams{
		SWIfIndex:  swIfIndex,
		VRID:       r.VRID,
		Priority:   r.Priority,
		IntervalCS: uint16(r.IntervalMS / 10),
		Preempt:    r.Preempt,
		Accept:     r.Accept,
		Addrs:      addrs,
	}, nil
}

func addAndStart(ctx context.Context, plane vrrpPlane, d desiredVR) error {
	if err := plane.VRRPAdd(ctx, d.params); err != nil {
		return fmt.Errorf("vrrp: %s 建不出来: %w", d.label, err)
	}
	if err := plane.VRRPStartStop(ctx, d.key.swIfIndex, d.key.vrID, false, true); err != nil {
		return fmt.Errorf("vrrp: %s 建好了但 start 失败（它现在在 INIT，不会发通告）: %w", d.label, err)
	}
	return nil
}

// vrrpParamsEqual compares every field the config owns -- including Unicast,
// which the model never sets, so a VR someone put into unicast by hand is
// rebuilt rather than kept half-configured. Runtime state, the computed timers
// and the virtual MAC are VPP's, not ours.
func vrrpParamsEqual(want, got vppapi.VRRPParams) bool {
	return want.VRID == got.VRID && want.Priority == got.Priority &&
		want.IntervalCS == got.IntervalCS && want.Preempt == got.Preempt &&
		want.Accept == got.Accept && want.Unicast == got.Unicast &&
		addrSetEqual(want.Addrs, got.Addrs)
}

func addrSetEqual(a, b []netip.Addr) bool {
	if len(a) != len(b) {
		return false
	}
	x := addrStrings(a)
	y := addrStrings(b)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func addrStrings(addrs []netip.Addr) []string {
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = a.String()
	}
	sort.Strings(out)
	return out
}

// verifyVRRP reads the box back and refuses to call the segment applied unless
// the dump matches the list -- exit codes are not evidence (AGENTS.md §2).
// Every VR must also have left INIT: "configured" is not "in the protocol".
func (a *Applier) verifyVRRP(ctx context.Context, plane vrrpPlane, desired []desiredVR) error {
	after, err := plane.VRRPDump(ctx, ^uint32(0))
	if err != nil {
		return fmt.Errorf("vrrp: 复检时读不到 VPP: %w", err)
	}
	byKey := make(map[vrKey]vppapi.VRRPVR, len(after))
	for _, v := range after {
		byKey[keyOfVR(v)] = v
	}
	var problems []string
	for _, d := range desired {
		v, ok := byKey[d.key]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s：VPP 里没有这条 VR", d.label))
			continue
		}
		if !vrrpParamsEqual(d.params, v.Params()) {
			problems = append(problems, fmt.Sprintf("%s：VPP 里是 prio=%d int=%dcs，和要配的不一样",
				d.label, v.Priority, v.IntervalCS))
		}
		if v.State == vppapi.VRRPStateInit {
			problems = append(problems, fmt.Sprintf("%s：还停在 INIT（没进协议，不会发通告）", d.label))
		}
		delete(byKey, d.key)
	}
	for _, v := range byKey {
		problems = append(problems, fmt.Sprintf("口 #%d vr%d：控制器这份清单里没有它，但 VPP 里还在", v.SWIfIndex, v.VRID))
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("vrrp: 复检不一致（%d 处）：%s", len(problems), strings.Join(problems, "；"))
	}
	for _, d := range desired {
		a.Logf("vrrp: 复检 %s -> %s", d.label, stateOfVR(after, d.key))
	}
	return nil
}

func stateOfVR(vrs []vppapi.VRRPVR, key vrKey) vppapi.VRRPState {
	for _, v := range vrs {
		if keyOfVR(v) == key {
			return v.State
		}
	}
	return vppapi.VRRPStateInit
}
