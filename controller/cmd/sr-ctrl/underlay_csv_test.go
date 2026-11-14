// Tests for the tunnel spreadsheet: the download → edit → upload loop.
//
// The properties worth pinning are the ones a person driving Excel cannot see:
// that an unedited file changes nothing (no re-delivery, no new version), that
// one file can add, edit and delete at the same time, that a row the allocator
// refuses takes the whole file down with it (never a half-applied table), and
// that a file in the wrong encoding is refused instead of guessed at.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"sdwan/controller/internal/underlay"
)

type csvImportResp struct {
	OK       bool            `json:"ok"`
	Dry      bool            `json:"dry"`
	Error    string          `json:"error"`
	Plan     underlayCSVPlan `json:"plan"`
	Underlay underlayView    `json:"underlay"`
}

func u8p(v uint8) *uint8 { return &v }

func encodeCSV(t *testing.T, recs [][]string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("\ufeff")
	w := csv.NewWriter(&b)
	for _, r := range recs {
		if err := w.Write(r); err != nil {
			t.Fatalf("encode csv: %v", err)
		}
	}
	w.Flush()
	return b.String()
}

// downloadCSV fetches the tunnel table and hands back its records, header
// first. Tests edit the records and upload them again -- exactly what an
// operator does, so a column that cannot survive the round trip fails here.
func downloadCSV(t *testing.T, s *server, sid string) [][]string {
	t.Helper()
	rec := underlayDo(t, s.handleAdminUnderlayTunnelCSV, sid, http.MethodGet, "/api/underlay/tunnels/csv", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("download = %d (%s)", rec.Code, rec.Body.String())
	}
	body := bytes.TrimPrefix(rec.Body.Bytes(), []byte("\xef\xbb\xbf"))
	recs, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("download is not CSV: %v", err)
	}
	return recs
}

// importCSV posts a table the way the page does: base64 inside a JSON body
// (every console POST carries JSON; see requireSession).
func importCSV(t *testing.T, s *server, sid, body string, dry, prune bool) (int, csvImportResp) {
	t.Helper()
	req := map[string]any{
		"csv":   base64.StdEncoding.EncodeToString([]byte(body)),
		"dry":   dry,
		"prune": prune,
	}
	rec := underlayDo(t, s.handleAdminUnderlayTunnelImport, sid, http.MethodPost,
		"/api/underlay/tunnels/import", mustJSON(t, req))
	var out csvImportResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		// 403 / 415 这类被中间层挡下来的回复是纯文本，不是 JSON。
		out.Error = rec.Body.String()
	}
	return rec.Code, out
}

func underlayGen(s *server, tenant string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effectiveUnderlayLocked(tenant).Gen
}

func tunnelIDs(doc underlayView) []string {
	out := make([]string, 0, len(doc.Tunnels))
	for _, t := range doc.Tunnels {
		out = append(out, t.ID)
	}
	return out
}

// TestUnderlayCSVHeaderMatchesLegend keeps the three copies of the column list
// -- the legend the page renders, the header the download writes, and the
// width of every row -- from drifting apart.
func TestUnderlayCSVHeaderMatchesLegend(t *testing.T) {
	s, sid, _ := underlayFixture(t)
	publishDefault(t, s, sid)
	createTunnel(t, s, sid, manualTunnel(1, 2, "wan0"))

	recs := downloadCSV(t, s, sid)
	titles := underlayCSVTitles()
	if len(recs[0]) != len(titles) {
		t.Fatalf("header has %d columns, legend has %d", len(recs[0]), len(titles))
	}
	for i, want := range titles {
		if recs[0][i] != want {
			t.Fatalf("column %d = %q, legend says %q", i+1, recs[0][i], want)
		}
	}
	for _, r := range recs[1:] {
		if len(r) != len(titles) {
			t.Fatalf("row has %d cells, header has %d", len(r), len(titles))
		}
	}
	// The document carries the same list, so the page never keeps its own copy.
	doc := getUnderlayDoc(t, s, sid)
	if len(doc.CSV.Columns) != len(titles) {
		t.Fatalf("document legend has %d columns, header has %d", len(doc.CSV.Columns), len(titles))
	}
	if doc.CSV.MaxRows != underlayCSVMaxRows || doc.CSV.FileName == "" {
		t.Fatalf("document csv block is incomplete: %+v", doc.CSV)
	}
}

// TestUnderlayCSVRoundTripChangesNothing is the promise that makes the feature
// safe: download it, upload it untouched, and no tunnel is rewritten.
func TestUnderlayCSVRoundTripChangesNothing(t *testing.T) {
	s, sid, _ := underlayFixture(t)
	publishDefault(t, s, sid)
	createTunnel(t, s, sid, manualTunnel(1, 2, "wan0"))
	createTunnel(t, s, sid, tunnelReq{
		A: underlaySpec{NodeID: 1, Iface: "wan0"}, B: underlaySpec{NodeID: 3, Iface: "wan0"},
		Color: u8p(3), Weight: u8p(20), FecRatio: u8p(2),
	})
	gen := underlayGen(s, "t")
	body := encodeCSV(t, downloadCSV(t, s, sid))

	code, out := importCSV(t, s, sid, body, true, false)
	if code != http.StatusOK {
		t.Fatalf("preview = %d (%s)", code, out.Error)
	}
	if out.Plan.Keep != 2 || out.Plan.Add+out.Plan.Update+out.Plan.Delete != 0 || out.Plan.Errors != 0 {
		t.Fatalf("an unedited file planned changes: %+v", out.Plan)
	}

	// Committing it is refused rather than answered with a hollow success:
	// "nothing to do" is information, not a write.
	code, out = importCSV(t, s, sid, body, false, false)
	if code != http.StatusBadRequest || !strings.Contains(out.Error, "没有可执行的改动") {
		t.Fatalf("commit of a no-op = %d (%s)", code, out.Error)
	}
	if got := underlayGen(s, "t"); got != gen {
		t.Fatalf("generation moved for a no-op: %d → %d", gen, got)
	}
}

// TestUnderlayCSVAddEditDeleteInOneFile is the whole point of the format: one
// file, three intents, one atomic write.
func TestUnderlayCSVAddEditDeleteInOneFile(t *testing.T) {
	s, sid, _ := underlayFixture(t)
	publishDefault(t, s, sid)
	createTunnel(t, s, sid, manualTunnel(1, 2, "wan0"))
	createTunnel(t, s, sid, manualTunnel(1, 3, "wan0"))

	recs := downloadCSV(t, s, sid)
	// 第 1 条：改颜色、权重、动态与 FEC。
	recs[1][10], recs[1][11], recs[1][12], recs[1][13] = "3", "20", "开", "2"
	// 第 2 条：整行留着，操作写「删除」。
	recs[2][0] = "删除"
	// 新增一条经 cpe-1 中转的 cpe-2 ↔ cpe-3 隧道，地址列留空（用观测地址）。
	recs = append(recs, []string{
		"", "", "cpe-2", "", "", "", "cpe-3", "", "", "", "1", "", "关", "0", "cpe-1",
	})
	body := encodeCSV(t, recs)

	code, out := importCSV(t, s, sid, body, true, false)
	if code != http.StatusOK {
		t.Fatalf("preview = %d (%s)", code, out.Error)
	}
	if out.Plan.Add != 1 || out.Plan.Update != 1 || out.Plan.Delete != 1 || out.Plan.Errors != 0 {
		t.Fatalf("preview = %+v, want 1 add / 1 update / 1 delete", out.Plan)
	}
	if out.Underlay.Tunnels != nil {
		t.Fatal("a preview must not carry a document: it wrote something")
	}

	code, out = importCSV(t, s, sid, body, false, false)
	if code != http.StatusOK {
		t.Fatalf("apply = %d (%s)", code, out.Error)
	}
	if len(out.Underlay.Tunnels) != 2 {
		t.Fatalf("after apply there are %d tunnels, want 2 (%v)", len(out.Underlay.Tunnels), tunnelIDs(out.Underlay))
	}
	byID := map[string]underlayTunnelView{}
	for _, tun := range out.Underlay.Tunnels {
		byID[tun.ID] = tun
	}
	edited := byID["t1"]
	if edited.Color != 3 || edited.Weight != 20 || !edited.Dynamic || edited.FecRatio != 2 {
		t.Fatalf("t1 was not edited as written: %+v", edited)
	}
	if _, still := byID["t2"]; still {
		t.Fatal("t2 was marked 删除 and still exists")
	}
	var added *underlayTunnelView
	for i, tun := range out.Underlay.Tunnels {
		if tun.ID != "t1" {
			added = &out.Underlay.Tunnels[i]
		}
	}
	if added == nil || added.Color != 1 || added.Dynamic || added.FecRatio != 0 {
		t.Fatalf("the added tunnel does not look like its row: %+v", added)
	}
	if added.Path == nil || len(added.Path.Hops) != 1 || added.Path.Hops[0].NodeID != 1 {
		t.Fatalf("the added tunnel lost its relay: %+v", added.Path)
	}
	if added.A.NodeID != 2 || added.B.NodeID != 3 {
		t.Fatalf("the added tunnel connects %d ↔ %d, want 2 ↔ 3", added.A.NodeID, added.B.NodeID)
	}

	// 同一份文件再传一次：这次三条都该是「不变」，说明落库的形状能被导回来。
	again := encodeCSV(t, downloadCSV(t, s, sid))
	if code, out := importCSV(t, s, sid, again, true, false); code != http.StatusOK || out.Plan.Keep != 2 || out.Plan.Update != 0 {
		t.Fatalf("second round trip = %d %+v", code, out.Plan)
	}
}

// TestUnderlayCSVPruneIsOptIn: a row deleted from the file is only a deletion
// when the operator says the file is the whole truth.
func TestUnderlayCSVPruneIsOptIn(t *testing.T) {
	s, sid, _ := underlayFixture(t)
	publishDefault(t, s, sid)
	createTunnel(t, s, sid, manualTunnel(1, 2, "wan0"))
	createTunnel(t, s, sid, manualTunnel(2, 3, "wan0"))
	createTunnel(t, s, sid, manualTunnel(1, 3, "wan0"))

	recs := downloadCSV(t, s, sid)
	shrunk := encodeCSV(t, recs[:2]) // 只留第一条

	if code, out := importCSV(t, s, sid, shrunk, true, false); code != http.StatusOK ||
		out.Plan.Keep != 1 || out.Plan.Delete != 0 {
		t.Fatalf("without prune = %d %+v, want 1 keep and no delete", code, out.Plan)
	}
	code, out := importCSV(t, s, sid, shrunk, true, true)
	if code != http.StatusOK || out.Plan.Delete != 2 {
		t.Fatalf("with prune = %d %+v, want 2 deletes", code, out.Plan)
	}
	if code, out = importCSV(t, s, sid, shrunk, false, true); code != http.StatusOK {
		t.Fatalf("apply = %d (%s)", code, out.Error)
	}
	if ids := tunnelIDs(out.Underlay); len(ids) != 1 {
		t.Fatalf("after prune there are %d tunnels: %v", len(ids), ids)
	}
}

// TestUnderlayCSVBadRowBlocksEverything: one bad line and nothing is written.
// A half-applied table is worse than a rejected one, because the file no
// longer describes the controller.
func TestUnderlayCSVBadRowBlocksEverything(t *testing.T) {
	s, sid, _ := underlayFixture(t)
	publishDefault(t, s, sid)
	createTunnel(t, s, sid, manualTunnel(1, 2, "wan0"))
	createTunnel(t, s, sid, manualTunnel(2, 3, "wan0"))
	gen := underlayGen(s, "t")
	before := getUnderlayDoc(t, s, sid)

	recs := downloadCSV(t, s, sid)
	recs[1][2] = "cpe-9" // 设备不存在
	recs[2][10] = "9"    // 颜色越界
	body := encodeCSV(t, recs)

	code, out := importCSV(t, s, sid, body, false, false)
	if code != http.StatusBadRequest {
		t.Fatalf("apply of a broken file = %d, want 400", code)
	}
	if out.Plan.Errors != 2 {
		t.Fatalf("plan reports %d errors, want 2: %+v", out.Plan.Errors, out.Plan.Rows)
	}
	if !strings.Contains(out.Plan.Rows[0].Msg, "cpe-9") {
		t.Fatalf("the error does not name the device: %+v", out.Plan.Rows[0])
	}
	if got := underlayGen(s, "t"); got != gen {
		t.Fatalf("a rejected import moved the generation: %d → %d", gen, got)
	}
	after := getUnderlayDoc(t, s, sid)
	if len(after.Tunnels) != len(before.Tunnels) || after.Tunnels[0].Color != before.Tunnels[0].Color {
		t.Fatalf("a rejected import changed the tunnels: %+v", after.Tunnels)
	}
}

// TestUnderlayCSVAllocatorRefusalIsAtomic covers the errors that only show up
// while writing: the plan is clean, the allocator is not.
func TestUnderlayCSVAllocatorRefusalIsAtomic(t *testing.T) {
	s, sid, _ := underlayFixture(t)
	publishDefault(t, s, sid)
	createTunnel(t, s, sid, manualTunnel(1, 2, "wan0"))
	taken := getUnderlayDoc(t, s, sid).Tunnels[0].A.Port
	gen := underlayGen(s, "t")

	recs := downloadCSV(t, s, sid)
	recs = append(recs, []string{
		"", "", "cpe-1", "", "", fmt.Sprint(taken), "cpe-3", "", "", "", "0", "", "关", "0", "",
	})
	// 本端端口写成 cpe-1 上已被占用的那个：规划看不出来，分配器会拒绝。
	code, out := importCSV(t, s, sid, encodeCSV(t, recs), false, false)
	if code != http.StatusBadRequest || !strings.Contains(out.Error, "占用") {
		t.Fatalf("apply = %d (%s), want the port conflict", code, out.Error)
	}
	if got := underlayGen(s, "t"); got != gen {
		t.Fatalf("a refused write moved the generation: %d → %d", gen, got)
	}
	if ids := tunnelIDs(getUnderlayDoc(t, s, sid)); len(ids) != 1 {
		t.Fatalf("a refused write left %d tunnels: %v", len(ids), ids)
	}
}

// TestUnderlayCSVDeviceSpellings: the same device can be named three ways, and
// a name two devices claim resolves to neither.
func TestUnderlayCSVDeviceSpellings(t *testing.T) {
	s, sid, _ := underlayFixture(t)
	publishDefault(t, s, sid)

	body := encodeCSV(t, [][]string{
		underlayCSVTitles(),
		{"", "", "node-2", "", "", "", "3", "", "", "", "", "", "", "", ""},
	})
	code, out := importCSV(t, s, sid, body, false, false)
	if code != http.StatusOK {
		t.Fatalf("apply = %d (%s)", code, out.Error)
	}
	tun := out.Underlay.Tunnels[0]
	if tun.A.NodeID != 2 || tun.B.NodeID != 3 {
		t.Fatalf("resolved %d ↔ %d, want 2 ↔ 3", tun.A.NodeID, tun.B.NodeID)
	}

	// 两台设备同名时不能二选一：宁可报错。
	s.mu.Lock()
	s.state.Nodes[key("t", 2)].Serial = "cpe-1"
	s.state.Nodes[key("t", 3)].Serial = "cpe-1"
	s.mu.Unlock()
	code, out = importCSV(t, s, sid, encodeCSV(t, [][]string{
		underlayCSVTitles(),
		{"", "", "cpe-1", "", "", "", "node-3", "", "", "", "", "", "", "", ""},
	}), true, false)
	if code != http.StatusBadRequest || out.Plan.Errors != 1 {
		t.Fatalf("an ambiguous name resolved anyway: %+v", out.Plan)
	}
}

// TestUnderlayCSVTemplateByName pins the one place the table reaches outside
// itself: a path template is referenced by name and never created here.
func TestUnderlayCSVTemplateByName(t *testing.T) {
	s, sid, _ := underlayFixture(t)
	publishDefault(t, s, sid)

	s.mu.Lock()
	uc := s.underlayConfigLocked("t", true)
	tpl, err := uc.State.CreatePath("经 cpe-3 中转", []underlay.PathHop{{NodeID: 3}}, 1)
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("create template: %v", err)
	}

	body := encodeCSV(t, [][]string{
		underlayCSVTitles(),
		{"", "", "cpe-1", "", "", "", "cpe-2", "", "", "", "", "", "", "", "模板:经 cpe-3 中转"},
	})
	code, out := importCSV(t, s, sid, body, false, false)
	if code != http.StatusOK {
		t.Fatalf("apply = %d (%s)", code, out.Error)
	}
	tun := out.Underlay.Tunnels[0]
	if tun.Path == nil || tun.Path.TemplateID != tpl.ID {
		t.Fatalf("the tunnel does not reference the template: %+v", tun.Path)
	}
	// 导回来的写法必须是「模板:名字」，不是内联的跳链——否则再传一次就变成
	// 一条不再跟随模板的隧道。
	if cell := downloadCSV(t, s, sid)[1][14]; cell != "模板:经 cpe-3 中转" {
		t.Fatalf("exported path cell = %q", cell)
	}

	// 模板不存在时整份拒绝，而不是悄悄改成直连。
	body = encodeCSV(t, [][]string{
		underlayCSVTitles(),
		{"", "", "cpe-2", "", "", "", "cpe-3", "", "", "", "", "", "", "", "模板:没有这个模板"},
	})
	if code, out := importCSV(t, s, sid, body, true, false); code != http.StatusBadRequest ||
		!strings.Contains(out.Plan.Rows[0].Msg, "路径模板") {
		t.Fatalf("missing template = %d %+v", code, out.Plan)
	}
}

// TestUnderlayCSVRejectsBadFiles: the file-level refusals, all of which are
// about the file rather than a row.
func TestUnderlayCSVRejectsBadFiles(t *testing.T) {
	body := func() string {
		return encodeCSV(t, [][]string{underlayCSVTitles(),
			{"", "", "cpe-1", "", "", "", "cpe-2", "", "", "", "", "", "", "", ""}})
	}
	s, sid, _ := underlayFixture(t)
	publishDefault(t, s, sid)

	// GBK 的「操作」：不是合法 UTF-8，不能靠猜。
	gbk := append([]byte("\xef\xbb\xbf"), []byte{0xb2, 0xd9, 0xd7, 0xf7, ',', 'a', '\n'}...)
	code, out := importCSV(t, s, sid, string(gbk), true, false)
	if code != http.StatusBadRequest || !strings.Contains(out.Error, "UTF-8") {
		t.Fatalf("gbk file = %d (%s)", code, out.Error)
	}

	if code, out := importCSV(t, s, sid, "a,b,c\nd,e,f\n", true, false); code != http.StatusBadRequest ||
		!strings.Contains(out.Error, "表头") {
		t.Fatalf("wrong header = %d (%s)", code, out.Error)
	}

	// 只有表头：没有行可做，明说。
	if code, out := importCSV(t, s, sid, encodeCSV(t, [][]string{underlayCSVTitles()}), false, false); code != http.StatusBadRequest {
		t.Fatalf("header-only file = %d (%s)", code, out.Error)
	}

	// 注释行与空行可以夹在表头前面，Excel 里写备注不会被当成数据。
	withNote := "# 这一行是备注，导入时忽略\n\n" + strings.TrimPrefix(body(), "\ufeff")
	if code, out := importCSV(t, s, sid, withNote, true, false); code != http.StatusOK || out.Plan.Add != 1 {
		t.Fatalf("noted file = %d %+v (%s)", code, out.Plan, out.Error)
	}

	// 同一份文件里同一个隧道ID出现两次。
	createTunnel(t, s, sid, manualTunnel(1, 2, "wan0"))
	dup := encodeCSV(t, [][]string{underlayCSVTitles(),
		{"", "t1", "cpe-1", "", "", "", "cpe-2", "", "", "", "", "", "", "", ""},
		{"", "t1", "cpe-1", "", "", "", "cpe-3", "", "", "", "", "", "", "", ""}})
	if code, out := importCSV(t, s, sid, dup, true, false); code != http.StatusBadRequest ||
		!strings.Contains(out.Plan.Rows[0].Msg+out.Plan.Rows[1].Msg, "已经出现过") {
		t.Fatalf("duplicate id = %d %+v", code, out.Plan)
	}
}

// TestUnderlayCSVTenantIsolation: the table is a customer's network layout.
func TestUnderlayCSVTenantIsolation(t *testing.T) {
	s, sid, _ := underlayFixture(t)
	publishDefault(t, s, sid)
	createTunnel(t, s, sid, manualTunnel(1, 2, "wan0"))

	other := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t2", Username: "bob", Via: "password"})
	if rec := underlayDo(t, s.handleAdminUnderlayTunnelCSV, other, http.MethodGet, "/api/underlay/tunnels/csv", ""); rec.Code != http.StatusOK {
		t.Fatalf("t2 download = %d", rec.Code)
	} else if lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n"); len(lines) != 1 {
		t.Fatalf("t2 sees %d lines, want only the header", len(lines))
	}
	// 另一个租户的设备名在这份表里不存在：整份拒绝，不会写到 t 的隧道上。
	body := encodeCSV(t, [][]string{underlayCSVTitles(),
		{"", "", "cpe-1", "", "", "", "cpe-2", "", "", "", "", "", "", "", ""}})
	code, out := importCSV(t, s, other, body, false, false)
	if code != http.StatusBadRequest {
		t.Fatalf("t2 import = %d (%s)", code, out.Error)
	}

	// 运营角色根本进不来。
	op := consoleSession(t, s, consoleIdentity{Role: roleOperator, Username: "ops", Via: "password"})
	if rec := underlayDo(t, s.handleAdminUnderlayTunnelCSV, op, http.MethodGet, "/api/underlay/tunnels/csv", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("operator download = %d, want 403", rec.Code)
	}
	if code, _ := importCSV(t, s, op, body, true, false); code != http.StatusForbidden {
		t.Fatalf("operator import = %d, want 403", code)
	}
}
