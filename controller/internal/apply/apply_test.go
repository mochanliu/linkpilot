package apply

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
}

func (f *fakeExec) Run(_ context.Context, name string, args ...string) (string, error) {
	key := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, key)
	return f.outputs[key], nil
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

	// VPP fragment first, then the kernel fragment, then the FRR restart.
	bootAt := fx.index("sh " + filepath.Join(root, "versions", "3", "boot.sh"))
	if bootAt < 0 {
		t.Fatalf("boot fragment not executed: %v", fx.calls)
	}
	if fx.index("ip table add 10") > bootAt {
		t.Fatalf("VPP fragment must run before boot: %v", fx.calls)
	}
	if fx.index("sh -c true") < bootAt {
		t.Fatalf("FRR reload must run after boot: %v", fx.calls)
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
	if fx.count("sh -c true") != 0 {
		t.Fatalf("boot restarted FRR without --reload-frr: %v", fx.calls)
	}
	st, err := Store{Root: root}.ReadStatus()
	if err != nil || !st.OK || st.Source != "boot" {
		t.Fatalf("boot status = %+v, %v", st, err)
	}

	if err := a.Boot(context.Background(), true); err != nil {
		t.Fatalf("boot --reload-frr: %v", err)
	}
	if fx.count("sh -c true") != 1 {
		t.Fatalf("reload-frr did not restart FRR: %v", fx.calls)
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
	}
	for out, want := range cases {
		if got := vppError(out) != ""; got != want {
			t.Errorf("vppError(%q) = %v, want %v", out, got, want)
		}
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
