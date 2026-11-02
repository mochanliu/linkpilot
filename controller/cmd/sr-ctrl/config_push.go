package main

// The push channel: the console PUBLISHES a frozen bundle, and the device PULLS
// it on its own schedule.
//
// The direction is the whole design. The controller never dials a CPE -- it
// cannot, the boxes are behind NAT -- so a heartbeat carries the published
// version to the device and the device fetches the bundle over the mTLS
// identity it already uses for everything else. That is the same shape
// enrollment and the peer registry rely on, and it means the channel works in
// the lab and on a customer uplink without a second transport.
//
// Nothing here retries on the device's behalf: the agent owns retry, because it
// is the only party that knows whether the failure was "fetch" or "FRR would
// not restart".

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"sdwan/controller/internal/apply"
)

type publishRequest struct {
	NodeID uint16 `json:"node_id"`
}

// publishConfigLocked freezes the current draft as what the device should run.
// It reports whether anything changed: re-publishing an identical bundle is not a
// new instruction and must not bump any timestamp the page shows.
//
// The overlay and leak layers are part of what is published, so an edit to a
// 导通组 or a prefix-list counts as a change even when the draft text did not
// move -- and it has to produce a NEW version number. The box's store treats the
// version as identity (same number with different content is refused as "already
// installed with different content"), so reusing the old number would mean the
// change never reaches the device. Bumping it keeps one numbering scheme for the
// page's "有没有未发布的改动".
//
// The comparison is over every artifact, not just route.json: a prefix-list edit
// can change FRR and the srfrr segment without touching route.json or the draft
// version, and that is exactly the "重新发布引用它的设备" case.
func (s *server) publishConfigLocked(tenantID string, nodeID uint16, dc *DeviceConfig,
	checks []ConfigCheck, by string, now int64) bool {
	art := s.artifactsLocked(tenantID, nodeID, &dc.Draft, dc.Leak)
	if dc.Published != nil {
		if dc.Published.Version == dc.Version && dc.Published.Artifacts == art {
			return false
		}
		if dc.Published.Version == dc.Version {
			dc.Version++
		}
	}
	dc.Published = &PublishedBundle{
		Version:     dc.Version,
		PublishedAt: now,
		PublishedBy: by,
		Checks:      checks,
		Artifacts:   art,
	}
	return true
}

// publishDeviceLocked is the one publish body every console entry point uses
// (基础配置 page, 泄露 device row, prefix-list republish, batch). It adds the
// event the console's SSE stream listens for; the callers still own save().
func (s *server) publishDeviceLocked(tenantID string, nodeID uint16, dc *DeviceConfig,
	checks []ConfigCheck, by string, now int64) bool {
	changed := s.publishConfigLocked(tenantID, nodeID, dc, checks, by, now)
	if changed {
		if n := s.state.Nodes[key(tenantID, nodeID)]; n != nil {
			s.admin.publish("config.published", tenantID, n)
		}
	}
	return changed
}

// handleAdminConfigPublish is the tenant's explicit "make the device run this".
// Publishing is refused -- not silently downgraded -- when:
//
//   - nothing was ever saved (version 0): there is no configuration to send;
//   - the saved draft has blocking errors: the device would refuse it anyway,
//     and a publish that always fails at the box is noise in the console.
//
// The force override lives on the device CLI (`sr-apply apply --force`), where
// someone has to type it and can see the machine. There is no force button here
// on purpose: the console speaks for a tenant, not for the box's operator.
func (s *server) handleAdminConfigPublish(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	if id.Role != roleTenant {
		http.Error(w, "tenant role required", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
		return
	}
	var req publishRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, cfgMaxBody)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.NodeID == 0 {
		http.Error(w, "node_id must be 1..65535", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.state.Nodes[key(id.TenantID, req.NodeID)]
	if n == nil || n.TenantID != id.TenantID {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	dc := s.state.Configs[configKey(id.TenantID, req.NodeID)]
	if dc == nil || dc.Version == 0 {
		http.Error(w, "这台设备还没有保存过草稿（v0），没有可发布的配置", http.StatusConflict)
		return
	}
	checks := s.publishChecksLocked(id.TenantID, req.NodeID, dc)
	for _, c := range checks {
		if c.Severity == "error" {
			http.Error(w, fmt.Sprintf("草稿 v%d 还有一个校验错误：%s先在页面上改掉——强制下发只在设备侧 CLI（sr-apply --force）",
				dc.Version, c.Text), http.StatusConflict)
			return
		}
	}
	if s.publishDeviceLocked(id.TenantID, req.NodeID, dc, checks, id.Username, time.Now().Unix()) {
		s.save()
	}
	writeJSON(w, s.buildConfigView(n, dc))
}

// handleConfigBundle is the device-facing half. Identity comes from the client
// certificate -- there is no node parameter, so a device cannot even ask for
// another node's bundle.
func (s *server) handleConfigBundle(w http.ResponseWriter, r *http.Request) {
	tenant, nodeID, err := peerTenantFromCert(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Nodes[key(tenant, nodeID)]; !ok {
		http.Error(w, "unknown node", http.StatusNotFound)
		return
	}
	dc := s.state.Configs[configKey(tenant, nodeID)]
	if dc == nil || dc.Published == nil {
		// 409 and not 404: the node is known, it just has nothing to install.
		// The agent keys its retry on this distinction, and so does a person
		// reading the log ("unknown node" is a different problem).
		http.Error(w, "这台设备没有已发布的配置", http.StatusConflict)
		return
	}
	writeJSON(w, bundleFor(dc.Published, nodeID))
}

// desiredConfigVersionLocked is what a heartbeat advertises. 0 means "nothing
// published": the agent then has nothing to fetch and must not interpret it as
// "install version 0".
func (s *server) desiredConfigVersionLocked(tenant string, nodeID uint16) int {
	dc := s.state.Configs[configKey(tenant, nodeID)]
	if dc == nil || dc.Published == nil {
		return 0
	}
	return dc.Published.Version
}

// bundleFor converts the stored snapshot into the wire type the on-box applier
// decodes (internal/apply.Bundle). The conversion is deliberate: the controller
// keeps its own artifact/check types (it renders them), and the shared JSON
// shape is what the two sides actually agree on.
func bundleFor(p *PublishedBundle, nodeID uint16) apply.Bundle {
	checks := make([]apply.Check, 0, len(p.Checks))
	for _, c := range p.Checks {
		checks = append(checks, apply.Check{Severity: c.Severity, Text: c.Text})
	}
	return apply.Bundle{
		NodeID:      nodeID,
		Version:     p.Version,
		GeneratedAt: p.PublishedAt,
		Checks:      checks,
		Artifacts: apply.Artifacts{
			FRR:   p.Artifacts.FRR,
			VPP:   p.Artifacts.VPP,
			Boot:  p.Artifacts.Boot,
			Route: p.Artifacts.Route,
		},
	}
}
