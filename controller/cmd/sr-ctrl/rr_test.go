// Tests for the tenant's RR list (O6-c C-a; moved under the tenant 2026-11-01).
//
// The properties worth pinning are the ones a page cannot show:
//
//   - the list is the tenant's own, and the same filter decides what the page
//     shows and what the device is told -- so "the page offered it" and "the
//     device will dial it" cannot drift apart.
//   - a device with no rows in its tenant renders NO rrs key at all. An empty
//     list is not a harmless placeholder: sr-route reads "rrs": [] and refuses
//     to start, which would take the whole route plane of that box down over an
//     empty table.
//   - only a device that picked an RR 1 / RR 2 pair gets reflectors: that pair.
//     A device with no selection gets no rrs key and keeps its own route.json
//     list (2026-11-01: the pair is edited on the 导通策略 device table).
//   - deleting a row a device still dials is refused, because the alternative
//     silently drops that device's reflector.
//   - a row nothing can manage (no tenant, or a tenant that no longer exists) is
//     dropped by normalize rather than left in the table forever.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"sdwan/controller/internal/apply"
)

// rrFixture returns the tenant sessions for t and t2 plus an operator session.
// The operator is only here to prove it no longer has any business with this
// table.
func rrFixture(t *testing.T) (*server, string, string, string) {
	t.Helper()
	s, ten, two := overlayFixture(t)
	now := int64(1700000000)
	ensureTenant(s.state, "t", now).Name = "甲方网络"
	ensureTenant(s.state, "t2", now).Name = "乙方网络"
	ops := consoleSession(t, s, consoleIdentity{Role: roleOperator, Username: "louis-ops", Via: "password"})
	return s, ten, two, ops
}

// rrReq routes one request to the RR handlers.
func rrReq(t *testing.T, s *server, method, target, body, sid string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	rec := httptest.NewRecorder()
	switch r.URL.Path {
	case "/api/rrs":
		s.handleTenantRRs(rec, r)
	case "/api/rrs/delete":
		s.handleTenantRRDelete(rec, r)
	default:
		t.Fatalf("no RR handler for %s", r.URL.Path)
	}
	return rec
}

// rrCreate adds one row as the session's tenant and returns its id.
func rrCreate(t *testing.T, s *server, sid, name, addr string) string {
	t.Helper()
	body, _ := json.Marshal(rrWriteRequest{Name: name, Addr: addr, Location: "lab"})
	rec := rrReq(t, s, http.MethodPost, "/api/rrs", string(body), sid)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/rrs (%s %s) = %d (%s)", name, addr, rec.Code, rec.Body.String())
	}
	var out struct {
		RR rrRow `json:"rr"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode RR response: %v (%s)", err, rec.Body.String())
	}
	return out.RR.ID
}

// deviceBundle fetches what the box would install right now, the same way
// sr-agent does, and decodes it with the device's own decoder.
func deviceBundle(t *testing.T, s *server, tenant string, node uint16) apply.Bundle {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleConfigBundle(rec, peerReq(http.MethodGet, "/v1/config/bundle", "", tenant, node))
	if rec.Code != http.StatusOK {
		t.Fatalf("bundle = %d (%s)", rec.Code, rec.Body.String())
	}
	b, err := apply.DecodeBundle(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("bundle 解不开: %v", err)
	}
	return b
}

func decodedRRs(t *testing.T, doc map[string]json.RawMessage) []wireRRRow {
	t.Helper()
	var out []wireRRRow
	if err := json.Unmarshal(doc["rrs"], &out); err != nil {
		t.Fatalf("rrs 不是一个数组: %v (%s)", err, doc["rrs"])
	}
	return out
}

// wireRRRow is what the device reads: the address it dials plus the id it logs.
type wireRRRow struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
}

func TestRRAddListAndSeeInOverlay(t *testing.T) {
	s, ten, _, ops := rrFixture(t)

	first := rrCreate(t, s, ten, "RR 上海", "rr-sh.example.com:20200")
	second := rrCreate(t, s, ten, "RR 法兰克福", "192.0.2.9:20200")
	if first != "rr1" || second != "rr2" {
		t.Fatalf("ids = %s, %s，想要 rr1、rr2", first, second)
	}

	rec := rrReq(t, s, http.MethodGet, "/api/rrs", "", ten)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/rrs = %d", rec.Code)
	}
	var list struct {
		RRs    []rrRow `json:"rrs"`
		MaxRRs int     `json:"max_rrs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.RRs) != 2 || list.MaxRRs != rrMaxPerTenant {
		t.Fatalf("列表 = %+v max=%d", list.RRs, list.MaxRRs)
	}
	for _, row := range list.RRs {
		if len(row.Devices) != 0 {
			t.Errorf("%s 才刚建就被设备选着？devices=%v", row.ID, row.Devices)
		}
	}

	// The same two rows arrive inside the overlay view, which is the only read the
	// page uses -- one filter, not two.
	v := overlayGetView(t, s, ten)
	if len(v.RRLibrary) != 2 || v.RRLibrary[0].ID != first || v.RRLibrary[0].Addr == "" {
		t.Fatalf("overlay 里的 RR 列表 = %+v", v.RRLibrary)
	}
	if v.MaxRRs != rrMaxPerTenant || v.RRNote == "" {
		t.Fatalf("页面上限/说明没给: max=%d note=%q", v.MaxRRs, v.RRNote)
	}

	// The operator has no page and no endpoint here any more.
	if rec := rrReq(t, s, http.MethodGet, "/api/rrs", "", ops); rec.Code != http.StatusForbidden {
		t.Errorf("运营方读 /api/rrs = %d，想要 403", rec.Code)
	}
}

func TestRRAddressValidation(t *testing.T) {
	s, ten, _, _ := rrFixture(t)
	cases := []struct {
		addr string
		ok   bool
	}{
		{"rr.example.com:20200", true},
		{"192.0.2.9:20200", true},
		{"[2001:db8::1]:20200", true},
		{"rr1.lab-ctrl:8080", true},
		{"203.0.113.7:1", true},
		{"rr.example.com", false},
		{"rr.example.com:0", false},
		{"rr.example.com:70000", false},
		{":20200", false},
		{"https://rr.example.com:20200", false},
		{"rr.example.com:20200/status", false},
		{"user@rr.example.com:20200", false},
		{"rr .example.com:20200", false},
		{"-rr.example.com:20200", false},
		{"2001:db8::1:20200", false},
		{"", false},
	}
	for i, c := range cases {
		body, _ := json.Marshal(rrWriteRequest{Name: fmt.Sprintf("probe-%d", i), Addr: c.addr})
		rec := rrReq(t, s, http.MethodPost, "/api/rrs", string(body), ten)
		if ok := rec.Code == http.StatusOK; ok != c.ok {
			t.Errorf("addr %q = %d，想要 ok=%v (%s)", c.addr, rec.Code, c.ok, rec.Body.String())
		}
	}
	// The name is checked too, and a long one is the shape that would otherwise
	// only be noticed when the page overflows.
	if rec := rrReq(t, s, http.MethodPost, "/api/rrs",
		fmt.Sprintf(`{"name":%q,"addr":"192.0.2.40:20200"}`, strings.Repeat("长", 90)), ten); rec.Code != http.StatusBadRequest {
		t.Errorf("超长名字 = %d，想要 400", rec.Code)
	}
}

func TestRRIsOneTenantsOwn(t *testing.T) {
	s, ten, two, _ := rrFixture(t)
	mine := rrCreate(t, s, ten, "VIP 专线", "192.0.2.2:20200")

	// t sees its own row; t2 sees nothing -- and cannot even tell that the row
	// exists, because it is not in its list and every by-id answer is 404.
	if got := overlayGetView(t, s, ten).RRLibrary; len(got) != 1 || got[0].ID != mine {
		t.Errorf("t 的列表 = %+v，想要只有 %s", got, mine)
	}
	if got := overlayGetView(t, s, two).RRLibrary; len(got) != 0 {
		t.Errorf("t2 的列表 = %+v，想要 0 条", got)
	}
	if rec := rrReq(t, s, http.MethodPut, "/api/rrs",
		fmt.Sprintf(`{"id":%q,"name":"偷改","addr":"192.0.2.3:20200"}`, mine), two); rec.Code != http.StatusNotFound {
		t.Errorf("t2 改 t 的 RR = %d，想要 404", rec.Code)
	}
	if rec := rrReq(t, s, http.MethodPost, "/api/rrs/delete", fmt.Sprintf(`{"id":%q}`, mine), two); rec.Code != http.StatusNotFound {
		t.Errorf("t2 删 t 的 RR = %d，想要 404", rec.Code)
	}
	// Reserving an address is a per-tenant rule, not a global one: two tenants may
	// each run a reflector on the same host without affecting each other.
	if got := rrCreate(t, s, two, "乙方自己的", "192.0.2.2:20200"); got == mine {
		t.Fatalf("t2 拿到了一个已存在的 id: %s", got)
	}

	// A t2 device that names t's row is refused by the same visibility gate the
	// rest of the page uses, and the message must not hand the owner away.
	rec := rrBindSet(t, s, two, 9, mine, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("t2 的设备选了 t 的 RR = %d，想要 400 (%s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "甲方网络") || strings.Contains(body, "VIP 专线") {
		t.Errorf("拒绝消息里说出了另一租户的名字: %s", body)
	}
	if !strings.Contains(rec.Body.String(), "不在本租户") {
		t.Errorf("拒绝的话没说明原因: %s", rec.Body.String())
	}
}

// A row restored from before the binding rule -- its "owner" was empty, which used
// to mean "shared" -- must not be adopted by every tenant, and nobody can manage
// it through the console any more, so normalize drops it.
func TestRRUnmanageableRowIsDropped(t *testing.T) {
	s, ten, _, _ := rrFixture(t)
	s.mu.Lock()
	lib := s.rrLibraryLocked(true)
	lib.Items = append(lib.Items,
		RRItem{ID: "rr50", Name: "老数据", Addr: "192.0.2.50:20200"},
		RRItem{ID: "rr51", Name: "幽灵租户", Addr: "192.0.2.51:20200", TenantID: "gone"})
	s.mu.Unlock()

	if got := overlayGetView(t, s, ten).RRLibrary; len(got) != 0 {
		t.Errorf("t 看到了不属于任何现存租户的行: %+v", got)
	}
	if doc := decodeRouteDoc(t, routeArtifact(t, s, "t", 1)); doc["rrs"] != nil {
		t.Errorf("管不了的行还是下发了: %s", doc["rrs"])
	}
	// The empty list is an info, not an error: the device keeps its own rrs.
	if msg := hasBlockingCheck(overlayGetView(t, s, ten).Checks); msg != "" {
		t.Errorf("没有 RR 不该是错误: %s", msg)
	}
	if !normalizeRRs(s.state) {
		t.Fatal("normalize 没报告删掉了管不了的行")
	}
	if len(s.state.RRs.Items) != 0 {
		t.Fatalf("normalize 之后还留着: %+v", s.state.RRs.Items)
	}
}

func TestRRRenderIsSelectionOnlyAndNeverEmpty(t *testing.T) {
	s, ten, _, _ := rrFixture(t)
	if _, ok := decodeRouteDoc(t, routeArtifact(t, s, "t", 1))["rrs"]; ok {
		t.Fatal("一台设备也没选，第 4 段里就有 rrs 了")
	}

	a := rrCreate(t, s, ten, "RR-A", "192.0.2.10:20200")
	b := rrCreate(t, s, ten, "RR-B", "192.0.2.11:20200")

	// Adding rows alone reaches nobody: an unselected device keeps its own list.
	if _, ok := decodeRouteDoc(t, routeArtifact(t, s, "t", 1))["rrs"]; ok {
		t.Fatal("库里加了 RR，没选的设备也拿到了 rrs")
	}

	if rec := rrBindSet(t, s, ten, 1, a, b); rec.Code != http.StatusOK {
		t.Fatalf("存选择 = %d (%s)", rec.Code, rec.Body.String())
	}
	doc := decodeRouteDoc(t, routeArtifact(t, s, "t", 1))
	if _, ok := doc["rrs"]; !ok {
		t.Fatal("选了 RR，第 4 段却没有 rrs")
	}
	rrs := decodedRRs(t, doc)
	if len(rrs) != 2 || rrs[0].ID != a || rrs[0].Addr != "192.0.2.10:20200" || rrs[1].ID != b {
		t.Fatalf("rrs = %+v，想要两条、按 id 排序、都带上 id 和 addr", rrs)
	}
	// The other tenant is unaffected: its devices get no rrs key.
	if _, ok := decodeRouteDoc(t, routeArtifact(t, s, "t2", 9))["rrs"]; ok {
		t.Error("t2 没有选择，却收到了 rrs")
	}

	// Deleting every row goes back to "no key at all", which is what keeps a box's
	// own reflector list alive. The selection has to go first: it is a reference.
	if rec := rrBindSet(t, s, ten, 1, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("清空选择 = %d (%s)", rec.Code, rec.Body.String())
	}
	for _, id := range []string{a, b} {
		if rec := rrReq(t, s, http.MethodPost, "/api/rrs/delete",
			fmt.Sprintf(`{"id":%q}`, id), ten); rec.Code != http.StatusOK {
			t.Fatalf("删 %s = %d (%s)", id, rec.Code, rec.Body.String())
		}
	}
	out := routeArtifact(t, s, "t", 1)
	if doc := decodeRouteDoc(t, out); doc["rrs"] != nil {
		t.Fatalf("删空了还在发 rrs: %s", doc["rrs"])
	}
	// And a tenant with no rows never sees an empty array either.
	if strings.Contains(out, `"rrs": []`) {
		t.Fatal("发了一条空的 rrs：sr-route 读到这个会拒绝启动")
	}
}

func TestRRDuplicateAddressRejected(t *testing.T) {
	s, ten, _, _ := rrFixture(t)
	a := rrCreate(t, s, ten, "RR-A", "192.0.2.10:20200")

	// Same address under a different name is the same reflector twice.
	body, _ := json.Marshal(rrWriteRequest{Name: "RR-A 副本", Addr: "192.0.2.10:20200"})
	rec := rrReq(t, s, http.MethodPost, "/api/rrs", string(body), ten)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "192.0.2.10:20200") {
		t.Fatalf("重复地址 = %d (%s)，想要 400 并带上那个地址", rec.Code, rec.Body.String())
	}

	b := rrCreate(t, s, ten, "RR-B", "192.0.2.11:20200")
	// Editing B onto A's address is refused; saving B unchanged is not.
	if rec := rrReq(t, s, http.MethodPut, "/api/rrs",
		fmt.Sprintf(`{"id":%q,"name":"RR-B","addr":"192.0.2.10:20200"}`, b), ten); rec.Code != http.StatusBadRequest {
		t.Errorf("改成别人的地址 = %d，想要 400 (%s)", rec.Code, rec.Body.String())
	}
	if rec := rrReq(t, s, http.MethodPut, "/api/rrs",
		fmt.Sprintf(`{"id":%q,"name":"RR-B","addr":"192.0.2.11:20200"}`, b), ten); rec.Code != http.StatusOK {
		t.Errorf("不改地址的编辑被自己挡住了 = %d (%s)", rec.Code, rec.Body.String())
	}

	// The API refuses it, so a duplicate can only come from a hand-edited file;
	// then the read-time check has to name both rows instead of staying silent.
	s.mu.Lock()
	s.state.RRs.Items = append(s.state.RRs.Items,
		RRItem{ID: "rr90", Name: "手写副本", Addr: "192.0.2.10:20200", TenantID: "t"})
	s.mu.Unlock()
	msg := hasBlockingCheck(overlayGetView(t, s, ten).Checks)
	if !strings.Contains(msg, a) || !strings.Contains(msg, "rr90") {
		t.Errorf("重复地址没被两个 id 一起点名: %q", msg)
	}
}

func TestRRMaxPerTenant(t *testing.T) {
	s, ten, two, _ := rrFixture(t)
	for i := 0; i < rrMaxPerTenant; i++ {
		rrCreate(t, s, ten, fmt.Sprintf("RR-%d", i), fmt.Sprintf("192.0.2.%d:20200", 100+i))
	}
	body, _ := json.Marshal(rrWriteRequest{Name: "多出来的", Addr: "192.0.2.200:20200"})
	rec := rrReq(t, s, http.MethodPost, "/api/rrs", string(body), ten)
	if rec.Code != http.StatusConflict {
		t.Fatalf("第 %d 条 = %d，想要 409 (%s)", rrMaxPerTenant+1, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), fmt.Sprint(rrMaxPerTenant)) {
		t.Errorf("拒绝消息没带上上限: %s", rec.Body.String())
	}
	// The cap is per tenant, not per table.
	if got := rrCreate(t, s, two, "乙方第一条", "192.0.2.201:20200"); got == "" {
		t.Fatal("另一个租户被别人的上限挡住了")
	}
}

func TestRRDeleteGuardAndIDNeverReturns(t *testing.T) {
	s, ten, _, _ := rrFixture(t)
	keep := rrCreate(t, s, ten, "被选着的 RR", "192.0.2.20:20200")
	free := rrCreate(t, s, ten, "没人选", "192.0.2.21:20200")

	if rec := rrBindSet(t, s, ten, 1, keep, ""); rec.Code != http.StatusOK {
		t.Fatalf("存选择 = %d (%s)", rec.Code, rec.Body.String())
	}
	rec := rrReq(t, s, http.MethodPost, "/api/rrs/delete", fmt.Sprintf(`{"id":%q}`, keep), ten)
	if rec.Code != http.StatusConflict {
		t.Fatalf("删被设备选着的 RR = %d，想要 409 (%s)", rec.Code, rec.Body.String())
	}
	// The refusal has to name the device to go and change, or the tenant learns
	// only that something is wrong.
	if !strings.Contains(rec.Body.String(), "node-1") {
		t.Errorf("拒绝消息没带上是哪台设备: %s", rec.Body.String())
	}
	// The list shows the reference too, so the row can explain itself before the
	// delete button is pressed.
	for _, row := range overlayGetView(t, s, ten).RRLibrary {
		if row.ID != keep {
			continue
		}
		if len(row.Devices) != 1 || row.Devices[0].NodeID != 1 {
			t.Errorf("%s 的引用 = %+v，想要 node 1", keep, row.Devices)
		}
	}
	if rec := rrReq(t, s, http.MethodPost, "/api/rrs/delete", fmt.Sprintf(`{"id":%q}`, free), ten); rec.Code != http.StatusOK {
		t.Fatalf("删没人选的 RR = %d (%s)", rec.Code, rec.Body.String())
	}
	// Dropping the selection releases the row; the counter does not hand the id back.
	if rec := rrBindSet(t, s, ten, 1, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("清空选择 = %d", rec.Code)
	}
	if rec := rrReq(t, s, http.MethodPost, "/api/rrs/delete", fmt.Sprintf(`{"id":%q}`, keep), ten); rec.Code != http.StatusOK {
		t.Fatalf("解除引用之后删 = %d (%s)", rec.Code, rec.Body.String())
	}
	next := rrCreate(t, s, ten, "RR 新增", "192.0.2.22:20200")
	if next == keep || next == free {
		t.Fatalf("新条目复用了已删除的 id %s：设备选择里那条引用会突然指向一台别的机器", next)
	}
}

func TestRRPersistenceAndNormalize(t *testing.T) {
	s, ten, _, _ := rrFixture(t)
	a := rrCreate(t, s, ten, "RR-A", "192.0.2.10:20200")
	path := filepath.Join(t.TempDir(), "state.json")
	file := &fileStore{path: path}
	s.store = file
	s.hardIndex = nil // 换 store = 模拟另一次启动，基线要从零建
	s.save()
	st, err := file.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if st.RRs == nil || len(st.RRs.Items) != 1 || st.RRs.Items[0].ID != a {
		t.Fatalf("RR 列表没存下来: %+v", st.RRs)
	}
	if st.RRs.Items[0].TenantID != "t" {
		t.Fatalf("归属没存下来: %+v", st.RRs.Items[0])
	}
	// A row with no address cannot be dialled: normalize drops it rather than
	// advertise an RR no device can reach.
	st.RRs.Items = append(st.RRs.Items, RRItem{ID: "rr9", Name: "残缺", Addr: "", TenantID: "t"})
	if !normalizeRRs(st) {
		t.Error("残缺条目被留下了")
	}
	for _, it := range st.RRs.Items {
		if it.ID == "rr9" {
			t.Fatal("normalize 之后那条没有地址的 RR 还在")
		}
	}
	if normalizeRRs(st) {
		t.Error("一次干净的 normalize 报告说有改动")
	}
	// A hand-edited id higher than the counter must not be re-minted later.
	st.RRs.Items = append(st.RRs.Items, RRItem{ID: "rr40", Name: "手写", Addr: "192.0.2.40:20200", TenantID: "t"})
	normalizeRRs(st)
	if st.RRs.NextID < 40 {
		t.Errorf("计数器 %d 还低于列表里已有的 rr40，下次新建会撞 id", st.RRs.NextID)
	}
}

// Adding or editing a row is a configuration change like any other: it reaches a
// box by publishing, and the page has to say so rather than let the tenant think
// an address is live the moment it is typed.
func TestRREditMarksTheDevicePending(t *testing.T) {
	s, ten, _, _ := rrFixture(t)
	// The overlay fixture seeds drafts straight into the map, so node 1 has no
	// saved version and a publish would be refused. Save one the way the page does.
	cfg := baseValidConfig()
	cfg.Hostname = "edge-1"
	draftBody, _ := json.Marshal(saveConfigRequest{NodeID: 1, Config: cfg})
	if v := decodeConfigView(t, configRequest(t, s, ten, http.MethodPut, "/api/config", string(draftBody))); v.Version != 1 {
		t.Fatalf("存草稿 = %+v", v)
	}
	a := rrCreate(t, s, ten, "RR-A", "192.0.2.10:20200")
	// The row only reaches node 1 through a selection; without one the device has
	// no rrs at all, so an address change would not be part of its bundle.
	if rec := rrBindSet(t, s, ten, 1, a, ""); rec.Code != http.StatusOK {
		t.Fatalf("存选择 = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := publishReq(t, s, ten, http.MethodPost, `{"node_id":1}`); rec.Code != http.StatusOK {
		t.Fatalf("发布 v1 = %d (%s)", rec.Code, rec.Body.String())
	}
	if v := decodeConfigView(t, configRequest(t, s, ten, http.MethodGet, "/api/config?node=1", "")); v.OverlayPending {
		t.Fatal("刚发布完就说待发布")
	}
	body, _ := json.Marshal(rrWriteRequest{ID: a, Name: "RR-A", Addr: "192.0.2.99:20200"})
	if rec := rrReq(t, s, http.MethodPut, "/api/rrs", string(body), ten); rec.Code != http.StatusOK {
		t.Fatalf("改地址 = %d (%s)", rec.Code, rec.Body.String())
	}
	if v := decodeConfigView(t, configRequest(t, s, ten, http.MethodGet, "/api/config?node=1", "")); !v.OverlayPending {
		t.Fatal("改了 RR 地址，设备页却没有说这一版还没发布")
	}
	if rrs := decodedRRs(t, decodeRouteDoc(t, routeArtifact(t, s, "t", 1))); len(rrs) != 1 || rrs[0].Addr != "192.0.2.99:20200" {
		t.Fatalf("改完地址、准备发布的那段还是旧的: %+v", rrs)
	}
	// What the box fetches right now is still the frozen v1: the published snapshot
	// must not re-render, or a device would change reflectors mid-flight without
	// anyone publishing.
	if b := deviceBundle(t, s, "t", 1); b.Version != 1 || !strings.Contains(b.Artifacts.Route, "192.0.2.10:20200") {
		t.Fatalf("未再发布时设备拿到 v%d，第 4 段应还是旧地址: %s", b.Version, b.Artifacts.Route)
	}
	// Publishing after the edit has to mint a *new* version -- the box's store
	// refuses "same version, different content", and an RR address is content.
	if rec := publishReq(t, s, ten, http.MethodPost, `{"node_id":1}`); rec.Code != http.StatusOK {
		t.Fatalf("再发布 = %d (%s)", rec.Code, rec.Body.String())
	}
	if v := decodeConfigView(t, configRequest(t, s, ten, http.MethodGet, "/api/config?node=1", "")); v.OverlayPending {
		t.Fatal("已经发布了，页面还说待发布")
	}
	if b := deviceBundle(t, s, "t", 1); b.Version != 2 || !strings.Contains(b.Artifacts.Route, "192.0.2.99:20200") {
		t.Fatalf("设备拿到 v%d 且第 4 段 %s，想要 v2 带新地址", b.Version, b.Artifacts.Route)
	}
}
