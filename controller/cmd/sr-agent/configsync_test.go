package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"sdwan/controller/internal/apply"
)

func syncBundle(nodeID uint16, version int) apply.Bundle {
	return apply.Bundle{
		NodeID:      nodeID,
		Version:     version,
		GeneratedAt: 1700000000,
		Checks:      []apply.Check{{Severity: "info", Text: "ok"}},
		Artifacts: apply.Artifacts{
			FRR: "hostname edge-1\n",
			VPP: strings.Join([]string{
				"set interface ip table host-eth3 10",
				"set interface state host-eth3 up",
				"lcp create host-eth3 host-if lcp-x3",
			}, "\n"),
			Boot: "#!/bin/sh\nset -e\ntrue\n",
		},
	}
}

func bundleBytes(t *testing.T, b apply.Bundle) []byte {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	return raw
}

type fakeExec struct {
	mu      sync.Mutex
	calls   []string
	outputs map[string]string
}

func (f *fakeExec) Run(_ context.Context, name string, args ...string) (string, error) {
	key := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.mu.Unlock()
	return f.outputs[key], nil
}

func (f *fakeExec) count(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

// realApplier returns an applier that touches only t.TempDir() and a fake
// executor, so the test exercises the same code path the box runs.
func realApplier(t *testing.T, root, frrConf string, fx *fakeExec) *apply.Applier {
	t.Helper()
	a := apply.New(apply.Store{Root: root})
	a.FRRConf = frrConf
	a.FRRReload = "true"
	a.Wait = time.Second
	a.Exec = fx
	a.Sleep = func(time.Duration) {}
	a.TAPExists = func(string) bool { return true }
	a.Logf = func(string, ...any) {}
	return a
}

// applyNoForce is the wiring main() uses: the network path never passes --force.
func applyNoForce(a *apply.Applier) func(context.Context, apply.Bundle) error {
	return func(ctx context.Context, b apply.Bundle) error { return a.Apply(ctx, b, false) }
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func syncRoots(t *testing.T) (root, frrConf string) {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "store"), filepath.Join(dir, "frr.conf")
}

func TestConfigSyncAppliesAndReportsTheStoresVerdict(t *testing.T) {
	root, frrConf := syncRoots(t)
	fx := &fakeExec{outputs: map[string]string{}}
	applier := realApplier(t, root, frrConf, fx)
	raw := bundleBytes(t, syncBundle(7, 2))

	var cs *configSync
	fetch := func(context.Context) ([]byte, error) {
		// The heartbeat is attachable mid-apply: it must say "applying v2"
		// rather than claim the old version is still all there is.
		if st := cs.State(); !st.Applying || st.ApplyingVersion != 2 {
			t.Errorf("state during fetch = %+v, want applying v2", st)
		}
		return raw, nil
	}
	cs = newConfigSync(7, apply.Store{Root: root}, fetch, applyNoForce(applier), func(string, ...any) {}, time.Now)

	if st := cs.State(); !st.Capable || st.Version != 0 || st.Error != "" {
		t.Fatalf("initial state = %+v, want capable with no version", st)
	}
	if err := cs.Sync(context.Background(), 2); err != nil {
		t.Fatalf("sync: %v", err)
	}
	st := cs.State()
	if !st.Capable || st.Version != 2 || !st.OK {
		t.Fatalf("state after sync = %+v, want capable v2 ok", st)
	}
	if st.Applying || st.ApplyingVersion != 0 {
		t.Fatalf("applying flags survived sync: %+v", st)
	}
	if st.Source != "apply" || st.AppliedAt == 0 || st.Error != "" {
		t.Fatalf("status not taken from the applier: %+v", st)
	}
	if got, err := os.ReadFile(frrConf); err != nil || string(got) != "hostname edge-1\n" {
		t.Fatalf("frr.conf = %q, %v", got, err)
	}
	if fx.count("lcp create host-eth3 host-if lcp-x3") != 1 {
		t.Fatalf("vpp fragment not applied: %v", fx.calls)
	}

	// A restart of the agent must not forget what is installed: the report is
	// rebuilt from the store, not from memory.
	again := newConfigSync(7, apply.Store{Root: root}, func(context.Context) ([]byte, error) {
		t.Fatal("restarted sync must not fetch before the controller asks")
		return nil, nil
	}, applyNoForce(applier), func(string, ...any) {}, time.Now)
	if st := again.State(); st.Version != 2 || !st.OK || st.Source != "apply" {
		t.Fatalf("state after restart = %+v", st)
	}
}

func TestConfigSyncRefusesForeignBundle(t *testing.T) {
	root, frrConf := syncRoots(t)
	applied := false
	cs := newConfigSync(7, apply.Store{Root: root},
		func(context.Context) ([]byte, error) { return bundleBytes(t, syncBundle(9, 1)), nil },
		func(context.Context, apply.Bundle) error { applied = true; return nil },
		func(string, ...any) {}, time.Now)

	err := cs.Sync(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "属于 node 9") {
		t.Fatalf("err = %v, want foreign-node refusal", err)
	}
	if applied {
		t.Fatal("applier ran on a bundle addressed to another node")
	}
	if st := cs.State(); st.OK || st.Version != 0 || !strings.Contains(st.Error, "属于 node 9") {
		t.Fatalf("state = %+v", st)
	}
	_ = frrConf
}

func TestConfigSyncNeverForcesBlockingErrors(t *testing.T) {
	root, frrConf := syncRoots(t)
	fx := &fakeExec{outputs: map[string]string{}}
	applier := realApplier(t, root, frrConf, fx)

	bad := syncBundle(7, 4)
	bad.Checks = append(bad.Checks, apply.Check{Severity: "error", Text: "还没有客户口"})
	cs := newConfigSync(7, apply.Store{Root: root},
		func(context.Context) ([]byte, error) { return bundleBytes(t, bad), nil },
		applyNoForce(applier), func(string, ...any) {}, time.Now)

	err := cs.Sync(context.Background(), 4)
	if err == nil || !strings.Contains(err.Error(), "refusing to apply") {
		t.Fatalf("err = %v, want applier refusal", err)
	}
	if st := cs.State(); st.OK || !strings.Contains(st.Error, "还没有客户口") {
		t.Fatalf("state = %+v", st)
	}
	if _, err := (apply.Store{Root: root}).Current(); !errors.Is(err, apply.ErrNoConfig) {
		t.Fatalf("store has a current version after a refused bundle: %v", err)
	}
	if len(fx.calls) != 0 {
		t.Fatalf("vppctl ran for a refused bundle: %v", fx.calls)
	}
}

func TestConfigSyncKickSkipsWhatIsAlreadyRunning(t *testing.T) {
	root, frrConf := syncRoots(t)
	applier := realApplier(t, root, frrConf, &fakeExec{outputs: map[string]string{}})
	raw := bundleBytes(t, syncBundle(7, 3))
	fetches := 0
	cs := newConfigSync(7, apply.Store{Root: root},
		func(context.Context) ([]byte, error) { fetches++; return raw, nil },
		applyNoForce(applier), func(string, ...any) {}, time.Now)

	cs.Kick(context.Background(), 0) // nothing published yet
	if cs.State().Applying || fetches != 0 {
		t.Fatal("kick ran with no desired version")
	}
	if err := cs.Sync(context.Background(), 3); err != nil {
		t.Fatalf("sync: %v", err)
	}
	cs.Kick(context.Background(), 3) // already v3 and ok
	time.Sleep(50 * time.Millisecond)
	if fetches != 1 {
		t.Fatalf("kick refetched an applied version: %d fetches", fetches)
	}
}

func TestConfigSyncKickRetriesWithBackoff(t *testing.T) {
	root, _ := syncRoots(t)
	clock := time.Unix(1700000000, 0)
	now := func() time.Time { return clock }

	var mu sync.Mutex
	attempts := 0
	fail := func(context.Context, apply.Bundle) error {
		mu.Lock()
		attempts++
		mu.Unlock()
		return errors.New("boom")
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return attempts
	}
	backoffLeft := func(c *configSync) time.Duration {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.nextTry.Sub(clock)
	}

	cs := newConfigSync(7, apply.Store{Root: root},
		func(context.Context) ([]byte, error) { return bundleBytes(t, syncBundle(7, 2)), nil },
		fail, func(string, ...any) {}, now)

	cs.Kick(context.Background(), 2)
	waitUntil(t, "first attempt", func() bool { return count() == 1 })
	if got := backoffLeft(cs); got != 5*time.Second {
		t.Fatalf("backoff after first failure = %s, want 5s", got)
	}

	// Inside the window the heartbeat must not start another apply: a box that
	// fails to install would otherwise restart FRR on every heartbeat.
	cs.Kick(context.Background(), 2)
	time.Sleep(50 * time.Millisecond)
	if count() != 1 {
		t.Fatalf("retried inside backoff window: %d attempts", count())
	}

	clock = clock.Add(5 * time.Second)
	cs.Kick(context.Background(), 2)
	waitUntil(t, "second attempt", func() bool { return count() == 2 })
	if got := backoffLeft(cs); got != 10*time.Second {
		t.Fatalf("backoff after second failure = %s, want 10s", got)
	}
	if st := cs.State(); st.OK || !strings.Contains(st.Error, "boom") || st.Applying {
		t.Fatalf("state after failures = %+v", st)
	}
}

// A device started without -apply must still answer the heartbeat: silence is
// indistinguishable from an old agent, and the console would render it as
// unknown instead of "this agent does not install configs".
func TestAgentStatusReportsCapabilityWithoutApply(t *testing.T) {
	st := &agentStatus{}
	if c := st.configReport(); c == nil || c.Capable {
		t.Fatalf("report without -apply = %+v, want capable:false", c)
	}

	root, _ := syncRoots(t)
	store := apply.Store{Root: root}
	if _, err := store.Install(syncBundle(7, 1)); err != nil {
		t.Fatalf("install: %v", err)
	}
	st.cfg = newConfigSync(7, store, nil, nil, func(string, ...any) {}, time.Now)
	c := st.configReport()
	if c == nil || !c.Capable || c.Version != 1 {
		t.Fatalf("report with -apply = %+v, want capable v1", c)
	}
}

func TestConfigSyncReportsStatusPointerMismatch(t *testing.T) {
	root, _ := syncRoots(t)
	store := apply.Store{Root: root}
	inst, err := store.Install(syncBundle(7, 1))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	// A status file for a version the pointer does not name: an install that
	// never finished, or a hand-edited store.
	if err := store.RecordStatus(apply.Status{Version: 2, OK: true, Source: "apply"}); err != nil {
		t.Fatalf("record status: %v", err)
	}
	cs := newConfigSync(7, store, func(context.Context) ([]byte, error) { return nil, nil },
		func(context.Context, apply.Bundle) error { return nil }, func(string, ...any) {}, time.Now)

	st := cs.State()
	if st.Version != inst.Manifest.Version || st.OK {
		t.Fatalf("state = %+v, want v1 not ok", st)
	}
	if !strings.Contains(st.Error, "status.json 记录的是 v2") || !strings.Contains(st.Error, "指向 v1") {
		t.Fatalf("mismatch not reported: %q", st.Error)
	}
}
