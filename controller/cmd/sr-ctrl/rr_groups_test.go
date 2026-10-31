// Tests for RR groups (O9-b): one tenant's devices split across reflector sets.
//
// The properties worth pinning:
//
//   - a device named by a group dials that group's pair; a device no group names
//     gets no rrs key at all, so ungrouped boxes have no RR neighbours and keep
//     the list their own route.json already carries.
//   - a group's slots go through the same ownership gate as the list, so a group
//     can never name a reflector another tenant owns.
//   - the list is replaced as a whole, which is why "one device in two groups" is
//     refused here: the renderer answers membership with a single lookup.
//   - a group is a reference: deleting an RR it names is refused until the group
//     is changed.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rrGroupsSet replaces the whole group list. Sending only rr_groups is a valid
// document: the handler leaves the fields it is not given alone.
func rrGroupsSet(t *testing.T, s *server, sid string, groups ...rrGroupRequest) *httptest.ResponseRecorder {
	t.Helper()
	// A nil slice must still marshal as [], not null: null decodes into "the field
	// was not sent", which is a different instruction from "the list is now empty".
	list := append([]rrGroupRequest{}, groups...)
	body, _ := json.Marshal(map[string]any{"rr_groups": list})
	return overlayDo(t, s, http.MethodPut, "/api/overlay/settings", string(body), sid)
}

func rrGroupWrite(id, name string, devices []uint16, primary, backup string) rrGroupRequest {
	return rrGroupRequest{ID: id, Name: name, DeviceIDs: devices, RRPrimary: primary, RRBackup: backup}
}

func TestRRGroupOverridesReflectorsPerDevice(t *testing.T) {
	s, ten, _, _ := rrFixture(t)
	a := rrCreate(t, s, ten, "RR-A", "192.0.2.10:20200")
	b := rrCreate(t, s, ten, "RR-B", "192.0.2.11:20200")
	c := rrCreate(t, s, ten, "RR-C", "192.0.2.12:20200")
	d := rrCreate(t, s, ten, "RR-D", "192.0.2.13:20200")

	rec := rrGroupsSet(t, s, ten, rrGroupWrite("", "华东站点", []uint16{1}, c, d))
	if rec.Code != http.StatusOK {
		t.Fatalf("存分组 = %d (%s)", rec.Code, rec.Body.String())
	}

	// node 1 is in the group; node 3 is not, so it gets no rrs at all and keeps
	// whatever list its own route.json already carries.
	if rrs := decodedRRs(t, decodeRouteDoc(t, routeArtifact(t, s, "t", 1))); len(rrs) != 2 ||
		rrs[0].ID != c || rrs[1].ID != d {
		t.Fatalf("组内设备的 rrs = %+v，想要 %s/%s", rrs, c, d)
	}
	if doc := decodeRouteDoc(t, routeArtifact(t, s, "t", 3)); doc["rrs"] != nil {
		t.Fatalf("组外设备拿到了 rrs = %v，想要不建邻居", doc["rrs"])
	}

	v := overlayGetView(t, s, ten)
	if len(v.RRGroups) != 1 || v.RRGroups[0].ID != "rg1" || len(v.RRGroups[0].DeviceIDs) != 1 ||
		v.RRGroups[0].DeviceIDs[0] != 1 {
		t.Fatalf("分组列表 = %+v", v.RRGroups)
	}

	// The id survives a save that names it; a row without one gets the next id.
	rec = rrGroupsSet(t, s, ten,
		rrGroupWrite("rg1", "华东站点", []uint16{1, 2}, c, d),
		rrGroupWrite("", "华南站点", []uint16{3}, a, b))
	if rec.Code != http.StatusOK {
		t.Fatalf("改写分组 = %d (%s)", rec.Code, rec.Body.String())
	}
	v = overlayGetView(t, s, ten)
	if len(v.RRGroups) != 2 || v.RRGroups[0].ID != "rg1" || v.RRGroups[1].ID != "rg2" {
		t.Fatalf("分组 id 不稳定: %+v", v.RRGroups)
	}
	if got := v.RRGroups[0].DeviceIDs; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("成员 = %v，想要 [1 2]", got)
	}
	if rrs := decodedRRs(t, decodeRouteDoc(t, routeArtifact(t, s, "t", 3))); len(rrs) != 2 || rrs[0].ID != a {
		t.Fatalf("第二个组没有生效: %+v", rrs)
	}
}

func TestRRGroupRejectsUnpickableOrConflictingLists(t *testing.T) {
	s, ten, two, _ := rrFixture(t)
	mine := rrCreate(t, s, ten, "RR-A", "192.0.2.10:20200")
	foreign := rrCreate(t, s, two, "别人的 RR", "192.0.2.20:20200")

	cases := []struct {
		name string
		body []rrGroupRequest
		want string
	}{
		{"别的租户的 RR", []rrGroupRequest{rrGroupWrite("", "甲", []uint16{1}, foreign, "")}, "不在本租户"},
		{"一台 RR 也没选", []rrGroupRequest{rrGroupWrite("", "乙", []uint16{1}, "", "")}, "至少选一台 RR"},
		{"两格同一条", []rrGroupRequest{rrGroupWrite("", "丙", []uint16{1}, mine, mine)}, "不能选同一条"},
		{"设备不存在", []rrGroupRequest{rrGroupWrite("", "丁", []uint16{77}, mine, "")}, "不属于这个租户"},
		{"设备在两处", []rrGroupRequest{
			rrGroupWrite("", "戊", []uint16{1}, mine, ""),
			rrGroupWrite("", "己", []uint16{1}, mine, ""),
		}, "已经在分组"},
	}
	for _, c := range cases {
		rec := rrGroupsSet(t, s, ten, c.body...)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s：存下来了（%d）", c.name, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s：拒绝话术里没有 %q：%s", c.name, c.want, rec.Body.String())
		}
	}

	// Nothing was written by any of the refusals -- including the id counter, so
	// the first group that does save is rg1 and not rg6.
	if v := overlayGetView(t, s, ten); len(v.RRGroups) != 0 {
		t.Fatalf("拒绝的保存写进了状态: %+v", v.RRGroups)
	}
	if rec := rrGroupsSet(t, s, ten, rrGroupWrite("", "庚", []uint16{2}, mine, "")); rec.Code != http.StatusOK {
		t.Fatalf("合法分组 = %d (%s)", rec.Code, rec.Body.String())
	}
	if v := overlayGetView(t, s, ten); len(v.RRGroups) != 1 || v.RRGroups[0].ID != "rg1" {
		t.Fatalf("被拒绝的保存烧掉了 id: %+v", v.RRGroups)
	}
}

func TestRRGroupIsAReferenceForTheDeleteGuard(t *testing.T) {
	s, ten, _, _ := rrFixture(t)
	mine := rrCreate(t, s, ten, "RR-A", "192.0.2.10:20200")
	other := rrCreate(t, s, ten, "RR-B", "192.0.2.11:20200")
	if rec := rrGroupsSet(t, s, ten, rrGroupWrite("", "华东", []uint16{1}, mine, other)); rec.Code != http.StatusOK {
		t.Fatalf("存分组 = %d (%s)", rec.Code, rec.Body.String())
	}
	rec := rrReq(t, s, http.MethodPost, "/api/rrs/delete", `{"id":"`+mine+`"}`, ten)
	if rec.Code != http.StatusConflict {
		t.Fatalf("删被分组选着的 RR = %d，想要 409 (%s)", rec.Code, rec.Body.String())
	}
	// Dropping the group releases both rows.
	if rec := rrGroupsSet(t, s, ten); rec.Code != http.StatusOK {
		t.Fatalf("清空分组 = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := rrReq(t, s, http.MethodPost, "/api/rrs/delete", `{"id":"`+mine+`"}`, ten); rec.Code != http.StatusOK {
		t.Fatalf("分组删掉后还删不了 RR = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestRRGroupStaleStateIsNamedNotHealed(t *testing.T) {
	s, ten, _, _ := rrFixture(t)
	a := rrCreate(t, s, ten, "RR-A", "192.0.2.10:20200")

	s.mu.Lock()
	oc := s.overlayConfigLocked("t", true)
	oc.RRGroups = []RRGroup{
		{ID: "rg1", Name: "坏组", DeviceIDs: []uint16{1}, RRPrimary: "rr77"},
		{ID: "rg2", Name: "重叠组", DeviceIDs: []uint16{1, 2}, RRPrimary: a},
	}
	s.mu.Unlock()

	v := overlayGetView(t, s, ten)
	msg := hasBlockingCheck(v.Checks)
	if !strings.Contains(msg, "分组") {
		t.Fatalf("分组的问题没被点名: %q", msg)
	}
	if !strings.Contains(fmtChecks(v.Checks), "rr77") || !strings.Contains(fmtChecks(v.Checks), "已经在分组") {
		t.Errorf("两类问题没有都说出来: %+v", v.Checks)
	}
	// The dangling group renders no rrs key for its device; the state is left
	// alone so the tenant can see what they wrote and fix it.
	if doc := decodeRouteDoc(t, routeArtifact(t, s, "t", 1)); doc["rrs"] != nil {
		t.Errorf("解析不出来的分组还是下发了 rrs: %s", doc["rrs"])
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

func TestRRGroupNormalizeAndReload(t *testing.T) {
	s, ten, _, _ := rrFixture(t)
	a := rrCreate(t, s, ten, "RR-A", "192.0.2.10:20200")
	// Unsorted, duplicated device ids are normalized on the way in.
	if rec := rrGroupsSet(t, s, ten, rrGroupWrite("", "  华东 ", []uint16{3, 1, 3}, a, "")); rec.Code != http.StatusOK {
		t.Fatalf("存分组 = %d (%s)", rec.Code, rec.Body.String())
	}
	v := overlayGetView(t, s, ten)
	if got := v.RRGroups[0].DeviceIDs; len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("成员没有规整: %v", got)
	}
	if v.RRGroups[0].Name != "华东" {
		t.Fatalf("组名没有 trim: %q", v.RRGroups[0].Name)
	}

	st, err := s.store.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	oc := st.Overlays["t"]
	if oc == nil || len(oc.RRGroups) != 1 || oc.NextRRGroup != 1 {
		t.Fatalf("分组没存下来: %+v", oc)
	}
	if got := oc.RRGroups[0].DeviceIDs; len(got) != 2 || got[0] != 1 {
		t.Fatalf("盘上的成员没有规整: %v", got)
	}
}
