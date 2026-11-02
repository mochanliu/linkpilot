package apply

// The two things a wrong 5th segment would do silently: overwrite a box-owned
// key in /etc/sr-frr.json, or report "applied" while the running sr-frr still
// holds the old table. The tests below pin both, plus the touch set that makes
// a newly added protocol actually take effect on already-injected routes.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sdwan/controller/internal/frr"
)

func TestDecodeSrfrrRequiresBothKeys(t *testing.T) {
	for _, seg := range []string{
		`{}`,
		`{"export_vrfs":[0]}`,
		`{"import_rules":[]}`,
		`{"export_vrfs":null,"import_rules":[]}`,
		`[]`,
	} {
		if _, err := decodeSrfrr(seg); err == nil {
			t.Errorf("decodeSrfrr(%s) accepted a segment without both keys", seg)
		}
	}
	seg, err := decodeSrfrr(`{"export_vrfs":[],"import_rules":[]}`)
	if err != nil || string(seg.exportRaw) != "[]" || string(seg.importRaw) != "[]" {
		t.Fatalf("empty arrays rejected: %+v %v", seg, err)
	}
}

func TestMergeSrfrrOwnsTwoKeysOnly(t *testing.T) {
	base := map[string]json.RawMessage{
		"status_url":   json.RawMessage(`"http://127.0.0.1:9080/local-prefixes"`),
		"zserv_socket": json.RawMessage(`"/run/frr/zserv.api"`),
		"state_file":   json.RawMessage(`"/var/lib/sr-frr/state.json"`),
		"poll_sec":     json.RawMessage(`5`),
		"export_vrfs":  json.RawMessage(`[0]`),
		"import_rules": json.RawMessage(`[]`),
		"vrfs":         json.RawMessage(`[0]`),
		"import":       json.RawMessage(`{"bgp":true}`),
		"distance":     json.RawMessage(`200`),
		"metric":       json.RawMessage(`10`),
	}
	seg, err := decodeSrfrr(`{"export_vrfs":[0,100],"import_rules":[
		{"vpn":100,"sources":["bgp"],"prefixes":[{"prefix":"10.0.0.0/8","le":24}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := mergeSrfrr(base, seg)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("merged file is not JSON: %v", err)
	}
	for _, k := range []string{"status_url", "zserv_socket", "state_file", "poll_sec"} {
		if string(got[k]) != string(base[k]) {
			t.Errorf("box key %s changed: %s", k, got[k])
		}
	}
	for _, k := range srfrrLegacyKeys {
		if _, ok := got[k]; ok {
			t.Errorf("legacy key %s survived the merge", k)
		}
	}
	var vrfs []uint32
	if err := json.Unmarshal(got["export_vrfs"], &vrfs); err != nil || !reflect.DeepEqual(vrfs, []uint32{0, 100}) {
		t.Errorf("export_vrfs = %s (%v)", got["export_vrfs"], err)
	}
	if !strings.Contains(string(got["import_rules"]), "10.0.0.0/8") {
		t.Errorf("import_rules = %s", got["import_rules"])
	}
}

// srfrrServer answers PUT /config on a Unix socket. reply returns the status
// and body; the default reply computes the hash sr-frr would.
func srfrrServer(t *testing.T, reply func(req map[string]json.RawMessage) (int, string)) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "control.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/config" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		var req map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		code, body := reply(req)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func goodReply(t *testing.T, seen *map[string]json.RawMessage) func(map[string]json.RawMessage) (int, string) {
	return func(req map[string]json.RawMessage) (int, string) {
		if seen != nil {
			*seen = req
		}
		h, err := frr.ConfigHash(req["export_vrfs"], req["import_rules"])
		if err != nil {
			return http.StatusBadRequest, err.Error()
		}
		out, _ := json.Marshal(map[string]string{"config_hash": h})
		return http.StatusOK, string(out)
	}
}

func newSrfrrApplier(t *testing.T, conf, sock string) *Applier {
	t.Helper()
	a := New(Store{Root: t.TempDir()})
	a.SrfrrConf = conf
	a.SrfrrSock = sock
	a.Logf = func(string, ...any) {}
	return a
}

func writeSrfrrBase(t *testing.T, conf, body string) []byte {
	t.Helper()
	if err := os.WriteFile(conf, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return []byte(body)
}

func TestApplySrfrrPushesBeforeWriting(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "sr-frr.json")
	orig := writeSrfrrBase(t, conf, `{
  "status_url": "http://127.0.0.1:9080/local-prefixes",
  "control_socket": "/etc/sr-frr/does-not-matter.sock",
  "export_vrfs": [0],
  "import_rules": []
}
`)
	var seen map[string]json.RawMessage
	sock := srfrrServer(t, goodReply(t, &seen))

	inst := Installed{Srfrr: `{"export_vrfs":[0,100],"import_rules":[
		{"vpn":100,"sources":["bgp","ospf"],"prefixes":[]}]}`}
	if err := newSrfrrApplier(t, conf, sock).applySrfrr(context.Background(), inst, Installed{}, true); err != nil {
		t.Fatalf("applySrfrr: %v", err)
	}

	if !reflect.DeepEqual(seen["refresh_vrfs"], json.RawMessage(`[]`)) {
		t.Errorf("refresh_vrfs = %s, want [] (first export is not touched)", seen["refresh_vrfs"])
	}
	var onBox map[string]json.RawMessage
	raw, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &onBox); err != nil {
		t.Fatal(err)
	}
	if string(onBox["status_url"]) != `"http://127.0.0.1:9080/local-prefixes"` {
		t.Errorf("status_url lost: %s", onBox["status_url"])
	}
	var vrfs []uint32
	if err := json.Unmarshal(onBox["export_vrfs"], &vrfs); err != nil || !reflect.DeepEqual(vrfs, []uint32{0, 100}) {
		t.Errorf("export_vrfs = %s (%v)", onBox["export_vrfs"], err)
	}
	if len(raw) == len(orig) && string(raw) == string(orig) {
		t.Fatal("file was not rewritten")
	}
}

func TestApplySrfrrRefusesMismatchedHashAndKeepsFile(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "sr-frr.json")
	orig := writeSrfrrBase(t, conf, `{"status_url":"http://x","export_vrfs":[0],"import_rules":[]}`)
	sock := srfrrServer(t, func(map[string]json.RawMessage) (int, string) {
		return http.StatusOK, `{"config_hash":"0000"}`
	})

	inst := Installed{Srfrr: `{"export_vrfs":[0,100],"import_rules":[]}`}
	err := newSrfrrApplier(t, conf, sock).applySrfrr(context.Background(), inst, Installed{}, true)
	if err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("hash mismatch = %v, want an error", err)
	}
	if now, _ := os.ReadFile(conf); string(now) != string(orig) {
		t.Fatalf("file changed despite a rejected push:\n%s", now)
	}
}

func TestApplySrfrrRefusesFailedPutAndKeepsFile(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "sr-frr.json")
	orig := writeSrfrrBase(t, conf, `{"status_url":"http://x","export_vrfs":[0],"import_rules":[]}`)
	sock := srfrrServer(t, func(map[string]json.RawMessage) (int, string) {
		return http.StatusInternalServerError, `{"error":"bad config"}`
	})

	inst := Installed{Srfrr: `{"export_vrfs":[],"import_rules":[]}`}
	err := newSrfrrApplier(t, conf, sock).applySrfrr(context.Background(), inst, Installed{}, true)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("failed PUT = %v, want an error carrying the status", err)
	}
	if now, _ := os.ReadFile(conf); string(now) != string(orig) {
		t.Fatalf("file changed despite a failed push:\n%s", now)
	}
}

func TestApplySrfrrBootWritesWithoutPush(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "sr-frr.json")
	writeSrfrrBase(t, conf, `{"status_url":"http://x","export_vrfs":[],"import_rules":[]}`)
	// Nothing listening: a boot replay that pushed would fail here.
	inst := Installed{Srfrr: `{"export_vrfs":[0],"import_rules":[]}`}
	if err := newSrfrrApplier(t, conf, filepath.Join(t.TempDir(), "absent.sock")).
		applySrfrr(context.Background(), inst, inst, false); err != nil {
		t.Fatalf("boot replay pushed: %v", err)
	}
	var onBox map[string]json.RawMessage
	raw, _ := os.ReadFile(conf)
	_ = json.Unmarshal(raw, &onBox)
	var vrfs []uint32
	if err := json.Unmarshal(onBox["export_vrfs"], &vrfs); err != nil || !reflect.DeepEqual(vrfs, []uint32{0}) {
		t.Errorf("boot did not write the artifact: %s", raw)
	}
}

func TestApplySrfrrMissingBaseFile(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "sr-frr.json")
	inst := Installed{Srfrr: `{"export_vrfs":[],"import_rules":[]}`}
	err := newSrfrrApplier(t, conf, "").applySrfrr(context.Background(), inst, Installed{}, false)
	if err == nil || !strings.Contains(err.Error(), "status_url") {
		t.Fatalf("missing base file = %v, want the box-key explanation", err)
	}
}

const frrLeakBase = `router bgp 65101
  redistribute kernel route-map SDWAN2CPE-vpn0
!
router bgp 65101 vrf vpn100
  redistribute kernel route-map SDWAN2CPE-vpn100
!
`

func TestExportProtocolSets(t *testing.T) {
	got := exportProtocolSets(frrLeakBase + `router ospf vrf vpn100
 redistribute kernel route-map SDWAN2CPE-vpn100
!
router ospf
 redistribute connected
!
`)
	want := map[uint32]string{0: "bgp", 100: "bgp+ospf"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exportProtocolSets = %v, want %v", got, want)
	}
	// A policy line outside any router block is not an instance.
	if got := exportProtocolSets(" redistribute kernel route-map SDWAN2CPE-vpn7\n"); len(got) != 0 {
		t.Fatalf("indented orphan counted: %v", got)
	}
}

func TestRefreshVRFs(t *testing.T) {
	next := frrLeakBase + `router ospf vrf vpn100
 redistribute kernel route-map SDWAN2CPE-vpn100
!
router bgp 65101 vrf vpn200
  redistribute kernel route-map SDWAN2CPE-vpn200
!
`
	// vpn0 unchanged; vpn100 gained ospf; vpn200 is new. export_vrfs also
	// mentions 300, which was exported before and is being dropped: nothing to
	// touch, the injection removal is enough.
	got := refreshVRFs(frrLeakBase, next, []byte(`[0,100,200,300]`))
	if !reflect.DeepEqual(got, []uint32{100}) {
		t.Fatalf("refreshVRFs = %v, want [100]", got)
	}
	if got := refreshVRFs(frrLeakBase, next, []byte(`[0]`)); got != nil {
		t.Fatalf("unchanged VPN touched: %v", got)
	}
	if got := refreshVRFs(frrLeakBase, next, []byte(`not-json`)); got != nil {
		t.Fatalf("bad export_vrfs still produced a touch set: %v", got)
	}
	// Losing a protocol is a change too.
	got = refreshVRFs(frrLeakBase+"router ospf vrf vpn100\n redistribute kernel route-map SDWAN2CPE-vpn100\n!\n",
		frrLeakBase, []byte(`[100]`))
	if !reflect.DeepEqual(got, []uint32{100}) {
		t.Fatalf("protocol removal = %v, want [100]", got)
	}
}
