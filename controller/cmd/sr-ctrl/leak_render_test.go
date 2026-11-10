package main

// Tests for the render half of 路由泄露 (L-2, docs/o6e-leak-plan.md §4/§5.2):
// the SDWAN-CPE FRR triple and bundle 第 5 段 srfrr.
//
// The properties pinned here are the ones a wrong line would silently break:
// an empty whitelist must mean "全放过" (not "deny everything"), a VPN with no
// row must contribute nothing, and a list edit must change the frozen artifact
// even though nobody saved a draft.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func frrBlock(frr, header string) string {
	lines := strings.Split(frr, "\n")
	var out []string
	in := false
	for _, l := range lines {
		if l == header {
			in = true
			out = append(out, l)
			continue
		}
		if !in {
			continue
		}
		if l == "!" || l == "" || !strings.HasPrefix(l, " ") {
			break
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func TestRenderFRRLeakToCPE(t *testing.T) {
	cfg := BasicConfig{
		Hostname: "edge-1",
		VPNs: []ConfigVPN{
			{ID: 0, Name: "default",
				BGP:  BGPConfig{On: true, ASN: 65101, RouterID: "10.0.0.1"},
				OSPF: OSPFConfig{On: true, RouterID: "10.0.0.1"}},
			{ID: 100, Name: "guest",
				BGP:  BGPConfig{On: true, ASN: 65101, RouterID: "10.0.0.1"},
				OSPF: OSPFConfig{On: true, RouterID: "10.0.0.1"}},
		},
	}
	rules := []leakRenderRule{
		{VPN: 100, Protocols: []string{"bgp", "ospf"}, HasList: true,
			Entries: []PrefixEntry{
				{Prefix: "10.0.0.0/8", GE: 8, LE: 24},
				{Prefix: "192.168.0.0/16"},
			}},
		{VPN: 0, Protocols: []string{"bgp"}},
	}
	frr := renderFRR(&cfg, rules)

	for _, want := range []string{
		"ip prefix-list SDWAN2CPE-vpn100 seq 5 permit 10.0.0.0/8 ge 8 le 24",
		"ip prefix-list SDWAN2CPE-vpn100 seq 10 permit 192.168.0.0/16",
		"route-map SDWAN2CPE-vpn100 permit 10\n match ip address prefix-list SDWAN2CPE-vpn100",
		"route-map SDWAN2CPE-vpn0 permit 10\n!",
	} {
		if !strings.Contains(frr, want) {
			t.Errorf("FRR artifact missing %q\n%s", want, frr)
		}
	}
	// The redistribute line belongs inside the selected instance only.
	if b := frrBlock(frr, "router bgp 65101"); !strings.Contains(b, "  redistribute kernel route-map SDWAN2CPE-vpn0") {
		t.Errorf("default-VRF BGP block has no kernel redistribute:\n%s", b)
	}
	if b := frrBlock(frr, "router bgp 65101 vrf vpn100"); !strings.Contains(b, "  redistribute kernel route-map SDWAN2CPE-vpn100") {
		t.Errorf("VPN 100 BGP block has no kernel redistribute:\n%s", b)
	}
	if b := frrBlock(frr, "router ospf vrf vpn100"); !strings.Contains(b, " redistribute kernel route-map SDWAN2CPE-vpn100") {
		t.Errorf("VPN 100 OSPF block has no kernel redistribute:\n%s", b)
	}
	// VPN 0 only selected BGP: its OSPF must stay untouched.
	if b := frrBlock(frr, "router ospf"); strings.Contains(b, "redistribute") {
		t.Errorf("default-VRF OSPF block gained a redistribute it was not asked for:\n%s", b)
	}
}

func TestRenderFRRLeakNothingIsNothing(t *testing.T) {
	cfg := baseValidConfig()
	frr := renderFRR(&cfg, nil)
	if strings.Contains(frr, "SDWAN2CPE") || strings.Contains(frr, "redistribute kernel") {
		t.Fatalf("no leak rows still produced leak lines:\n%s", frr)
	}
	// The old placeholder told readers the overlay would put these here via a
	// table that is being retired; it must not come back.
	if strings.Contains(frr, "table-direct 1250") || strings.Contains(frr, "CUST-IN") {
		t.Fatalf("legacy 1250 placeholder is still in the artifact:\n%s", frr)
	}

	// An empty (but selected) list means 全放过 per §3.1. Emitting the match would
	// point at an empty FRR prefix-list, which matches nothing.
	empty := renderFRR(&cfg, []leakRenderRule{{VPN: 100, Protocols: []string{"bgp"}, HasList: true}})
	if strings.Contains(empty, "ip prefix-list SDWAN2CPE") {
		t.Fatalf("empty list produced prefix-list entries:\n%s", empty)
	}
	if strings.Contains(empty, "match ip address prefix-list") {
		t.Fatalf("empty list produced a match line (would deny everything):\n%s", empty)
	}
	if !strings.Contains(empty, "route-map SDWAN2CPE-vpn100 permit 10") {
		t.Fatalf("empty list still needs its route-map:\n%s", empty)
	}
}

func TestSrfrrArtifactExpandsLibraryAndFollowsEdits(t *testing.T) {
	s, ten, _ := leakFixture(t)
	all := leakGet(t, s, ten).PrefixLists[0].ID

	putRow := func(direction string, vpn uint32, protocols []string, list string) {
		t.Helper()
		b, _ := json.Marshal(leakRowRequest{
			NodeID: 1, Direction: direction, VPN: vpn, Protocols: protocols, PrefixList: list,
		})
		if rec := leakDo(t, s, http.MethodPut, "/api/leak/rows", string(b), ten); rec.Code != http.StatusOK {
			t.Fatalf("save %s vpn %d = %d (%s)", direction, vpn, rec.Code, rec.Body.String())
		}
	}
	putRow(leakToCPE, 0, []string{"bgp", "ospf"}, "")
	putRow(leakToSDWAN, 10, []string{"bgp"}, all)

	dc := s.state.Configs[configKey("t", 1)]
	art := s.artifactsLocked("t", 1, &dc.Draft, dc.Leak, dc.Policy)
	var got srfrrArtifact
	if err := json.Unmarshal([]byte(art.Srfrr), &got); err != nil {
		t.Fatalf("srfrr artifact is not JSON: %v\n%s", err, art.Srfrr)
	}
	if len(got.ExportVRFs) != 1 || got.ExportVRFs[0] != 0 {
		t.Fatalf("export_vrfs = %v, want [0]", got.ExportVRFs)
	}
	if len(got.ImportRules) != 1 {
		t.Fatalf("import_rules = %+v, want the VPN 10 rule", got.ImportRules)
	}
	r := got.ImportRules[0]
	if r.VPN != 10 || strings.Join(r.Sources, ",") != "bgp" ||
		len(r.Prefixes) != 1 || r.Prefixes[0].Prefix != "0.0.0.0/0" {
		t.Fatalf("import rule = %+v, want vpn 10 bgp from ALL", r)
	}
	if !strings.Contains(art.FRR, "redistribute kernel route-map SDWAN2CPE-vpn0") ||
		!strings.Contains(art.FRR, "route-map SDWAN2CPE-vpn0 permit 10") {
		t.Fatalf("FRR artifact missing the VPN 0 leak lines:\n%s", art.FRR)
	}

	// Changing the list content with no draft save must change the frozen bundle.
	body, _ := json.Marshal(prefixListRequest{
		ID: all, Name: "ALL", Entries: []PrefixEntry{{Prefix: "10.0.0.0/8"}},
	})
	if rec := leakDo(t, s, http.MethodPut, "/api/prefix-lists", string(body), ten); rec.Code != http.StatusOK {
		t.Fatalf("edit list = %d (%s)", rec.Code, rec.Body.String())
	}
	edited := s.artifactsLocked("t", 1, &dc.Draft, dc.Leak, dc.Policy)
	if edited == art {
		t.Fatal("editing the prefix-list did not change the artifacts")
	}
	if !strings.Contains(edited.Srfrr, `"prefix": "10.0.0.0/8"`) {
		t.Fatalf("edited entries did not reach the srfrr artifact:\n%s", edited.Srfrr)
	}

	again := s.artifactsLocked("t", 1, &dc.Draft, dc.Leak, dc.Policy)
	if again != edited {
		t.Fatal("artifacts not stable across two renders")
	}

	now := int64(100)
	if !s.publishConfigLocked("t", 1, dc, nil, "alice", now) {
		t.Fatal("first publish reported no change")
	}
	v1 := dc.Published.Version
	if s.publishConfigLocked("t", 1, dc, nil, "alice", now) {
		t.Fatal("identical republish reported a change")
	}
	body, _ = json.Marshal(prefixListRequest{
		ID: all, Name: "ALL", Entries: []PrefixEntry{{Prefix: "10.0.0.0/8", LE: 24}},
	})
	if rec := leakDo(t, s, http.MethodPut, "/api/prefix-lists", string(body), ten); rec.Code != http.StatusOK {
		t.Fatalf("second edit = %d (%s)", rec.Code, rec.Body.String())
	}
	if !s.publishConfigLocked("t", 1, dc, nil, "alice", now) {
		t.Fatal("list-only change was not published")
	}
	if dc.Published.Version != v1+1 {
		t.Fatalf("published version = %d, want %d (one bump for the list edit)", dc.Published.Version, v1+1)
	}

	// Removing every row must still produce a document that says "nothing", so
	// sr-apply can turn the box's previous leak off.
	dc.Leak = LeakSet{}
	empty := s.artifactsLocked("t", 1, &dc.Draft, dc.Leak, dc.Policy)
	want := "{\n  \"export_vrfs\": [],\n  \"import_rules\": []\n}\n"
	if empty.Srfrr != want {
		t.Fatalf("empty srfrr artifact = %q, want %q", empty.Srfrr, want)
	}
	if strings.Contains(empty.FRR, "SDWAN2CPE") {
		t.Fatalf("empty leak set still rendered FRR leak lines:\n%s", empty.FRR)
	}
}
