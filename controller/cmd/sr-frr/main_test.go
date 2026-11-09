package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"sdwan/controller/internal/frr"
)

// Slice A #276 covers the reporter that decides between the full
// /local-prefixes and the delta /local-prefixes/delta body. Same posture as
// bgpd's zebra session: full on first success and after any delta that failed
// (bad op / invalid ADD), delta in between.

func rep(vpn uint8, p string) frr.ReportedPrefix {
	return frr.ReportedPrefix{VPN: vpn, Prefix: netip.MustParsePrefix(p)}
}

func TestDiffLocalPrefixes(t *testing.T) {
	prev := map[frr.ImportKey]struct{}{
		{VPN: 5, Prefix: netip.MustParsePrefix("10.10.0.0/16")}: {},
		{VPN: 5, Prefix: netip.MustParsePrefix("10.20.0.0/16")}: {},
	}
	cur := map[frr.ImportKey]struct{}{
		{VPN: 5, Prefix: netip.MustParsePrefix("10.20.0.0/16")}: {},
		{VPN: 5, Prefix: netip.MustParsePrefix("10.30.0.0/16")}: {},
		{VPN: 7, Prefix: netip.MustParsePrefix("172.16.0.0/16")}: {},
	}
	got := diffLocalPrefixes(prev, cur)
	want := []localPrefixDeltaJSON{
		{VRFID: 5, Prefix: "10.10.0.0/16", Op: "del"},
		{VRFID: 5, Prefix: "10.30.0.0/16", Op: "add"},
		{VRFID: 7, Prefix: "172.16.0.0/16", Op: "add"},
	}
	if len(got) != len(want) {
		t.Fatalf("delta = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delta[%d] = %+v, want %+v (whole=%+v)", i, got[i], want[i], got)
		}
	}
}

// A reporter's first reconcile is always a whole-table PUT; a second with the
// same set is a no-op; a third that changed sends delta; a delta that failed
// on the sr-route side (4xx) demotes to full on the next call.
func TestLocalReporterStateMachine(t *testing.T) {
	var fullCount, deltaCount int32
	var lastDelta []localPrefixDeltaJSON
	var failDelta atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/local-prefixes":
			atomic.AddInt32(&fullCount, 1)
			w.WriteHeader(http.StatusOK)
		case "/local-prefixes/delta":
			if failDelta.Load() {
				http.Error(w, "unconfigured vrf", http.StatusBadRequest)
				return
			}
			atomic.AddInt32(&deltaCount, 1)
			var doc localPrefixDeltaDoc
			_ = json.NewDecoder(r.Body).Decode(&doc)
			lastDelta = doc.Deltas
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	reporter := &localReporter{
		hc:       srv.Client(),
		fullURL:  srv.URL + "/local-prefixes",
		deltaURL: srv.URL + "/local-prefixes/delta",
	}
	ctx := context.Background()

	// First reconcile: full.
	posted, err := reporter.reconcile(ctx, []frr.ReportedPrefix{rep(5, "10.10.0.0/16")})
	if err != nil || !posted || fullCount != 1 || deltaCount != 0 {
		t.Fatalf("first: posted=%v err=%v full=%d delta=%d", posted, err, fullCount, deltaCount)
	}
	if reporter.hasSync() != true {
		t.Fatal("reporter should be synced after successful full")
	}

	// Same set again: no post at all.
	posted, err = reporter.reconcile(ctx, []frr.ReportedPrefix{rep(5, "10.10.0.0/16")})
	if err != nil || posted || fullCount != 1 || deltaCount != 0 {
		t.Fatalf("equal set: posted=%v err=%v full=%d delta=%d", posted, err, fullCount, deltaCount)
	}

	// Changed set: one delta with the diff.
	posted, err = reporter.reconcile(ctx, []frr.ReportedPrefix{rep(5, "10.10.0.0/16"), rep(5, "10.30.0.0/16")})
	if err != nil || !posted || fullCount != 1 || deltaCount != 1 {
		t.Fatalf("delta: posted=%v err=%v full=%d delta=%d", posted, err, fullCount, deltaCount)
	}
	if len(lastDelta) != 1 || lastDelta[0].Op != "add" || lastDelta[0].Prefix != "10.30.0.0/16" {
		t.Fatalf("delta body = %+v, want one add 10.30/16", lastDelta)
	}

	// Delta now fails → demote to unsynced.
	failDelta.Store(true)
	posted, err = reporter.reconcile(ctx, []frr.ReportedPrefix{rep(5, "10.30.0.0/16")})
	if err == nil || posted {
		t.Fatalf("failed delta should surface err=%v posted=%v", err, posted)
	}
	if reporter.hasSync() {
		t.Fatal("failed delta should demote synced to false")
	}

	// Restore delta success but reporter still has to go full first.
	failDelta.Store(false)
	posted, err = reporter.reconcile(ctx, []frr.ReportedPrefix{rep(5, "10.30.0.0/16")})
	if err != nil || !posted || fullCount != 2 {
		t.Fatalf("re-sync after failure: posted=%v err=%v full=%d", posted, err, fullCount)
	}
	if !strings.Contains(reporter.fullURL, "/local-prefixes") {
		t.Fatalf("full URL should target the whole-table endpoint: %s", reporter.fullURL)
	}
}

// fakeZserv opens a ZAPI socket that only accepts. zebraConnected() asks
// nothing more than "is there a connection that isn't dead", and the pull
// endpoint gates on exactly that -- the replay content is the importer's
// problem, covered in internal/frr.
func fakeZserv(t *testing.T) string {
	t.Helper()
	// Short directory on purpose: a Unix socket's name is capped (108 on Linux,
	// 104 on macOS) and t.TempDir() carries the test name into the path.
	dir, err := os.MkdirTemp("/tmp", "z")
	if err != nil {
		t.Fatalf("临时目录: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "z.api")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("监听 %s: %v", path, err)
	}
	t.Cleanup(func() { ln.Close() })
	held := make(chan net.Conn, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Held open, never closed and never answered: closing it is what a
			// dead connection looks like to Client.Err().
			held <- c
		}
	}()
	t.Cleanup(func() {
		for {
			select {
			case c := <-held:
				_ = c.Close()
			default:
				return
			}
		}
	})
	return path
}

// B4: sr-route 重启后来控制口拉一份现值。三条都要成立：没连上 zebra 时不许
// 拿一份没法信的空集走人；应答的内容必须是 importer 的现值（与下一次 PUT 同
// 一口径）；应答一次必须把 reporter 降级，让后面那一次推送是全量而不是差量。
func TestControlLocalPrefixesPull(t *testing.T) {
	rule := frr.ImportRule{
		VPN:     5,
		Sources: []uint8{frr.RouteBGP},
		Allow:   []frr.AllowEntry{{Prefix: netip.MustParsePrefix("10.20.0.0/16")}},
	}
	rt := &runtimeStore{cfg: runtimeConfig{rules: []frr.ImportRule{rule}}}
	exp := &exportState{}
	st := &importState{}
	imp := frr.NewImporter(func(id uint32) (uint8, bool) {
		if id == 7 {
			return 5, true
		}
		return 0, id == 0
	})
	imp.Handle(frr.RouteEvent{Add: true, VRFID: 7, Type: frr.RouteBGP,
		Prefix: netip.MustParsePrefix("10.20.0.0/16")})

	var fullCount, deltaCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/local-prefixes":
			atomic.AddInt32(&fullCount, 1)
			w.WriteHeader(http.StatusOK)
		case "/local-prefixes/delta":
			atomic.AddInt32(&deltaCount, 1)
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	reporter := &localReporter{
		hc:       srv.Client(),
		fullURL:  srv.URL + "/local-prefixes",
		deltaURL: srv.URL + "/local-prefixes/delta",
	}
	reps := []frr.ReportedPrefix{rep(5, "10.20.0.0/16")}
	// 先把 reporter 推到"sr-route 认为自己持有这一份"的状态：这正是 sr-route
	// 重启以后两边各说各话的那个起点。
	if _, err := reporter.reconcile(ctx, reps); err != nil {
		t.Fatalf("seed reconcile: %v", err)
	}
	if !reporter.hasSync() || fullCount != 1 {
		t.Fatalf("seed: synced=%v full=%d", reporter.hasSync(), fullCount)
	}

	h := controlMux(rt, exp, st, imp, reporter, &fileConfig{}, ctx)

	// 1) 规则在、zebra 没连上：现值无从谈起，必须是 503，且不许顺手把
	//    reporter 降级（没人拿到任何东西，降级只会换来一次无意义的全表 PUT）。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/local-prefixes", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未就绪时 GET = %d，想要 503：%s", rec.Code, rec.Body.String())
	}
	if !reporter.hasSync() {
		t.Fatal("503 不该降级 reporter")
	}
	if fullCount != 1 || deltaCount != 0 {
		t.Fatalf("503 不该触发任何推送：full=%d delta=%d", fullCount, deltaCount)
	}

	// 2) 连上以后答现值，并且把 reporter 降级。
	cl, err := frr.Dial(ctx, fakeZserv(t))
	if err != nil {
		t.Fatalf("连假 zebra: %v", err)
	}
	st.mu.Lock()
	st.cl = cl
	st.clSig = "fake"
	st.mu.Unlock()
	t.Cleanup(func() { _ = cl.Close() })

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/local-prefixes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("就绪后 GET = %d：%s", rec.Code, rec.Body.String())
	}
	var doc localPrefixesDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("解码应答: %v\n%s", err, rec.Body.String())
	}
	if len(doc.Prefixes) != 1 || doc.Prefixes[0].VRFID != 5 || doc.Prefixes[0].Prefix != "10.20.0.0/16" {
		t.Fatalf("应答 = %+v，想要 vpn5 的 10.20.0.0/16", doc.Prefixes)
	}
	if reporter.hasSync() {
		t.Fatal("应答过一次就该降级：reporter 不能再宣称 sr-route 持有 lastSeen 那一份")
	}

	// 3) 降级之后，同一份集合的下一次 reconcile 必须走全表 PUT，而不是"差量为零、
	//    什么都不发"。这一条就是 B4 的缺口本身：sr-route 重启后集合没变，
	//    不降级就永远没有人再说话。
	posted, err := reporter.reconcile(ctx, reps)
	if err != nil {
		t.Fatalf("reconcile after pull: %v", err)
	}
	if !posted || fullCount != 2 || deltaCount != 0 {
		t.Fatalf("拉取后应重发全量：posted=%v full=%d delta=%d", posted, fullCount, deltaCount)
	}
	if !reporter.hasSync() {
		t.Fatal("全量 PUT 成功以后应回到 synced")
	}
}

// 没有导入规则时，空集是可信的现值（不是"还没学到"），必须答 200，
// 否则 sr-route 的开机拉取会对一台不泄露的盒子永远重试。
func TestControlLocalPrefixesNoRules(t *testing.T) {
	rt := &runtimeStore{cfg: runtimeConfig{}}
	exp := &exportState{}
	st := &importState{}
	imp := frr.NewImporter(nil)
	reporter := &localReporter{
		hc:       httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })).Client(),
		fullURL:  "http://127.0.0.1:1/local-prefixes",
		deltaURL: "http://127.0.0.1:1/local-prefixes/delta",
	}
	h := controlMux(rt, exp, st, imp, reporter, &fileConfig{}, context.Background())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/local-prefixes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("无规则时 GET = %d：%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"prefixes":[]}` {
		t.Fatalf("应答 = %s，想要一个明确为空而不是 null 的数组", body)
	}
}
