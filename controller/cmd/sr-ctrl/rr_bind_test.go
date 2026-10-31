// Tests for the per-device RR 1 / RR 2 choice (2026-11-01: the 反射器分组 list
// became two columns on the 导通策略 device table).
//
// The properties worth pinning:
//
//   - a device with a pair dials exactly those two; a device with no pair gets no
//     rrs key at all, so it keeps the list its own route.json already carries.
//   - the slots go through the same ownership gate as the library, so a bind can
//     never name a reflector another tenant owns -- and the refusal must not hand
//     the other tenant's name away.
//   - the bind is a reference: deleting an RR a device still dials is refused,
//     and the refusal names the device to go and change.
//   - clearing both slots is "no selection", not a row with two empty strings.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rrBindSet saves one device's pair through the same endpoint the 导通策略 table
// uses.
func rrBindSet(t *testing.T, s *server, sid string, node uint16, primary, backup string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(overlayDeviceRRRequest{NodeID: node, Primary: primary, Backup: backup})
	return overlayDo(t, s, http.MethodPut, "/api/overlay/device-rr", string(body), sid)
}

func TestDeviceRRSelectionRenders(t *testing.T) {
	s, ten, _, _ := rrFixture(t)
	a := rrCreate(t, s, ten, "RR-A", "192.0.2.10:20200")
	b := rrCreate(t, s, ten, "RR-B", "192.0.2.11:20200")

	if rec := rrBindSet(t, s, ten, 1, a, b); rec.Code != http.StatusOK {
		t.Fatalf("存设备 RR = %d (%s)", rec.Code, rec.Body.String())
	}

	// node 1 dials the pair; node 3 has no selection and keeps its own list.
	rrs := decodedRRs(t, decodeRouteDoc(t, routeArtifact(t, s, "t", 1)))
	if len(rrs) != 2 || rrs[0].ID != a || rrs[1].ID != b {
		t.Fatalf("设备的 rrs = %+v，想要 %s/%s", rrs, a, b)
	}
	if doc := decodeRouteDoc(t, routeArtifact(t, s, "t", 3)); doc["rrs"] != nil {
		t.Fatalf("没选的设备拿到了 rrs = %v", doc["rrs"])
	}

	// The page reads the selection back from the same view it saved into.
	v := overlayGetView(t, s, ten)
	if got := v.RRBinds["1"]; got.Primary != a || got.Backup != b {
		t.Fatalf("视图里的选择 = %+v，想要 %s/%s", got, a, b)
	}
	if _, ok := v.RRBinds["3"]; ok {
		t.Fatalf("没选的设备出现在 rr_binds 里: %+v", v.RRBinds)
	}

	// Both slots empty is "no selection": the entry goes away instead of staying
	// as a pair of empty strings that renders nothing.
	if rec := rrBindSet(t, s, ten, 1, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("清空选择 = %d (%s)", rec.Code, rec.Body.String())
	}
	if _, ok := overlayGetView(t, s, ten).RRBinds["1"]; ok {
		t.Fatal("清空之后 rr_binds 里还留着这一台")
	}
	if doc := decodeRouteDoc(t, routeArtifact(t, s, "t", 1)); doc["rrs"] != nil {
		t.Fatalf("清空之后还在下发 rrs: %v", doc["rrs"])
	}
}

func TestDeviceRRRejectsUnpickablePairs(t *testing.T) {
	s, ten, two, _ := rrFixture(t)
	mine := rrCreate(t, s, ten, "RR-A", "192.0.2.10:20200")
	other := rrCreate(t, s, ten, "RR-B", "192.0.2.11:20200")
	foreign := rrCreate(t, s, two, "别人的 RR", "192.0.2.20:20200")

	cases := []struct {
		name    string
		node    uint16
		primary string
		backup  string
		want    string
	}{
		{"别的租户的 RR", 1, foreign, "", "不在本租户"},
		{"不存在的 id", 1, "rr77", "", "不在本租户"},
		{"两格同一条", 1, mine, mine, "不能选同一条"},
		{"设备不存在", 77, mine, "", "不属于这个租户"},
	}
	for _, c := range cases {
		rec := rrBindSet(t, s, ten, c.node, c.primary, c.backup)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s：存下来了（%d）", c.name, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s：拒绝话术里没有 %q：%s", c.name, c.want, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "乙方网络") || strings.Contains(rec.Body.String(), "别人的 RR") {
			t.Errorf("%s：拒绝消息说出了另一租户的东西: %s", c.name, rec.Body.String())
		}
	}

	if v := overlayGetView(t, s, ten); len(v.RRBinds) != 0 {
		t.Fatalf("拒绝的保存写进了状态: %+v", v.RRBinds)
	}
	if rec := rrBindSet(t, s, ten, 2, other, ""); rec.Code != http.StatusOK {
		t.Fatalf("合法选择 = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestDeviceRRIsAReferenceForTheDeleteGuard(t *testing.T) {
	s, ten, _, _ := rrFixture(t)
	mine := rrCreate(t, s, ten, "RR-A", "192.0.2.10:20200")
	other := rrCreate(t, s, ten, "RR-B", "192.0.2.11:20200")
	if rec := rrBindSet(t, s, ten, 1, mine, other); rec.Code != http.StatusOK {
		t.Fatalf("存选择 = %d (%s)", rec.Code, rec.Body.String())
	}

	rec := rrReq(t, s, http.MethodPost, "/api/rrs/delete", `{"id":"`+mine+`"}`, ten)
	if rec.Code != http.StatusConflict {
		t.Fatalf("删被设备选着的 RR = %d，想要 409 (%s)", rec.Code, rec.Body.String())
	}
	// The refusal has to say which device to go and change.
	if !strings.Contains(rec.Body.String(), "node-1") {
		t.Errorf("拒绝消息没带上设备号: %s", rec.Body.String())
	}
	// The row explains itself before the button is pressed.
	for _, row := range overlayGetView(t, s, ten).RRLibrary {
		if row.ID != mine {
			continue
		}
		if len(row.Devices) != 1 || row.Devices[0].NodeID != 1 {
			t.Errorf("%s 的引用 = %+v，想要 node 1", mine, row.Devices)
		}
	}

	if rec := rrBindSet(t, s, ten, 1, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("清空选择 = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := rrReq(t, s, http.MethodPost, "/api/rrs/delete", `{"id":"`+mine+`"}`, ten); rec.Code != http.StatusOK {
		t.Fatalf("清空选择后还删不了 RR = %d (%s)", rec.Code, rec.Body.String())
	}
}

// A hand-edited state file can hold a selection that no longer resolves. The
// read has to name it and render nothing -- not heal it, which would be the
// controller silently picking reflectors for the customer.
func TestDeviceRRStaleStateIsNamedNotHealed(t *testing.T) {
	s, ten, _, _ := rrFixture(t)
	a := rrCreate(t, s, ten, "RR-A", "192.0.2.10:20200")

	s.mu.Lock()
	oc := s.overlayConfigLocked("t", true)
	oc.RRBinds["1"] = RRBind{Primary: "rr77"}
	oc.RRBinds["9"] = RRBind{Primary: a} // 设备 9 不属于 t
	s.mu.Unlock()

	v := overlayGetView(t, s, ten)
	text := fmtChecks(v.Checks)
	if !strings.Contains(text, "rr77") || !strings.Contains(text, "不属于这个租户") {
		t.Fatalf("两类问题没有都说出来: %+v", v.Checks)
	}
	if doc := decodeRouteDoc(t, routeArtifact(t, s, "t", 1)); doc["rrs"] != nil {
		t.Errorf("解析不出来的选择还是下发了 rrs: %s", doc["rrs"])
	}
}

func fmtChecks(checks []ConfigCheck) string {
	var b strings.Builder
	for _, c := range checks {
		b.WriteString(c.Text)
		b.WriteString("\n")
	}
	return b.String()
}

func TestDeviceRRPersistenceAndRepair(t *testing.T) {
	s, ten, _, _ := rrFixture(t)
	a := rrCreate(t, s, ten, "RR-A", "192.0.2.10:20200")
	// Whitespace is trimmed on the way in; a pair with neither slot is dropped.
	if rec := rrBindSet(t, s, ten, 2, " "+a+" ", ""); rec.Code != http.StatusOK {
		t.Fatalf("存选择 = %d (%s)", rec.Code, rec.Body.String())
	}
	st, err := s.store.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	oc := st.Overlays["t"]
	if oc == nil || oc.RRBinds["2"].Primary != a {
		t.Fatalf("选择没存下来: %+v", oc)
	}

	oc.RRBinds["3"] = RRBind{Primary: " ", Backup: " "}
	oc.ensure()
	if _, ok := oc.RRBinds["3"]; ok {
		t.Fatalf("两格全空的选择没被清掉: %+v", oc.RRBinds)
	}
}

// A state file written before the columns existed carries rr_groups, which this
// build no longer decodes into anything but LegacyRRGroups. Without the migration
// the next save would drop the field and every device on the box would silently
// stop dialling its reflectors -- so ensure fans each group's pair out to its
// devices and then drops the old rows.
func TestLegacyRRGroupsMigrateToDeviceBinds(t *testing.T) {
	const state = `{"overlays":{"t":{
		"rr_groups":[
			{"id":"g1","name":"老分组","device_ids":[1,2,3],"rr_primary":"rr1","rr_backup":"rr2"},
			{"id":"g2","name":"同一台当两格","device_ids":[4],"rr_primary":"rr1","rr_backup":"rr1"},
			{"id":"g3","name":"空","device_ids":[5],"rr_primary":"","rr_backup":""},
			{"id":"g4","name":"占位 0","device_ids":[0,6],"rr_primary":"rr1","rr_backup":""}
		],
		"rr_binds":{"2":{"primary":"rr9"}}
	}}}`
	var st struct {
		Overlays map[string]*OverlayConfig `json:"overlays"`
	}
	if err := json.Unmarshal([]byte(state), &st); err != nil {
		t.Fatalf("老状态解析失败: %v", err)
	}
	oc := st.Overlays["t"]
	if len(oc.LegacyRRGroups) != 4 {
		t.Fatalf("老字段没读到: %+v", oc.LegacyRRGroups)
	}
	oc.ensure()

	want := map[string]RRBind{
		"1": {Primary: "rr1", Backup: "rr2"},
		"2": {Primary: "rr9"}, // 留下的那一格不能被分组覆盖
		"3": {Primary: "rr1", Backup: "rr2"},
		"4": {Primary: "rr1"}, // 两格相同等于只选一台
		"6": {Primary: "rr1"},
	}
	for k, w := range want {
		if got := oc.RRBinds[k]; got != w {
			t.Errorf("node %s = %+v，想要 %+v", k, got, w)
		}
	}
	if _, ok := oc.RRBinds["5"]; ok {
		t.Errorf("空的一对也生成了选择: %+v", oc.RRBinds["5"])
	}
	if _, ok := oc.RRBinds["0"]; ok {
		t.Errorf("node 0 不该有选择")
	}
	if oc.LegacyRRGroups != nil {
		t.Errorf("迁移后还留着老行: %+v", oc.LegacyRRGroups)
	}

	// 再跑一次必须什么都不改：ensure 每次加载都会走，不是一次性开关。
	before := len(oc.RRBinds)
	oc.ensure()
	if len(oc.RRBinds) != before {
		t.Fatalf("第二次 ensure 又改了选择: %+v", oc.RRBinds)
	}
	for k, w := range want {
		if oc.RRBinds[k] != w {
			t.Errorf("第二次 ensure 改坏了 node %s: %+v", k, oc.RRBinds[k])
		}
	}
}
