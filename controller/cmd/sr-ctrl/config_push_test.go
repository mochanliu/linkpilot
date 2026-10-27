// Tests for the push channel: publishing freezes a bundle, and the device
// fetches exactly that frozen bundle -- never "whatever the draft says now".
//
// The properties under test are the ones a console cannot show: a publish that
// silently includes an edit made after it, a publish that returns 200 while
// nothing changed, and a device that is told to install a version its tenant
// never published. All three look identical in the UI and all three are wrong.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"sdwan/controller/internal/apply"
	"sdwan/controller/internal/wire"
)

func publishReq(t *testing.T, s *server, sid, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "/api/config/publish", strings.NewReader(body))
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
	}
	if sid != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	}
	rec := httptest.NewRecorder()
	s.handleAdminConfigPublish(rec, r)
	return rec
}

func saveDraft(t *testing.T, s *server, sid string, cfg BasicConfig) configView {
	t.Helper()
	body, _ := json.Marshal(saveConfigRequest{NodeID: 1, Config: cfg})
	return decodeConfigView(t, configRequest(t, s, sid, http.MethodPut, "/api/config", string(body)))
}

// publishFixture: tenant "t" owns node 1, tenant "t2" owns node 2.
func publishFixture(t *testing.T) (*server, string, string) {
	t.Helper()
	s := newTestServer()
	s.store = &fileStore{path: filepath.Join(t.TempDir(), "state.json")}
	s.state.Configs = map[string]*DeviceConfig{}
	addNode(s, "t", 1, nil, "")
	addNode(s, "t2", 2, nil, "")
	ten := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t", Username: "alice", Via: "password"})
	other := consoleSession(t, s, consoleIdentity{Role: roleTenant, TenantID: "t2", Username: "bob", Via: "password"})
	return s, ten, other
}

func TestPublishRefusesWhatTheDeviceWouldRefuse(t *testing.T) {
	s, ten, other := publishFixture(t)
	op := consoleSession(t, s, consoleIdentity{Role: roleOperator, Username: "op", Via: "password"})

	if rec := publishReq(t, s, "", http.MethodPost, `{"node_id":1}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d, want 401", rec.Code)
	}
	if rec := publishReq(t, s, op, http.MethodPost, `{"node_id":1}`); rec.Code != http.StatusForbidden {
		t.Fatalf("operator = %d, want 403", rec.Code)
	}
	if rec := publishReq(t, s, other, http.MethodPost, `{"node_id":1}`); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant = %d, want 404", rec.Code)
	}
	if rec := publishReq(t, s, ten, http.MethodPost, `{"node_id":0}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("node 0 = %d, want 400", rec.Code)
	}
	if rec := publishReq(t, s, ten, http.MethodGet, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", rec.Code)
	}

	// Nothing saved: there is no configuration to publish, and v0 is not one.
	if rec := publishReq(t, s, ten, http.MethodPost, `{"node_id":1}`); rec.Code != http.StatusConflict {
		t.Fatalf("v0 publish = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}

	// A saved draft with a blocking error is refused at the publish gate, not
	// left to fail at the box: the console has no force button by design.
	bad := baseValidConfig()
	bad.Hostname = "bad host!"
	v := saveDraft(t, s, ten, bad)
	if v.Version != 1 || !hasCheck(v.Checks, "error", "主机名") {
		t.Fatalf("setup: draft = v%d %+v", v.Version, v.Checks)
	}
	rec := publishReq(t, s, ten, http.MethodPost, `{"node_id":1}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("draft with errors = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if dc := s.state.Configs[key("t", 1)]; dc.Published != nil {
		t.Fatalf("refused publish still stored a bundle: %+v", dc.Published)
	}
}

func TestPublishedBundleIsFrozenAndIdempotent(t *testing.T) {
	s, ten, _ := publishFixture(t)
	good := baseValidConfig()
	good.Hostname = "edge-1"
	if v := saveDraft(t, s, ten, good); v.Version != 1 {
		t.Fatalf("save = v%d", v.Version)
	}

	rec := publishReq(t, s, ten, http.MethodPost, `{"node_id":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish = %d (%s)", rec.Code, rec.Body.String())
	}
	var published configView
	if err := json.Unmarshal(rec.Body.Bytes(), &published); err != nil {
		t.Fatalf("publish response: %v", err)
	}
	if published.PublishedVersion != 1 || published.PublishedBy != "alice" || published.PublishedAt == 0 {
		t.Fatalf("published header = v%d by %q at %d", published.PublishedVersion, published.PublishedBy, published.PublishedAt)
	}

	dc := s.state.Configs[key("t", 1)]
	if dc.Published == nil || !strings.Contains(dc.Published.Artifacts.FRR, "hostname edge-1") {
		t.Fatalf("published artifacts not frozen: %+v", dc.Published)
	}

	// Re-publishing the same version is not a new instruction: no new timestamp,
	// no new publisher, nothing for the device to do.
	before := *dc.Published
	rec = publishReq(t, s, ten, http.MethodPost, `{"node_id":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("second publish = %d", rec.Code)
	}
	if dc.Published.PublishedAt != before.PublishedAt || dc.Published.Version != before.Version {
		t.Fatalf("re-publish rewrote the snapshot: %+v -> %+v", before, *dc.Published)
	}

	// Editing after publishing must not move what the device is told to run.
	edited := baseValidConfig()
	edited.Hostname = "edge-2"
	if v := saveDraft(t, s, ten, edited); v.Version != 2 {
		t.Fatalf("second save = v%d", v.Version)
	}
	if dc.Published.Version != 1 || !strings.Contains(dc.Published.Artifacts.FRR, "hostname edge-1") {
		t.Fatalf("draft edit leaked into the published bundle: %+v", dc.Published)
	}

	// ... and the device-facing endpoint serves the frozen v1, not the draft v2.
	rec = httptest.NewRecorder()
	s.handleConfigBundle(rec, peerReq(http.MethodGet, "/v1/config/bundle", "", "t", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("bundle = %d (%s)", rec.Code, rec.Body.String())
	}
	b, err := apply.DecodeBundle(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("bundle does not decode as apply.Bundle: %v", err)
	}
	if b.Version != 1 || b.NodeID != 1 || !strings.Contains(b.Artifacts.FRR, "hostname edge-1") {
		t.Fatalf("bundle header/artifacts = v%d node %d: %s", b.Version, b.NodeID, b.Artifacts.FRR)
	}

	// Publishing the new draft supersedes it.
	if rec := publishReq(t, s, ten, http.MethodPost, `{"node_id":1}`); rec.Code != http.StatusOK {
		t.Fatalf("publish v2 = %d", rec.Code)
	}
	if dc.Published.Version != 2 || !strings.Contains(dc.Published.Artifacts.FRR, "hostname edge-2") {
		t.Fatalf("republish did not take v2: %+v", dc.Published)
	}
}

func TestConfigBundleEndpointIsNodeScoped(t *testing.T) {
	s, ten, _ := publishFixture(t)

	// No client certificate at all: this endpoint is mTLS-only.
	rec := httptest.NewRecorder()
	s.handleConfigBundle(rec, httptest.NewRequest(http.MethodGet, "/v1/config/bundle", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no cert = %d, want 401", rec.Code)
	}

	// A certificate for a node that does not exist is a 404, not an empty bundle.
	rec = httptest.NewRecorder()
	s.handleConfigBundle(rec, peerReq(http.MethodGet, "/v1/config/bundle", "", "t", 9))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown node = %d, want 404", rec.Code)
	}

	// A known node with nothing published: 409, so the agent can tell "wait"
	// from "you are not registered".
	rec = httptest.NewRecorder()
	s.handleConfigBundle(rec, peerReq(http.MethodGet, "/v1/config/bundle", "", "t", 1))
	if rec.Code != http.StatusConflict {
		t.Fatalf("nothing published = %d, want 409", rec.Code)
	}

	if v := saveDraft(t, s, ten, baseValidConfig()); v.Version != 1 {
		t.Fatalf("save = v%d", v.Version)
	}
	if rec := publishReq(t, s, ten, http.MethodPost, `{"node_id":1}`); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d", rec.Code)
	}

	// A device of another tenant cannot reach this node's bundle even though the
	// path carries no node parameter: the certificate IS the node.
	rec = httptest.NewRecorder()
	s.handleConfigBundle(rec, peerReq(http.MethodGet, "/v1/config/bundle", "", "t2", 1))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant cert = %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.handleConfigBundle(rec, peerReq(http.MethodPost, "/v1/config/bundle", "", "t", 1))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", rec.Code)
	}
}

func TestHeartbeatCarriesDesiredVersionAndStoresDeviceState(t *testing.T) {
	s := newTestServer()
	s.store = &fileStore{path: filepath.Join(t.TempDir(), "state.json")}
	rec := addNode(s, "t", 1, nil, "")
	other := addNode(s, "t", 2, nil, "")

	beat := func(node uint16, body string) wire.HeartbeatResponse {
		t.Helper()
		out := httptest.NewRecorder()
		s.handleHeartbeat(out, peerReq(http.MethodPost, "/v1/heartbeat", body, "t", node))
		if out.Code != http.StatusOK {
			t.Fatalf("heartbeat = %d (%s)", out.Code, out.Body.String())
		}
		var resp wire.HeartbeatResponse
		if err := json.Unmarshal(out.Body.Bytes(), &resp); err != nil {
			t.Fatalf("heartbeat response: %v", err)
		}
		return resp
	}

	// Nothing published: the device is told 0, which means "there is nothing to
	// install" and must not be read as "install version 0".
	if resp := beat(1, `{"node_id":1}`); resp.ConfigVersion != 0 {
		t.Fatalf("unpublished node told v%d", resp.ConfigVersion)
	}
	if rec.Config != nil {
		t.Fatalf("an agent that reports no config must not be recorded as reporting one: %+v", rec.Config)
	}

	s.state.Configs = map[string]*DeviceConfig{configKey("t", 1): {
		TenantID: "t", NodeID: 1, Version: 3,
		Published: &PublishedBundle{Version: 3, PublishedAt: 100, Artifacts: renderArtifacts(&BasicConfig{})},
	}}
	if resp := beat(1, `{"node_id":1}`); resp.ConfigVersion != 3 {
		t.Fatalf("desired = %d, want 3", resp.ConfigVersion)
	}
	if resp := beat(2, `{"node_id":2}`); resp.ConfigVersion != 0 {
		t.Fatalf("node 2 desired = %d, want 0", resp.ConfigVersion)
	}

	// The device's own report is stored verbatim, persisted, and survives an
	// agent that stops reporting (an old binary sends no Config at all).
	body := `{"node_id":1,"config":{"capable":true,"version":3,"ok":true,"applied_at_unix":111,"source":"apply"}}`
	beat(1, body)
	if rec.Config == nil || rec.Config.Version != 3 || !rec.Config.OK || rec.Config.Source != "apply" {
		t.Fatalf("stored config state = %+v", rec.Config)
	}
	beat(1, `{"node_id":1}`)
	if rec.Config == nil || rec.Config.Version != 3 {
		t.Fatalf("a heartbeat without config erased the report: %+v", rec.Config)
	}
	st, err := s.store.Load()
	if err != nil {
		t.Fatalf("state reload: %v", err)
	}
	saved := st.Nodes[key("t", 1)]
	if saved == nil || saved.Config == nil || saved.Config.Version != 3 || !saved.Config.OK {
		t.Fatalf("config state did not persist: %+v", saved)
	}
	if other.Config != nil {
		t.Fatalf("node 2 picked up node 1's state: %+v", other.Config)
	}
}
