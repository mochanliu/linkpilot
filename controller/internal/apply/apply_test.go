package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testBundle() Bundle {
	return Bundle{
		NodeID:  7,
		Version: 3,
		Checks:  []Check{{Severity: "info", Text: "ok"}},
		Artifacts: Artifacts{
			FRR: "hostname edge-1\n",
			VPP: strings.Join([]string{
				"# comment is skipped",
				"ip table add 10",
				"set interface ip table host-eth3 10",
				"set interface mtu packet 1500 host-eth3",
				"set interface state host-eth3 up",
				"lcp create host-eth3 host-if lcp-x3",
				"lcp lcp-sync on",
			}, "\n"),
			Boot: "#!/bin/sh\nset -e\ntrue\n",
		},
	}
}

func newTestApplier(t *testing.T, root, frrConf string, fx *fakeExec) *Applier {
	t.Helper()
	a := New(Store{Root: root})
	a.FRRConf = frrConf
	a.FRRReload = "true"
	a.Wait = time.Second
	a.Exec = fx
	a.Sleep = func(time.Duration) {}
	a.TAPExists = func(string) bool { return true }
	a.Logf = func(string, ...any) {}
	return a
}

type fakeExec struct {
	calls   []string
	outputs map[string]string
	fail    map[string]error
}

func (f *fakeExec) Run(_ context.Context, name string, args ...string) (string, error) {
	key := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, key)
	return f.outputs[key], f.fail[key]
}

func (f *fakeExec) index(substr string) int {
	for i, c := range f.calls {
		if strings.Contains(c, substr) {
			return i
		}
	}
	return -1
}

func (f *fakeExec) count(substr string) int {
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

func TestDecodeBundle(t *testing.T) {
	if _, err := DecodeBundle(strings.NewReader(`{"version":0,"artifacts":{"boot":"x"}}`)); err == nil {
		t.Fatal("version 0 must be rejected")
	}
	if _, err := DecodeBundle(strings.NewReader(`{"version":1,"artifacts":{"frr":"x"}}`)); err == nil {
		t.Fatal("missing boot fragment must be rejected")
	}
	b, err := DecodeBundle(strings.NewReader(`{"version":2,"artifacts":{"boot":"x"},"checks":[
		{"severity":"warning","text":"w"},{"severity":"error","text":"e"}]}`))
	if err != nil {
		t.Fatalf("valid bundle rejected: %v", err)
	}
	if errs := b.BlockingErrors(); len(errs) != 1 || errs[0] != "e" {
		t.Fatalf("BlockingErrors = %v, want [e]", errs)
	}
}

func TestStoreInstallAndCurrent(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	if _, err := store.Current(); !errors.Is(err, ErrNoConfig) {
		t.Fatalf("empty store = %v, want ErrNoConfig", err)
	}

	b := testBundle()
	inst, err := store.Install(b)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if inst.Manifest.Version != 3 || inst.VPP != b.Artifacts.VPP {
		t.Fatalf("installed = %+v", inst.Manifest)
	}
	cur, err := store.Current()
	if err != nil || cur.Manifest.Version != 3 {
		t.Fatalf("current = %+v, %v", cur.Manifest, err)
	}
	for _, name := range []string{"manifest.json", "frr.conf", "vpp.conf", "boot.sh"} {
		if _, err := os.Stat(filepath.Join(root, "versions", "3", name)); err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
	}

	// Same version and identical content is a no-op (boot may replay after a
	// reboot that happened between install and apply).
	again, err := store.Install(b)
	if err != nil || again.Manifest.InstalledAt != inst.Manifest.InstalledAt {
		t.Fatalf("reinstall identical: %+v, %v", again.Manifest, err)
	}

	// Same version, different content must not silently overwrite.
	b.Artifacts.VPP += "ip table add 11\n"
	if _, err := store.Install(b); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("collision = %v, want different-content error", err)
	}

	// A truncated artifact must fail the checksum instead of being replayed.
	if err := os.WriteFile(filepath.Join(root, "versions", "3", "boot.sh"), []byte("rm -rf /"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Current(); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("corrupted file = %v, want checksum error", err)
	}
}

func TestApplyOrderAndIdempotency(t *testing.T) {
	root := t.TempDir()
	frrConf := filepath.Join(t.TempDir(), "frr.conf")
	fx := &fakeExec{outputs: map[string]string{}}
	a := newTestApplier(t, root, frrConf, fx)

	if err := a.Apply(context.Background(), testBundle(), false); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// VPP fragment first, then the kernel fragment, then the FRR diff.
	bootAt := fx.index("sh " + filepath.Join(root, "versions", "3", "boot.sh"))
	if bootAt < 0 {
		t.Fatalf("boot fragment not executed: %v", fx.calls)
	}
	if fx.index("ip table add 10") > bootAt {
		t.Fatalf("VPP fragment must run before boot: %v", fx.calls)
	}
	if fx.index("true --reload") < bootAt {
		t.Fatalf("FRR diff must run after boot: %v", fx.calls)
	}
	if n := fx.count("true --test"); n != 1 {
		t.Fatalf("FRR diff was not re-checked exactly once (%d): %v", n, fx.calls)
	}
	if fx.count("# comment") != 0 {
		t.Fatalf("comments executed: %v", fx.calls)
	}
	got, err := os.ReadFile(frrConf)
	if err != nil || string(got) != "hostname edge-1\n" {
		t.Fatalf("frr.conf = %q, %v", got, err)
	}
	st, err := Store{Root: root}.ReadStatus()
	if err != nil || !st.OK || st.Version != 3 || st.Source != "apply" {
		t.Fatalf("status = %+v, %v", st, err)
	}

	// Second apply: VPP already has the pair and the table entry, so those two
	// commands are skipped instead of erroring.
	fx2 := &fakeExec{outputs: map[string]string{
		"vppctl show lcp":                         "lcp lcp-sync on\nitf-pair: [0] host-eth3 tap4096 lcp-x3 14 type tap\n",
		"vppctl show interface address host-eth3": "host-eth3 (up):\n  L3 10.68.2.1/24 ip4 table-id 10 fib-idx 1\n",
	}}
	a2 := newTestApplier(t, root, frrConf, fx2)
	if err := a2.Apply(context.Background(), testBundle(), false); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if n := fx2.count("lcp create"); n != 0 {
		t.Fatalf("lcp create re-ran: %v", fx2.calls)
	}
	if n := fx2.count("set interface ip table"); n != 0 {
		t.Fatalf("set interface ip table re-ran: %v", fx2.calls)
	}
	// MTU/state are idempotent in VPP and are still sent.
	if fx2.count("set interface mtu packet 1500 host-eth3") != 1 {
		t.Fatalf("mtu not re-applied: %v", fx2.calls)
	}
}

func TestApplyRefusesBlockingChecks(t *testing.T) {
	root := t.TempDir()
	b := testBundle()
	b.Checks = append(b.Checks, Check{Severity: "error", Text: "eBGP 邻居不在直连网段"})
	fx := &fakeExec{outputs: map[string]string{}}
	a := newTestApplier(t, root, filepath.Join(t.TempDir(), "frr.conf"), fx)

	err := a.Apply(context.Background(), b, false)
	if err == nil || !strings.Contains(err.Error(), "blocking error") {
		t.Fatalf("apply = %v, want blocking error", err)
	}
	if len(fx.calls) != 0 {
		t.Fatalf("nothing may run when checks block: %v", fx.calls)
	}
	if _, err := (Store{Root: root}).Current(); !errors.Is(err, ErrNoConfig) {
		t.Fatalf("blocked apply installed something: %v", err)
	}

	// --force is the documented escape hatch.
	if err := a.Apply(context.Background(), b, true); err != nil {
		t.Fatalf("forced apply: %v", err)
	}
}

// The apply is judged by the state after it, not by the reload's exit status:
// frr-reload.py can exit 0 and still leave commands unapplied.
func TestApplyFRRRefusesLeftoverDelta(t *testing.T) {
	root := t.TempDir()
	frrConf := filepath.Join(t.TempDir(), "frr.conf")
	dir := filepath.Dir(frrConf)
	fx := &fakeExec{outputs: map[string]string{
		"true --reload " + frrConf + " --stdout --confdir " + dir: "INFO: Applied\n",
		"true --test " + frrConf + " --stdout --confdir " + dir:   "INFO: Loading\n\nLines To Add\n============\nrouter bgp 65101\n",
	}}
	a := newTestApplier(t, root, frrConf, fx)

	err := a.Apply(context.Background(), testBundle(), false)
	if err == nil || !strings.Contains(err.Error(), "仍有差异") {
		t.Fatalf("apply = %v, want leftover-delta error", err)
	}
	if !strings.Contains(err.Error(), "router bgp 65101") {
		t.Fatalf("error must name the leftover command: %v", err)
	}
	if st, _ := (Store{Root: root}).ReadStatus(); st.OK {
		t.Fatalf("status claims ok after a failed diff: %+v", st)
	}
}

// A failed diff fails the apply. There is no restart-fallback: restarting FRR
// resets customer BGP sessions (AGENTS.md §1).
func TestApplyFRRFailureDoesNotRestart(t *testing.T) {
	root := t.TempDir()
	frrConf := filepath.Join(t.TempDir(), "frr.conf")
	key := "true --reload " + frrConf + " --stdout --confdir " + filepath.Dir(frrConf)
	fx := &fakeExec{
		outputs: map[string]string{key: "ERROR: vtysh returned status 1"},
		fail:    map[string]error{key: errors.New("exit status 1")},
	}
	a := newTestApplier(t, root, frrConf, fx)

	err := a.Apply(context.Background(), testBundle(), false)
	if err == nil || !strings.Contains(err.Error(), "差量热更失败") {
		t.Fatalf("apply = %v, want diff failure", err)
	}
	for _, c := range fx.calls {
		if strings.Contains(c, "restart") || strings.Contains(c, "systemctl") || strings.Contains(c, "frrinit") {
			t.Fatalf("applier fell back to a restart: %v", fx.calls)
		}
	}
}

func TestFRRDeltaParsing(t *testing.T) {
	clean := "2026-10-31 23:29:21,035  INFO: Loading Config object from vtysh show running\n"
	if got := frrDelta(clean); got != "" {
		t.Fatalf("frrDelta(clean) = %q", got)
	}
	withDelta := clean + "\nLines To Delete\n===============\nrouter bgp 65101\n no neighbor 10.0.0.2\n\nLines To Add\n============\nhostname edge-2\n"
	got := frrDelta(withDelta)
	if !strings.Contains(got, "no neighbor 10.0.0.2") || !strings.Contains(got, "hostname edge-2") {
		t.Fatalf("frrDelta = %q", got)
	}
}

func TestBootReplaysWithoutController(t *testing.T) {
	root := t.TempDir()
	frrConf := filepath.Join(t.TempDir(), "frr.conf")
	if _, err := (Store{Root: root}).Install(testBundle()); err != nil {
		t.Fatal(err)
	}

	fx := &fakeExec{outputs: map[string]string{}}
	a := newTestApplier(t, root, frrConf, fx)
	// The FRR config is already on disk from the last apply; boot must not
	// restart FRR unless asked.
	if err := os.WriteFile(frrConf, []byte("hostname edge-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Boot(context.Background(), false); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if fx.index("ip table add 10") < 0 || fx.index("sh "+filepath.Join(root, "versions", "3", "boot.sh")) < 0 {
		t.Fatalf("boot did not replay VPP+kernel: %v", fx.calls)
	}
	if fx.count("true --reload") != 0 || fx.count("true --test") != 0 {
		t.Fatalf("boot touched FRR without --frr-diff: %v", fx.calls)
	}
	st, err := Store{Root: root}.ReadStatus()
	if err != nil || !st.OK || st.Source != "boot" {
		t.Fatalf("boot status = %+v, %v", st, err)
	}

	if err := a.Boot(context.Background(), true); err != nil {
		t.Fatalf("boot --frr-diff: %v", err)
	}
	if fx.count("true --reload") != 1 || fx.count("true --test") != 1 {
		t.Fatalf("--frr-diff did not run one diff and one check: %v", fx.calls)
	}
}

func TestBootWithoutInstalledVersion(t *testing.T) {
	a := newTestApplier(t, t.TempDir(), filepath.Join(t.TempDir(), "frr.conf"), &fakeExec{outputs: map[string]string{}})
	if err := a.Boot(context.Background(), false); !errors.Is(err, ErrNoConfig) {
		t.Fatalf("boot = %v, want ErrNoConfig", err)
	}
}

func TestVPPErrorDetection(t *testing.T) {
	cases := map[string]bool{
		"":                                 false,
		"set interface mtu host-eth3 1500": false,
		"show: unknown input `frobnicate'": true,
		"set interface state: unknown interface `nope'":   true,
		"lcp create: linux-cp pair creation failed (-81)": true,
		// `show interface` 的计数器不是错误：这行的 error 是计数器名字。
		"tx-error 3":      false,
		"drops 87":        false,
		"rx packets 6928": false,
		"ip4 6772":        false,
		// 但带 `<命令>: ` 前缀的才是真的失败 —— 哪怕末列也是数字。
		"ip route add: failed 999": true,
	}
	for out, want := range cases {
		if got := vppError(out) != ""; got != want {
			t.Errorf("vppError(%q) = %v, want %v", out, got, want)
		}
	}
}

// TestVPPErrorIgnoresInterfaceCounters 锁住 2026-11-10 在 VM 台 CPE2 上踩到的那次
// 事故：客户口只要有过一次 tx error，`show interface` 就多出一行 `tx-error 3`，
// 旧实现把整段输出判成"读接口失败"，apply 在第 1 段就中止 —— 策略段一条都没跑，
// 而设备回执里只有那张计数表，看着像"应用成功"。
func TestVPPErrorIgnoresInterfaceCounters(t *testing.T) {
	// 与 CPE2 那次实测输出同形（去掉列宽无所谓，判定只看内容）。
	out := `              Name               Idx    State  MTU (L3/IP4/IP6/MPLS)     Counter          Count
host-enp0s8                       1      up          1500/0/0/0     rx packets                  6928
                                                                    rx bytes                  594312
                                                                    tx packets                  3569
                                                                    tx bytes                  303302
                                                                    drops                         87
                                                                    ip4                         6772
                                                                    ip6                           36
                                                                    tx-error                       3`
	if msg := vppError(out); msg != "" {
		t.Fatalf("vppError 把正常的接口计数表判成了错误：%q", msg)
	}
	// 同一张表里混进真的 CLI 错误时必须认出来。
	if msg := vppError(out + "\nshow interface: unknown input `host-enp0s8x'"); msg == "" {
		t.Fatal("vppError 漏掉了表尾真正的 unknown input")
	}
}

func TestArtifactCommandsSkipsCommentsAndBlanks(t *testing.T) {
	got := artifactCommands("# a\n\n  # b\nip table add 10\n")
	if len(got) != 1 || got[0] != "ip table add 10" {
		t.Fatalf("artifactCommands = %v", got)
	}
}

func TestStatusJSONShape(t *testing.T) {
	// Keep the on-disk status readable by jq and by the console later.
	data, err := json.Marshal(Status{Version: 1, OK: true, AppliedAt: 2, Source: "apply"})
	if err != nil || !strings.Contains(string(data), `"source":"apply"`) {
		t.Fatalf("status json = %s, %v", data, err)
	}
}

// A daemon started by the command inherits stdout. Capturing into a pipe would
// wait for an EOF that only arrives when the daemon exits; frrinit.sh does
// exactly this with watchfrr.
func TestExecRunnerDoesNotWaitForInheritedStdout(t *testing.T) {
	start := time.Now()
	out, err := execRunner{}.Run(context.Background(), "sh", "-c", "sleep 10 & echo started")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(out) != "started" {
		t.Fatalf("out = %q", out)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("run blocked on inherited stdout for %s", d)
	}
}

func TestExecRunnerReturnsExitErrorWithOutput(t *testing.T) {
	out, err := execRunner{}.Run(context.Background(), "sh", "-c", "echo boom; echo detail >&2; exit 7")
	if err == nil {
		t.Fatal("want an exit error")
	}
	if !strings.Contains(out, "boom") || !strings.Contains(out, "detail") {
		t.Fatalf("out = %q", out)
	}
}

// Every segment has to survive the store: Install writes it to a file and read()
// hands it back to the boot replayer. Both are lists someone maintains by hand,
// and the controller lost 第 5 段 and 第 6 段 that way, so this test enumerates the
// two structs by field name instead of listing the segments. A segment added to
// Artifacts without a matching Installed field -- or one the store forgets to
// write, or forgets to compare -- fails here, not on a box that silently never
// gets told about it.
func TestStoreRoundTripsEverySegment(t *testing.T) {
	store := Store{Root: t.TempDir()}

	artT := reflect.TypeOf(Artifacts{})
	instT := reflect.TypeOf(Installed{})
	names := make([]string, 0, artT.NumField())
	for i := 0; i < artT.NumField(); i++ {
		f := artT.Field(i)
		if f.Type.Kind() != reflect.String {
			t.Fatalf("Artifacts.%s is %s, not a string", f.Name, f.Type.Kind())
		}
		if _, ok := instT.FieldByName(f.Name); !ok {
			t.Fatalf("Installed has no %s field: this segment cannot reach the boot replayer", f.Name)
		}
		names = append(names, f.Name)
	}

	b := testBundle()
	av := reflect.ValueOf(&b.Artifacts).Elem()
	for i, name := range names {
		av.Field(i).SetString(fmt.Sprintf("segment %d of %s\n", i+1, name))
	}
	if _, err := store.Install(b); err != nil {
		t.Fatalf("install: %v", err)
	}
	cur, err := store.Current()
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	rv := reflect.ValueOf(cur)
	for i, name := range names {
		if got := rv.FieldByName(name).String(); got != av.Field(i).String() {
			t.Errorf("%s: wrote %q, the store reads back %q", name, av.Field(i).String(), got)
		}
	}

	// And each one is part of the version's identity: changing only that segment
	// must be refused as "same version, different content", or editing one segment
	// would silently overwrite what the box is running.
	for i, name := range names {
		mutated := b
		mav := reflect.ValueOf(&mutated.Artifacts).Elem()
		mav.Field(i).SetString(av.Field(i).String() + "extra\n")
		if _, err := store.Install(mutated); err == nil || !strings.Contains(err.Error(), "different content") {
			t.Errorf("%s: reinstall with only this segment changed = %v, want different-content refusal", name, err)
		}
	}
}

// -------------------------------------------------- 缺陷③：重接管用脏 MAC
//
// 现场（exp-286 §5.5，CPE1）：lcp-sync 把 tap 的随机 MAC 反写进内核 netdev，
// VPP 重启后重接管信了内核现值（ca:a3:…），VPP 里的口和烧录值（08:00:27:…）
// 不一致 —— 对端 ARP FAILED、OSPF 卡 ExStart，而"接管成功"的日志一切正常。
//
// 修法落在产品路径上：`lcp create host-<netdev>` 之前先保证 af_packet 口存在，
// 建它时 hw-addr 取 ethtool -P 的烧录值；内核 netdev 被写脏过时另外显式改回
// （hw-addr 只管 VPP 侧与 lcp TAP，管不到内核 —— 2026-11-10 现场实测，见 apply.go）。

func hostIfaceFragment() string {
	return "# 客户口\nlcp create host-eth5 host-if lcp-x5\n"
}

func TestHostIfaceCreatedWithPermanentMAC(t *testing.T) {
	fx := &fakeExec{outputs: map[string]string{
		"vppctl show lcp":                 "",
		"vppctl show interface host-eth5": "unknown interface 'host-eth5'\n",
		"ethtool -P eth5":                 "Permanent address: 08:00:27:9f:f9:1f\n",
	}}
	a := newTestApplier(t, t.TempDir(), filepath.Join(t.TempDir(), "frr.conf"), fx)
	if err := a.runVPP(context.Background(), hostIfaceFragment()); err != nil {
		t.Fatalf("runVPP: %v", err)
	}
	want := "vppctl create host-interface name eth5 hw-addr 08:00:27:9f:f9:1f"
	if n := fx.count(want); n != 1 {
		t.Fatalf("create host-interface 次数 = %d, want 1（calls=%v）", n, fx.calls)
	}
	if fx.count("ip link set dev eth5 allmulticast on") != 1 {
		t.Fatalf("接管前没打开 allmulticast（calls=%v）", fx.calls)
	}
	// 顺序：先建口、后配对。
	if !(fx.index(want) < fx.index("vppctl lcp create host-eth5 host-if lcp-x5")) {
		t.Fatalf("先配了对再建口（calls=%v）", fx.calls)
	}
}

// 内核 netdev 被 lcp-sync 写脏：改 MAC 的三步必须发出去，而且发生在建口之前。
// 现场证据（2026-11-10 CPE2，VPP 26.06）：用 hw-addr 建出来的口和 lcp TAP 都拿到了
// 烧录值，但 /sys/class/net/enp0s8/address 还是脏的 02:de:ad:be:ef:01 —— hw-addr 不改
// 内核那一侧。而脏值会让下一个接管者直接拒绝接管（lab 的 underlay.sh 就是这么干的），
// 于是这台盒子要人手工修一次才能再被接管。所以这一步不能省。
func TestHostIfaceRepairsDirtyKernelMAC(t *testing.T) {
	fx := &fakeExec{outputs: map[string]string{
		"vppctl show lcp":                 "",
		"vppctl show interface host-eth5": "unknown interface 'host-eth5'\n",
		"ethtool -P eth5":                 "Permanent address: 08:00:27:9f:f9:1f\n",
	}}
	a := newTestApplier(t, t.TempDir(), filepath.Join(t.TempDir(), "frr.conf"), fx)
	var logs []string
	a.Logf = func(f string, args ...any) { logs = append(logs, fmt.Sprintf(f, args...)) }
	a.NetdevMAC = func(string) (string, error) { return "ca:a3:04:12:a7:f8", nil }
	if err := a.runVPP(context.Background(), hostIfaceFragment()); err != nil {
		t.Fatalf("runVPP: %v", err)
	}
	for _, want := range []string{
		"ip link set dev eth5 down",
		"ip link set dev eth5 address 08:00:27:9f:f9:1f",
	} {
		if n := fx.count(want); n != 1 {
			t.Fatalf("%q 次数 = %d, want 1（calls=%v）", want, n, fx.calls)
		}
	}
	// 顺序：down → address → 建口。在 up 的口上内核会拒绝改地址，所以 down 必须在前面；
	// 建口放在改完之后，这样 hw-addr 与内核两侧是同一个值。
	down, addr := fx.index("ip link set dev eth5 down"),
		fx.index("ip link set dev eth5 address 08:00:27:9f:f9:1f")
	create := fx.index("vppctl create host-interface name eth5 hw-addr 08:00:27:9f:f9:1f")
	if !(down < addr && addr < create) {
		t.Fatalf("改 MAC 的顺序不对：down=%d address=%d create=%d（calls=%v）", down, addr, create, fx.calls)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "写脏") {
		t.Fatalf("改 MAC 没说明原因：%q", strings.Join(logs, "\n"))
	}
}

// 内核 MAC 本来就是烧录值（没被写脏过的盒子）：不发改 MAC 的命令 —— 每次收敛轮
// 都 down/up 一下网口是没有必要的抖动。大小写不同不算脏。
func TestHostIfaceCleanKernelMACIsNotRewritten(t *testing.T) {
	fx := &fakeExec{outputs: map[string]string{
		"vppctl show lcp":                 "",
		"vppctl show interface host-eth5": "unknown interface 'host-eth5'\n",
		"ethtool -P eth5":                 "Permanent address: 08:00:27:9f:f9:1f\n",
	}}
	a := newTestApplier(t, t.TempDir(), filepath.Join(t.TempDir(), "frr.conf"), fx)
	a.NetdevMAC = func(string) (string, error) { return "08:00:27:9F:F9:1F", nil }
	if err := a.runVPP(context.Background(), hostIfaceFragment()); err != nil {
		t.Fatalf("runVPP: %v", err)
	}
	if n := fx.count("ip link set dev eth5 address"); n != 0 {
		t.Fatalf("MAC 本来就是烧录值还去改：%d 次（calls=%v）", n, fx.calls)
	}
	if n := fx.count("ip link set dev eth5 down"); n != 0 {
		t.Fatalf("不该把干净的口 down 一下：%d 次（calls=%v）", n, fx.calls)
	}
	if n := fx.count("vppctl create host-interface name eth5 hw-addr 08:00:27:9f:f9:1f"); n != 1 {
		t.Fatalf("口还是要建的（calls=%v）", fx.calls)
	}
}

// 口已经在 VPP 里（正常收敛轮）：不多做任何写动作。
func TestHostIfacePresentSkipsCreation(t *testing.T) {
	fx := &fakeExec{outputs: map[string]string{
		"vppctl show lcp":                 "",
		"vppctl show interface host-eth5": "host-eth5 (up):\n  Ethernet address 08:00:27:9f:f9:1f\n",
	}}
	a := newTestApplier(t, t.TempDir(), filepath.Join(t.TempDir(), "frr.conf"), fx)
	if err := a.runVPP(context.Background(), hostIfaceFragment()); err != nil {
		t.Fatalf("runVPP: %v", err)
	}
	if n := fx.count("create host-interface"); n != 0 {
		t.Fatalf("口已经在了还建：%d 次（calls=%v）", n, fx.calls)
	}
	if fx.count("vppctl lcp create host-eth5 host-if lcp-x5") != 1 {
		t.Fatalf("配对没做（calls=%v）", fx.calls)
	}
}

// ethtool -P 读不到时回落内核现值，但回落必须说出来：那条路正是脏 MAC 的形状。
func TestHostIfaceFallsBackToKernelMACLoudly(t *testing.T) {
	fx := &fakeExec{outputs: map[string]string{
		"vppctl show lcp":                 "",
		"vppctl show interface host-eth5": "unknown interface 'host-eth5'\n",
		"ethtool -P eth5":                 "Permanent address: \n", // 驱动不报
	}}
	a := newTestApplier(t, t.TempDir(), filepath.Join(t.TempDir(), "frr.conf"), fx)
	var logs []string
	a.Logf = func(f string, args ...any) { logs = append(logs, fmt.Sprintf(f, args...)) }
	a.NetdevMAC = func(string) (string, error) { return "ca:a3:04:12:a7:f8", nil }
	if err := a.runVPP(context.Background(), hostIfaceFragment()); err != nil {
		t.Fatalf("runVPP: %v", err)
	}
	if n := fx.count("vppctl create host-interface name eth5 hw-addr ca:a3:04:12:a7:f8"); n != 1 {
		t.Fatalf("回落路径没把内核现值写进 hw-addr（calls=%v）", fx.calls)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "回落") || !strings.Contains(joined, "脏 MAC") {
		t.Fatalf("回落没被点名：%q", joined)
	}
}

// 两个来源都读不到（口名写错就是这种）：失败要响，不能建一个 MAC 不明的口。
func TestHostIfaceUnknownNetdevFails(t *testing.T) {
	fx := &fakeExec{outputs: map[string]string{
		"vppctl show lcp":                 "",
		"vppctl show interface host-eth9": "unknown interface 'host-eth9'\n",
	}}
	a := newTestApplier(t, t.TempDir(), filepath.Join(t.TempDir(), "frr.conf"), fx)
	a.NetdevMAC = func(string) (string, error) { return "", errors.New("no such file") }
	err := a.runVPP(context.Background(), "lcp create host-eth9 host-if lcp-x9\n")
	if err == nil || !strings.Contains(err.Error(), "MAC") {
		t.Fatalf("MAC 读不到时 = %v, want 报错", err)
	}
}

func TestPermanentMACParsing(t *testing.T) {
	cases := []struct {
		out  string
		want string
		ok   bool
	}{
		{"Permanent address: 08:00:27:9f:f9:1f\n", "08:00:27:9f:f9:1f", true},
		{"Permanent address: 08:00:27:9F:F9:1F", "08:00:27:9f:f9:1f", true},
		{"Permanent address: \n", "", false},
		{"", "", false},
		{"netlink error: Operation not supported", "", false},
	}
	for _, c := range cases {
		got, ok := permanentMACFrom(c.out)
		if got != c.want || ok != c.ok {
			t.Fatalf("%q → %q,%v; want %q,%v", c.out, got, ok, c.want, c.ok)
		}
	}
}
