package apply

// What the 6th segment could get wrong without anyone noticing: a VR added but
// never started (it sits in INIT and advertises nothing), a row deleted by a
// parameter set VPP refuses, a "change" applied as an in-place update the box
// cannot address, and an apply that reports success because the API returned 0
// while the dump says otherwise. Each test below is one of those.
//
// The fake plane is not a permissive fake: it answers the way lab-cpe1 answered
// the probe on 2026-11-02 -- duplicate add is refused and leaves the original
// alone, a delete with interval 0 is refused, start is idempotent, and a delete
// needs the live fields echoed back.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"sdwan/controller/internal/vppapi"
)

const (
	vrrpTestPort   = "tap7"
	vrrpTestIndex  = uint32(7)
	vrrpTestPortB  = "tap8"
	vrrpTestIndexB = uint32(8)
)

func vrrpTestIfaces() []vppapi.SwInterface {
	return []vppapi.SwInterface{
		{SWIfIndex: vrrpTestIndex, Name: vrrpTestPort, AdminUp: true, LinkUp: true},
		{SWIfIndex: vrrpTestIndexB, Name: vrrpTestPortB, AdminUp: true, LinkUp: true},
	}
}

// fakeVR is one virtual router as the fake box holds it. start on an up port
// moves INIT -> BACKUP with master_adv filled in; a VR that was only added stays
// in INIT with zero timers (both measured).
type fakeVR struct {
	params vppapi.VRRPParams
	state  vppapi.VRRPState
}

func (v *fakeVR) row() vppapi.VRRPVR {
	return vppapi.VRRPVR{
		SWIfIndex:  v.params.SWIfIndex,
		VRID:       v.params.VRID,
		Priority:   v.params.Priority,
		IntervalCS: v.params.IntervalCS,
		Preempt:    v.params.Preempt,
		Accept:     v.params.Accept,
		Unicast:    v.params.Unicast,
		Addrs:      append([]netip.Addr(nil), v.params.Addrs...),
		State:      v.state,
	}
}

// fakePlane is the box-side seam. calls records every message in order; lastDel
// and lastAdd keep the parameter sets so a test can tell "sent the live fields"
// from "sent the ones it wanted".
type fakePlane struct {
	ifaces []vppapi.SwInterface
	vrs    map[vrKey]*fakeVR
	// phantom holds keys the box claimed to create but has no VR for -- the
	// fault-injection shape of "the API said ok and nothing happened".
	phantom map[vrKey]bool
	calls   []string
	closed  int

	dumpErr      error
	addErr       error
	blackHoleAdd bool

	lastAdd vppapi.VRRPParams
	lastDel vppapi.VRRPParams
}

func newFakePlane(rows ...vppapi.VRRPVR) *fakePlane {
	p := &fakePlane{ifaces: vrrpTestIfaces(), vrs: map[vrKey]*fakeVR{}, phantom: map[vrKey]bool{}}
	for i := range rows {
		v := &rows[i]
		p.vrs[keyOfVR(*v)] = &fakeVR{params: v.Params(), state: v.State}
	}
	return p
}

func (p *fakePlane) DumpInterfaces(context.Context) ([]vppapi.SwInterface, error) {
	p.calls = append(p.calls, "dump-ifaces")
	return p.ifaces, nil
}

func (p *fakePlane) VRRPDump(_ context.Context, swIfIndex uint32) ([]vppapi.VRRPVR, error) {
	p.calls = append(p.calls, fmt.Sprintf("dump(#%d)", swIfIndex))
	if p.dumpErr != nil {
		return nil, p.dumpErr
	}
	keys := make([]vrKey, 0, len(p.vrs))
	for k := range p.vrs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].swIfIndex != keys[j].swIfIndex {
			return keys[i].swIfIndex < keys[j].swIfIndex
		}
		return keys[i].vrID < keys[j].vrID
	})
	var out []vppapi.VRRPVR
	for _, k := range keys {
		// Measured: VPP filters the dump by index, and ~0 answers every VR on the box.
		if swIfIndex != ^uint32(0) && k.swIfIndex != swIfIndex {
			continue
		}
		out = append(out, p.vrs[k].row())
	}
	return out, nil
}

func (p *fakePlane) VRRPAdd(_ context.Context, params vppapi.VRRPParams) error {
	key := vrKey{params.SWIfIndex, params.VRID}
	p.calls = append(p.calls, fmt.Sprintf("add(#%d vr%d prio=%d int=%dcs)", key.swIfIndex, key.vrID, params.Priority, params.IntervalCS))
	p.lastAdd = params
	if params.IntervalCS == 0 {
		return errors.New("retval -7 INVALID_VALUE")
	}
	if _, dup := p.vrs[key]; dup {
		return errors.New("retval -116 ENTRY_ALREADY_EXISTS")
	}
	if p.addErr != nil {
		return p.addErr
	}
	if p.blackHoleAdd {
		p.phantom[key] = true
		return nil
	}
	p.vrs[key] = &fakeVR{params: params, state: vppapi.VRRPStateInit}
	return nil
}

func (p *fakePlane) VRRPDelByKey(_ context.Context, params vppapi.VRRPParams) error {
	key := vrKey{params.SWIfIndex, params.VRID}
	p.calls = append(p.calls, fmt.Sprintf("del(#%d vr%d prio=%d int=%dcs)", key.swIfIndex, key.vrID, params.Priority, params.IntervalCS))
	p.lastDel = params
	if params.IntervalCS == 0 {
		return errors.New("retval -7 INVALID_VALUE")
	}
	if _, ok := p.vrs[key]; !ok {
		return errors.New("retval -6 NO_SUCH_ENTRY")
	}
	delete(p.vrs, key)
	return nil
}

func (p *fakePlane) VRRPStartStop(_ context.Context, swIfIndex uint32, vrID uint8, isStop, isStart bool) error {
	what := "stop"
	if isStart {
		what = "start"
	}
	key := vrKey{swIfIndex, vrID}
	p.calls = append(p.calls, fmt.Sprintf("%s(#%d vr%d)", what, swIfIndex, vrID))
	v, ok := p.vrs[key]
	if !ok {
		if p.phantom[key] {
			return nil
		}
		return errors.New("retval -6 NO_SUCH_ENTRY")
	}
	switch {
	case isStop:
		v.state = vppapi.VRRPStateInit
	case v.state == vppapi.VRRPStateInit:
		v.state = vppapi.VRRPStateBackup
	}
	// Repeating start on a running VR answers ok and changes nothing (measured),
	// which is why the reconciler needs no state check before it.
	return nil
}

func (p *fakePlane) Close() error {
	p.closed++
	return nil
}

func (p *fakePlane) mutations() []string {
	var out []string
	for _, c := range p.calls {
		if strings.HasPrefix(c, "dump") {
			continue
		}
		out = append(out, c)
	}
	return out
}

// vrrpRowJSON is one row of the 6th segment, spelled the way the controller
// renders it so the decoder under test sees production bytes.
type vrrpRowJSON struct {
	VPP        string   `json:"vpp"`
	VRID       uint8    `json:"vr_id"`
	Priority   uint8    `json:"priority"`
	IntervalMS uint16   `json:"interval_ms"`
	Preempt    bool     `json:"preempt,omitempty"`
	Accept     bool     `json:"accept,omitempty"`
	Addrs      []string `json:"addrs"`
}

func vrrpSegment(t *testing.T, rows ...vrrpRowJSON) string {
	t.Helper()
	if rows == nil {
		rows = []vrrpRowJSON{}
	}
	data, err := json.Marshal(map[string]any{"vrrp": rows})
	if err != nil {
		t.Fatalf("segment: %v", err)
	}
	return string(data)
}

func vrrpRowOf(vpp string, vrID, prio uint8, intervalMS uint16, addr string) vrrpRowJSON {
	return vrrpRowJSON{VPP: vpp, VRID: vrID, Priority: prio, IntervalMS: intervalMS, Addrs: []string{addr}}
}

func vrrpVR(index uint32, vrID, prio uint8, intervalCS uint16, preempt bool, state vppapi.VRRPState, addrs ...string) vppapi.VRRPVR {
	v := vppapi.VRRPVR{SWIfIndex: index, VRID: vrID, Priority: prio, IntervalCS: intervalCS, Preempt: preempt, State: state}
	for _, a := range addrs {
		v.Addrs = append(v.Addrs, netip.MustParseAddr(a))
	}
	return v
}

// newVRRPApplier wires the plane seam. The socket it dials is asserted, not
// ignored: applying a 6th segment without --vpp-api is a different failure.
func newVRRPApplier(t *testing.T, plane *fakePlane) *Applier {
	t.Helper()
	a := New(Store{Root: t.TempDir()})
	a.Logf = func(string, ...any) {}
	a.VPPAPI = "/run/vpp/api.sock"
	a.DialVRRP = func(_ context.Context, socket string) (vrrpPlane, error) {
		if socket != "/run/vpp/api.sock" {
			t.Errorf("dialled %q, want the api.sock", socket)
		}
		return plane, nil
	}
	return a
}

func installedVRRP(seg string) Installed {
	return Installed{Boot: "true\n", Vrrp: seg}
}

func TestVRRPSegmentNeedsTheKey(t *testing.T) {
	for _, seg := range []string{`{}`, `{"vrrp":null}`, `[]`, `not json`} {
		if _, err := decodeVRRPSegment(seg); err == nil {
			t.Errorf("decodeVRRPSegment(%s) accepted a segment with no vrrp key", seg)
		}
	}
	rows, err := decodeVRRPSegment(`{"vrrp":[]}`)
	if err != nil || len(rows) != 0 {
		t.Fatalf("the empty list must decode as zero rows: %v %v", rows, err)
	}
}

func TestVRRPApplierWithoutASegmentTouchesNothing(t *testing.T) {
	// A bundle published before VRRP existed carries no 6th segment. It must not
	// dial the API, must not fail, and above all must not delete the VRs that are
	// keeping a site's VIP up right now.
	dialed := false
	a := New(Store{Root: t.TempDir()})
	a.Logf = func(string, ...any) {}
	a.VPPAPI = "/run/vpp/api.sock"
	a.DialVRRP = func(context.Context, string) (vrrpPlane, error) {
		dialed = true
		return nil, errors.New("this test must not connect")
	}
	if err := a.applyVRRP(context.Background(), installedVRRP("")); err != nil {
		t.Fatalf("no segment: %v", err)
	}
	if dialed {
		t.Fatal("dialled VPP for a bundle with no 6th segment")
	}
}

func TestVRRPDryRunDoesNotDial(t *testing.T) {
	plane := newFakePlane()
	a := newVRRPApplier(t, plane)
	a.DryRun = true
	err := a.applyVRRP(context.Background(), installedVRRP(vrrpSegment(t,
		vrrpRowOf(vrrpTestPort, 1, 100, 1000, "192.0.2.1"))))
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(plane.calls) != 0 || plane.closed != 0 {
		t.Fatalf("dry run touched the box: %v", plane.calls)
	}
}

func TestVRRPNeedsTheAPISocket(t *testing.T) {
	a := New(Store{Root: t.TempDir()})
	a.Logf = func(string, ...any) {}
	err := a.applyVRRP(context.Background(), installedVRRP(vrrpSegment(t,
		vrrpRowOf(vrrpTestPort, 1, 100, 1000, "192.0.2.1"))))
	if err == nil || !strings.Contains(err.Error(), "--vpp-api") {
		t.Fatalf("no api.sock = %v, want the flag named in the error", err)
	}
}

func TestVRRPAddsThenStarts(t *testing.T) {
	plane := newFakePlane()
	a := newVRRPApplier(t, plane)
	if err := a.applyVRRP(context.Background(), installedVRRP(vrrpSegment(t,
		vrrpRowOf(vrrpTestPort, 1, 100, 1000, "192.0.2.1")))); err != nil {
		t.Fatalf("apply: %v", err)
	}
	want := []string{"add(#7 vr1 prio=100 int=100cs)", "start(#7 vr1)"}
	if got := plane.mutations(); !equalStrings(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	v := plane.vrs[vrKey{vrrpTestIndex, 1}]
	if v == nil || v.state != vppapi.VRRPStateBackup {
		t.Fatalf("the VR did not end up in the protocol: %+v", v)
	}
	// 1000 ms must reach VPP as 100 centiseconds, not 1000.
	if v.params.IntervalCS != 100 {
		t.Fatalf("interval = %dcs, want 100", v.params.IntervalCS)
	}
	if plane.closed != 1 {
		t.Fatalf("plane closed %d times, want the one deferred Close", plane.closed)
	}
}

func TestVRRPLeavesEqualRowAloneAndStartsInit(t *testing.T) {
	equal := vrrpVR(vrrpTestIndex, 1, 100, 100, false, vppapi.VRRPStateBackup, "192.0.2.1")
	initRow := vrrpVR(vrrpTestIndexB, 2, 100, 100, false, vppapi.VRRPStateInit, "192.0.2.1")
	plane := newFakePlane(equal, initRow)
	a := newVRRPApplier(t, plane)

	seg := vrrpSegment(t,
		vrrpRowOf(vrrpTestPort, 1, 100, 1000, "192.0.2.1"),
		vrrpRowOf(vrrpTestPortB, 2, 100, 1000, "192.0.2.1"))
	if err := a.applyVRRP(context.Background(), installedVRRP(seg)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Re-adding either row would be refused (-116); re-adding the second would
	// also destroy a VR that is only missing its start.
	if got := plane.mutations(); !equalStrings(got, []string{"start(#8 vr2)"}) {
		t.Fatalf("calls = %v, want only the INIT row started", got)
	}
	if plane.vrs[vrKey{vrrpTestIndexB, 2}].state != vppapi.VRRPStateBackup {
		t.Fatal("the INIT row is still in INIT")
	}
}

func TestVRRPRebuildsChangedRowWithTheLiveFields(t *testing.T) {
	live := vrrpVR(vrrpTestIndex, 1, 100, 100, true, vppapi.VRRPStateMaster, "192.0.2.1")
	plane := newFakePlane(live)
	a := newVRRPApplier(t, plane)

	// priority 100 -> 120 and the VIP moves: two rows with the same key, and the
	// only change path the box offers is delete-then-add (in-place update is
	// addressed by vrrp_index, which the dump never returns).
	seg := vrrpSegment(t, vrrpRowJSON{VPP: vrrpTestPort, VRID: 1, Priority: 120,
		IntervalMS: 1000, Addrs: []string{"192.0.2.9"}})
	if err := a.applyVRRP(context.Background(), installedVRRP(seg)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	want := []string{"del(#7 vr1 prio=100 int=100cs)", "add(#7 vr1 prio=120 int=100cs)", "start(#7 vr1)"}
	if got := plane.mutations(); !equalStrings(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	// The delete has to echo what VPP holds. Sending the desired fields instead --
	// or the zero values -- is refused, and then the row never changes.
	if p := plane.lastDel; p.Priority != 100 || p.IntervalCS != 100 || !p.Preempt ||
		len(p.Addrs) != 1 || p.Addrs[0].String() != "192.0.2.1" {
		t.Fatalf("delete sent %+v, want the live VR", p)
	}
	v := plane.vrs[vrKey{vrrpTestIndex, 1}]
	if v == nil || v.params.Priority != 120 || v.params.Addrs[0].String() != "192.0.2.9" {
		t.Fatalf("rebuilt VR = %+v", v)
	}
}

func TestVRRPDeletesRowsTheControllerDoesNotWant(t *testing.T) {
	plane := newFakePlane(
		vrrpVR(vrrpTestIndex, 1, 100, 100, false, vppapi.VRRPStateMaster, "192.0.2.1"),
		vrrpVR(vrrpTestIndexB, 2, 90, 100, false, vppapi.VRRPStateBackup, "192.0.3.1"))
	a := newVRRPApplier(t, plane)
	// An empty list is the instruction "no VR on this box", and VPP lets a running
	// MASTER be deleted without stopping it first (measured).
	if err := a.applyVRRP(context.Background(), installedVRRP(vrrpSegment(t))); err != nil {
		t.Fatalf("apply: %v", err)
	}
	want := []string{"del(#7 vr1 prio=100 int=100cs)", "del(#8 vr2 prio=90 int=100cs)"}
	if got := plane.mutations(); !equalStrings(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if len(plane.vrs) != 0 {
		t.Fatalf("VRs left on the box: %v", plane.vrs)
	}
}

func TestVRRPPlanRefusesWhatVPPWouldMisread(t *testing.T) {
	cases := []struct {
		name string
		seg  string
		want string
	}{
		{"port not on the box",
			vrrpSegment(t, vrrpRowOf("tap4096", 1, 100, 1000, "192.0.2.1")), "这个口现在不存在"},
		{"two rows one key",
			`{"vrrp":[{"vpp":"tap7","vr_id":1,"priority":100,"interval_ms":1000,"addrs":["192.0.2.1"]},` +
				`{"vpp":"tap7","vr_id":1,"priority":90,"interval_ms":1000,"addrs":["192.0.2.2"]}]}`,
			"重复定义"},
		{"half a centisecond", vrrpSegment(t, vrrpRowOf(vrrpTestPort, 1, 100, 1005, "192.0.2.1")), "不是整厘秒"},
		{"zero interval", vrrpSegment(t, vrrpRowOf(vrrpTestPort, 1, 100, 0, "192.0.2.1")), "不是整厘秒"},
		{"vip not an address", vrrpSegment(t, vrrpRowOf(vrrpTestPort, 1, 100, 1000, "192.0.2.300")), "不是合法 IP"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plane := newFakePlane()
			a := newVRRPApplier(t, plane)
			err := a.applyVRRP(context.Background(), installedVRRP(c.seg))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want it to say %q", err, c.want)
			}
			if got := plane.mutations(); len(got) != 0 {
				t.Fatalf("touched the box before refusing the plan: %v", got)
			}
			if plane.closed != 1 {
				t.Fatalf("plane closed %d times, want 1", plane.closed)
			}
		})
	}
}

func TestVRRPRefusesToClaimSuccessTheBoxDidNotDeliver(t *testing.T) {
	// add and start both answer 0 while the VR does not exist. Everything a
	// caller could check by exit code says "done"; only the dump says it is not,
	// and AGENTS.md §2 makes that read-back mandatory.
	plane := newFakePlane()
	plane.blackHoleAdd = true
	a := newVRRPApplier(t, plane)
	err := a.applyVRRP(context.Background(), installedVRRP(vrrpSegment(t,
		vrrpRowOf(vrrpTestPort, 1, 100, 1000, "192.0.2.1"))))
	if err == nil || !strings.Contains(err.Error(), "复检不一致") {
		t.Fatalf("black-hole add = %v, want the read-back to refuse it", err)
	}
	if !strings.Contains(err.Error(), "VPP 里没有这条 VR") {
		t.Fatalf("the error does not name the row: %v", err)
	}
}

func TestVRRPStopsOnTheFirstFailure(t *testing.T) {
	// The convention everywhere else in this package: fail the apply and keep the
	// previous version's VRs. No restart, no second try, no partial success.
	plane := newFakePlane()
	plane.addErr = errors.New("retval -73 INVALID_ARGUMENT")
	a := newVRRPApplier(t, plane)
	err := a.applyVRRP(context.Background(), installedVRRP(vrrpSegment(t,
		vrrpRowOf(vrrpTestPort, 1, 100, 1000, "192.0.2.1"),
		vrrpRowOf(vrrpTestPortB, 2, 100, 1000, "192.0.3.1"))))
	if err == nil || !strings.Contains(err.Error(), "建不出来") {
		t.Fatalf("add failure = %v", err)
	}
	if got := plane.mutations(); !equalStrings(got, []string{"add(#7 vr1 prio=100 int=100cs)"}) {
		t.Fatalf("kept going after the failure: %v", got)
	}
	if plane.closed != 1 {
		t.Fatalf("plane closed %d times, want the one deferred Close", plane.closed)
	}
}

func TestVRRPDumpsOnceBeforeAndOnceAfter(t *testing.T) {
	// The dump answers ~0 with every VR on the box, so there is no reason to walk
	// the interfaces one by one the way ip_address_dump forces.
	plane := newFakePlane()
	a := newVRRPApplier(t, plane)
	if err := a.applyVRRP(context.Background(), installedVRRP(vrrpSegment(t,
		vrrpRowOf(vrrpTestPort, 1, 100, 1000, "192.0.2.1"),
		vrrpRowOf(vrrpTestPortB, 2, 100, 1000, "192.0.3.1")))); err != nil {
		t.Fatalf("apply: %v", err)
	}
	dumps := 0
	for _, c := range plane.calls {
		if c == "dump(#4294967295)" {
			dumps++
		}
	}
	if dumps != 2 {
		t.Fatalf("VR dumps = %d, want exactly one before and one after", dumps)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The order is a safety property, not cosmetics: the VIP is this box's claim to
// be the site's first hop, so it must be the last thing a bundle touches. If an
// earlier segment failed, the peer keeps the address and the customer's first hop
// stays where it was working. This test is what stops someone from "tidying" the
// sequence back into a list where VRRP is first.
func TestApplyReconcilesVRRPAfterEveryOtherSegment(t *testing.T) {
	root := t.TempDir()
	frrConf := filepath.Join(root, "frr.conf")
	fx := &fakeExec{outputs: map[string]string{}}
	a := newTestApplier(t, root, frrConf, fx)

	var lines []string
	a.Logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	plane := newFakePlane()
	a.VPPAPI = "/run/vpp/api.sock"
	a.DialVRRP = func(context.Context, string) (vrrpPlane, error) { return plane, nil }

	b := testBundle()
	b.Artifacts.Vrrp = vrrpSegment(t, vrrpRowOf(vrrpTestPort, 1, 100, 1000, "192.0.2.1"))
	if err := a.Apply(context.Background(), b, false); err != nil {
		t.Fatalf("apply: %v\n%s", err, strings.Join(lines, "\n"))
	}
	lineRange := func(prefix string) (int, int) {
		f, l := -1, -1
		for i, s := range lines {
			if strings.HasPrefix(s, prefix) {
				if f < 0 {
					f = i
				}
				l = i
			}
		}
		return f, l
	}
	vrrpFirst, _ := lineRange("vrrp:")
	if vrrpFirst < 0 {
		t.Fatalf("vrrp never ran: %v", lines)
	}
	for _, earlier := range []string{"overlay: ", "sr-frr: "} {
		_, lastOther := lineRange(earlier)
		if lastOther < 0 {
			t.Fatalf("no %q line in %v", earlier, lines)
		}
		if vrrpFirst < lastOther {
			t.Fatalf("vrrp ran at line %d, %s finished at %d: %v", vrrpFirst, earlier, lastOther, lines)
		}
	}
	if _, ok := plane.vrs[vrKey{vrrpTestIndex, 1}]; !ok {
		t.Fatalf("the VR is not on the box: %v", plane.calls)
	}
}
