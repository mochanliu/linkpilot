// Tests for the RR library and the tenant's two reflector slots (O6-c C-a).
//
// The properties worth pinning are the ones a page cannot show:
//
//   - a tenant can only pick a row it is allowed to see, and the check that
//     decides that is the same one the renderer uses -- so "the page offered it"
//     and "the device will be told about it" cannot drift apart.
//   - an unselected tenant renders NO rrs key at all. An empty list is not a
//     harmless placeholder: sr-route reads "rrs": [] and refuses to start, which
//     would take the whole route plane of that box down over a blank dropdown.
//   - deleting a row somebody still selects is refused, because the alternative
//     silently drops that tenant's reflectors.
//   - a reference that no longer resolves (a hand-edited state file) is reported
//     and omitted, never repaired behind the tenant's back.
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

func rrFixture(t *testing.T) (*server, string, string, string) {
	t.Helper()
	s, ten, two := overlayFixture(t)
	// The overlay fixture makes nodes but no tenant rows; the delete guard counts
	// references per *existing* tenant, so the library needs the world the console
	// actually has -- including names, which is what its refusal message quotes.
	now := int64(1700000000)
	ensureTenant(s.state, "t", now).Name = "甲方网络"
	ensureTenant(s.state, "t2", now).Name = "乙方网络"
	ops := consoleSession(t, s, consoleIdentity{Role: roleOperator, Username: "louis-ops", Via: "password"})
	return s, ten, two, ops
}

// rrReq routes one request to the library's handler.
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
		s.handleAdminRRs(rec, r)
	case "/api/rrs/delete":
		s.handleAdminRRDelete(rec, r)
	default:
		t.Fatalf("no RR handler for %s", r.URL.Path)
	}
	return rec
}

// rrCreate adds one row as the operator and returns its id.
func rrCreate(t *testing.T, s *server, sid, name, addr, owner string) string {
	t.Helper()
	body, _ := json.Marshal(rrWriteRequest{Name: name, Addr: addr, Location: "lab", TenantID: owner})
	rec := rrReq(t, s, http.MethodPost, "/api/rrs", string(body), sid)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/rrs (%s %s) = %d (%s)", name, addr, rec.Code, rec.Body.String())
	}
	var out struct {
		RR rrLibraryRow `json:"rr"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode RR response: %v (%s)", err, rec.Body.String())
	}
	return out.RR.ID
}

// rrSet writes the tenant's two slots. A nil pointer means "leave that格子不动",
// which is the distinction the handler is built around.
func rrSet(t *testing.T, s *server, sid string, primary, backup *string) *httptest.ResponseRecorder {
	t.Helper()
	req := map[string]any{"escape_all_colors": false}
	if primary != nil {
		req["rr_primary"] = *primary
	}
	if backup != nil {
		req["rr_backup"] = *backup
	}
	body, _ := json.Marshal(req)
	return overlayDo(t, s, http.MethodPut, "/api/overlay/settings", string(body), sid)
}

func rrStr(s string) *string { return &s }

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

// wireRRRow is what the device reads: the pair it dials plus the id it logs.
type wireRRRow struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
}

func TestRRAddListAndPick(t *testing.T) {
	s, ten, _, ops := rrFixture(t)

	first := rrCreate(t, s, ops, "RR 上海", "rr-sh.example.com:20200", "t")
	second := rrCreate(t, s, ops, "RR 法兰克福", "192.0.2.9:20200", "t")
	if first != "rr1" || second != "rr2" {
		t.Fatalf("ids = %s, %s，想要 rr1、rr2", first, second)
	}

	rec := rrReq(t, s, http.MethodGet, "/api/rrs", "", ops)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/rrs = %d", rec.Code)
	}
	var list struct {
		RRs []rrLibraryRow `json:"rrs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.RRs) != 2 || list.RRs[0].OwnerLabel != "甲方网络" {
		t.Fatalf("operator list = %+v", list.RRs)
	}
	for _, row := range list.RRs {
		if len(row.UsedBy) != 0 {
			t.Errorf("%s 才刚建就有人选？used_by=%v", row.ID, row.UsedBy)
		}
	}

	// The tenant sees both rows, with the address it will be dialled at.
	v := overlayGetView(t, s, ten)
	if len(v.RRLibrary) != 2 || v.RRPrimary != "" || v.RRLibrary[0].Addr == "" {
		t.Fatalf("tenant picker = %+v primary=%q", v.RRLibrary, v.RRPrimary)
	}
	if v.RRNote == "" {
		t.Error("页面需要一句关于主备语义的话，API 没给")
	}
	// Nothing picked yet: the honest line is an info, not an error, because every
	// box enrolled before this page runs on the rrs its operator wrote by hand.
	if msg := hasBlockingCheck(v.Checks); msg != "" {
		t.Errorf("没选 RR 不该是错误: %s", msg)
	}
	if !strings.Contains(fmt.Sprint(v.Checks), "还没选 RR") {
		t.Errorf("没选 RR 时要有一条说明，checks=%+v", v.Checks)
	}
}

func TestRRAddressValidation(t *testing.T) {
	s, _, _, ops := rrFixture(t)
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
		body, _ := json.Marshal(rrWriteRequest{Name: fmt.Sprintf("probe-%d", i), Addr: c.addr, TenantID: "t"})
		rec := rrReq(t, s, http.MethodPost, "/api/rrs", string(body), ops)
		if ok := rec.Code == http.StatusOK; ok != c.ok {
			t.Errorf("addr %q = %d，想要 ok=%v (%s)", c.addr, rec.Code, c.ok, rec.Body.String())
		}
	}
	// The name is checked too, and a long one is the shape that would otherwise
	// only be noticed when the page overflows.
	if rec := rrReq(t, s, http.MethodPost, "/api/rrs",
		fmt.Sprintf(`{"name":%q,"addr":"192.0.2.40:20200"}`, strings.Repeat("长", 90)), ops); rec.Code != http.StatusBadRequest {
		t.Errorf("超长名字 = %d，想要 400", rec.Code)
	}
}

func TestRRIsBoundToOneTenant(t *testing.T) {
	s, ten, two, ops := rrFixture(t)
	// The row name is echoed in the refusal, so it must not be a name that hands
	// the owner away either.
	mine := rrCreate(t, s, ops, "VIP 专线", "192.0.2.2:20200", "t")

	// t sees its own row; t2 sees nothing -- and cannot even tell that the row
	// exists, because it is not in its list.
	if got := overlayGetView(t, s, ten).RRLibrary; len(got) != 1 || got[0].ID != mine {
		t.Errorf("t 的可选列表 = %+v，想要只有 %s", got, mine)
	}
	if got2 := overlayGetView(t, s, two).RRLibrary; len(got2) != 0 {
		t.Errorf("t2 的可选列表 = %+v，想要 0 条", got2)
	}

	if rec := rrSet(t, s, two, rrStr(mine), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("t2 选别人的 RR = %d，想要 400 (%s)", rec.Code, rec.Body.String())
	} else if body := rec.Body.String(); strings.Contains(body, "甲方网络") {
		t.Errorf("拒绝消息里说出了归属租户是谁: %s", body)
	} else if !strings.Contains(body, "不属于本租户") {
		t.Errorf("拒绝的话没说明原因: %s", body)
	}
	if rec := rrSet(t, s, ten, rrStr(mine), nil); rec.Code != http.StatusOK {
		t.Fatalf("t 选自己的 RR = %d (%s)", rec.Code, rec.Body.String())
	}
	// An id that is not in the library at all is refused the same way.
	if rec := rrSet(t, s, ten, rrStr("rr999"), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("选了不存在的 id = %d，想要 400", rec.Code)
	}
	// 主 = 备 is not visible from either field alone, so it needs the pair.
	if rec := rrSet(t, s, ten, rrStr(mine), rrStr(mine)); rec.Code != http.StatusBadRequest {
		t.Fatalf("主备同一条 = %d，想要 400 (%s)", rec.Code, rec.Body.String())
	}
	// Every row is added for somebody: no 归属 is a missing decision, not "shared".
	if rec := rrReq(t, s, http.MethodPost, "/api/rrs",
		`{"name":"没有归属","addr":"192.0.2.3:20200"}`, ops); rec.Code != http.StatusBadRequest {
		t.Errorf("不填归属 = %d，想要 400 (%s)", rec.Code, rec.Body.String())
	}
	// An 归属 that names nobody is a typo, and a typo means a row no tenant can pick.
	if rec := rrReq(t, s, http.MethodPost, "/api/rrs",
		`{"name":"幽灵","addr":"192.0.2.3:20200","owner":"nope"}`, ops); rec.Code != http.StatusBadRequest {
		t.Errorf("归属填了不存在的租户 = %d，想要 400", rec.Code)
	}
}

// A row restored from before the binding rule -- its "owner" was empty, which
// used to mean "shared" -- must not be adopted by every tenant now. It stays in
// the library, is visible to nobody, and is named in the checks.
func TestRRUnboundRowIsVisibleToNobody(t *testing.T) {
	s, ten, _, ops := rrFixture(t)
	s.mu.Lock()
	lib := s.rrLibraryLocked(true)
	lib.Items = append(lib.Items, RRItem{ID: "rr50", Name: "老数据", Addr: "192.0.2.50:20200"})
	s.overlayConfigLocked("t", true).RRPrimary = "rr50"
	s.mu.Unlock()

	if got := overlayGetView(t, s, ten).RRLibrary; len(got) != 0 {
		t.Errorf("t 看到了没绑租户的行: %+v", got)
	}
	msg := hasBlockingCheck(overlayGetView(t, s, ten).Checks)
	if !strings.Contains(msg, "rr50") || !strings.Contains(msg, "绑定租户") {
		t.Errorf("读时检查没把「没绑租户」说清楚: %q", msg)
	}
	if doc := decodeRouteDoc(t, routeArtifact(t, s, "t", 1)); doc["rrs"] != nil {
		t.Errorf("没绑租户的行还是下发了: %s", doc["rrs"])
	}
	// The operator's table has to explain it too, and normalize must keep the row.
	rec := rrReq(t, s, http.MethodGet, "/api/rrs", "", ops)
	if !strings.Contains(rec.Body.String(), "未绑定租户") {
		t.Errorf("运营方表里没说明这一条没绑租户: %s", rec.Body.String())
	}
	normalizeRRs(s.state)
	if len(s.state.RRs.Items) != 1 || s.state.RRs.Items[0].ID != "rr50" {
		t.Fatalf("normalize 把没绑租户的行删了: %+v", s.state.RRs.Items)
	}
}

func TestRRRenderIsOptInAndNeverEmpty(t *testing.T) {
	s, ten, _, ops := rrFixture(t)
	if _, ok := decodeRouteDoc(t, routeArtifact(t, s, "t", 1))["rrs"]; ok {
		t.Fatal("租户还没选，第 4 段里就有 rrs 了")
	}

	a := rrCreate(t, s, ops, "RR-A", "192.0.2.10:20200", "t")
	b := rrCreate(t, s, ops, "RR-B", "192.0.2.11:20200", "t")
	if rec := rrSet(t, s, ten, rrStr(a), rrStr(b)); rec.Code != http.StatusOK {
		t.Fatalf("选主备 = %d (%s)", rec.Code, rec.Body.String())
	}

	doc := decodeRouteDoc(t, routeArtifact(t, s, "t", 1))
	if _, ok := doc["rrs"]; !ok {
		t.Fatal("选了 RR，第 4 段却没有 rrs")
	}
	rrs := decodedRRs(t, doc)
	if len(rrs) != 2 || rrs[0].ID != a || rrs[0].Addr != "192.0.2.10:20200" || rrs[1].ID != b {
		t.Fatalf("rrs = %+v，想要主在前、两条都带上 id 和 addr", rrs)
	}
	// The other tenant is unaffected: its devices get no rrs key.
	if _, ok := decodeRouteDoc(t, routeArtifact(t, s, "t2", 9))["rrs"]; ok {
		t.Error("t2 没选 RR，却收到了 rrs")
	}

	// Only 主: one row, not a two-row list with a hole in it.
	if rec := rrSet(t, s, ten, rrStr(a), rrStr("")); rec.Code != http.StatusOK {
		t.Fatalf("清掉备 RR = %d (%s)", rec.Code, rec.Body.String())
	}
	if rrs := decodedRRs(t, decodeRouteDoc(t, routeArtifact(t, s, "t", 1))); len(rrs) != 1 || rrs[0].ID != a {
		t.Fatalf("只选主的时候 rrs = %+v", rrs)
	}

	// Clearing both goes back to "no key at all", which is what keeps a box's
	// hand-written reflector list alive.
	if rec := rrSet(t, s, ten, rrStr(""), nil); rec.Code != http.StatusOK {
		t.Fatalf("清掉主 RR = %d", rec.Code)
	}
	out := routeArtifact(t, s, "t", 1)
	if doc := decodeRouteDoc(t, out); doc["rrs"] != nil {
		t.Fatalf("两个格子都清空了还在发 rrs: %s", doc["rrs"])
	}
	// And a tenant that picks nothing never sees an empty array either.
	if strings.Contains(out, `"rrs": []`) {
		t.Fatal("发了一条空的 rrs：sr-route 读到这个会拒绝启动")
	}
}

func TestRRDeleteGuardAndIDNeverReturns(t *testing.T) {
	s, ten, _, ops := rrFixture(t)
	keep := rrCreate(t, s, ops, "被选中的 RR", "192.0.2.20:20200", "t")
	free := rrCreate(t, s, ops, "没人选", "192.0.2.21:20200", "t")

	if rec := rrSet(t, s, ten, rrStr(keep), nil); rec.Code != http.StatusOK {
		t.Fatalf("选 RR = %d", rec.Code)
	}
	rec := rrReq(t, s, http.MethodPost, "/api/rrs/delete", fmt.Sprintf(`{"id":%q}`, keep), ops)
	if rec.Code != http.StatusConflict {
		t.Fatalf("删被引用的 RR = %d，想要 409 (%s)", rec.Code, rec.Body.String())
	}
	// The refusal has to name the customer to go and un-pick it, or the operator
	// learns only that something is wrong.
	if !strings.Contains(rec.Body.String(), "甲方网络") {
		t.Errorf("拒绝消息没带上是哪个租户: %s", rec.Body.String())
	}
	if rec := rrReq(t, s, http.MethodPost, "/api/rrs/delete", fmt.Sprintf(`{"id":%q}`, free), ops); rec.Code != http.StatusOK {
		t.Fatalf("删没人选的 RR = %d (%s)", rec.Code, rec.Body.String())
	}
	// Unselect, and it becomes deletable; the counter does not hand that id back.
	if rec := rrSet(t, s, ten, rrStr(""), nil); rec.Code != http.StatusOK {
		t.Fatalf("清掉选择 = %d", rec.Code)
	}
	if rec := rrReq(t, s, http.MethodPost, "/api/rrs/delete", fmt.Sprintf(`{"id":%q}`, keep), ops); rec.Code != http.StatusOK {
		t.Fatalf("解除引用之后删 = %d (%s)", rec.Code, rec.Body.String())
	}
	next := rrCreate(t, s, ops, "RR 新增", "192.0.2.22:20200", "t")
	if next == keep || next == free {
		t.Fatalf("新条目复用了已删除的 id %s：租户里那条引用会突然指向一台别的机器", next)
	}
}

// A dangling reference is only reachable by editing state.json by hand (the API
// refuses it and delete is guarded). The answer must be "say it and send nothing",
// not "quietly forget the customer's setting".
func TestRRDanglingReferenceIsNamedNotHealed(t *testing.T) {
	s, ten, _, _ := rrFixture(t)
	s.mu.Lock()
	s.overlayConfigLocked("t", true).RRPrimary = "rr77"
	s.mu.Unlock()

	v := overlayGetView(t, s, ten)
	msg := hasBlockingCheck(v.Checks)
	if msg == "" {
		t.Fatal("引用了一个不存在的 RR，页面却说不出问题")
	}
	if !strings.Contains(msg, "rr77") {
		t.Errorf("拒绝的话没带上那个 id: %s", msg)
	}
	if v.RRPrimary != "rr77" {
		t.Errorf("读一次就把租户的选择抹掉了: %q", v.RRPrimary)
	}
	if doc := decodeRouteDoc(t, routeArtifact(t, s, "t", 1)); doc["rrs"] != nil {
		t.Errorf("解析不出来的引用还是下发了: %s", doc["rrs"])
	}
}

func TestRRPersistenceAndNormalize(t *testing.T) {
	s, ten, _, ops := rrFixture(t)
	a := rrCreate(t, s, ops, "RR-A", "192.0.2.10:20200", "t")
	if rec := rrSet(t, s, ten, rrStr(a), nil); rec.Code != http.StatusOK {
		t.Fatalf("选 RR = %d", rec.Code)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	file := &fileStore{path: path}
	s.store = file
	s.save()
	st, err := file.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if st.RRs == nil || len(st.RRs.Items) != 1 || st.RRs.Items[0].ID != a {
		t.Fatalf("RR 库没存下来: %+v", st.RRs)
	}
	oc := st.Overlays["t"]
	if oc == nil || oc.RRPrimary != a {
		t.Fatalf("租户的选择没存下来: %+v", oc)
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
	// normalize must NOT clear a selection nobody can resolve -- that is the rule
	// TestRRDanglingReferenceIsNamedNotHealed pins, restated here because a repair
	// pass is exactly what would break it.
	if normalizeRRs(st) {
		t.Error("一次干净的 normalize 报告说有改动")
	}
	if st.Overlays["t"].RRPrimary != a {
		t.Errorf("normalize 把租户的选择改了: %q", st.Overlays["t"].RRPrimary)
	}
	// A hand-edited id higher than the counter must not be re-minted later.
	st.RRs.Items = append(st.RRs.Items, RRItem{ID: "rr40", Name: "手写", Addr: "192.0.2.40:20200", TenantID: "t"})
	normalizeRRs(st)
	if st.RRs.NextID < 40 {
		t.Errorf("计数器 %d 还低于库里已有的 rr40，下次新建会撞 id", st.RRs.NextID)
	}
}

func TestRROperatorOnly(t *testing.T) {
	s, ten, _, _ := rrFixture(t)
	if rec := rrReq(t, s, http.MethodGet, "/api/rrs", "", ten); rec.Code != http.StatusForbidden {
		t.Errorf("租户读 /api/rrs = %d，想要 403", rec.Code)
	}
	body, _ := json.Marshal(rrWriteRequest{Name: "x", Addr: "1.2.3.4:20200"})
	if rec := rrReq(t, s, http.MethodPost, "/api/rrs", string(body), ten); rec.Code != http.StatusForbidden {
		t.Errorf("租户写 /api/rrs = %d，想要 403", rec.Code)
	}
}

// Changing an RR address is a configuration change like any other: it reaches a
// box by publishing, and the page has to say so rather than let the operator think
// an address is live the moment it is typed.
func TestRREditMarksTheDevicePending(t *testing.T) {
	s, ten, _, ops := rrFixture(t)
	// The overlay fixture seeds drafts straight into the map, so node 1 has no
	// saved version and a publish would be refused. Save one the way the page does.
	cfg := baseValidConfig()
	cfg.Hostname = "edge-1"
	draftBody, _ := json.Marshal(saveConfigRequest{NodeID: 1, Config: cfg})
	if v := decodeConfigView(t, configRequest(t, s, ten, http.MethodPut, "/api/config", string(draftBody))); v.Version != 1 {
		t.Fatalf("存草稿 = %+v", v)
	}
	a := rrCreate(t, s, ops, "RR-A", "192.0.2.10:20200", "t")
	if rec := rrSet(t, s, ten, rrStr(a), nil); rec.Code != http.StatusOK {
		t.Fatalf("选 RR = %d", rec.Code)
	}
	if rec := publishReq(t, s, ten, http.MethodPost, `{"node_id":1}`); rec.Code != http.StatusOK {
		t.Fatalf("发布 v1 = %d (%s)", rec.Code, rec.Body.String())
	}
	if v := decodeConfigView(t, configRequest(t, s, ten, http.MethodGet, "/api/config?node=1", "")); v.OverlayPending {
		t.Fatal("刚发布完就说待发布")
	}
	body, _ := json.Marshal(rrWriteRequest{ID: a, Name: "RR-A", Addr: "192.0.2.99:20200", TenantID: "t"})
	if rec := rrReq(t, s, http.MethodPut, "/api/rrs", string(body), ops); rec.Code != http.StatusOK {
		t.Fatalf("改地址 = %d (%s)", rec.Code, rec.Body.String())
	}
	if v := decodeConfigView(t, configRequest(t, s, ten, http.MethodGet, "/api/config?node=1", "")); !v.OverlayPending {
		t.Fatal("运营方改了 RR 地址，设备页却没有说这一版还没发布")
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
