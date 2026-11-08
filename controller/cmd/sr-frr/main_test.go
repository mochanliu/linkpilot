package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
