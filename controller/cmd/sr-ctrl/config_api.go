package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sdwan/controller/internal/wire"
)

type configView struct {
	NodeID    uint16          `json:"node_id"`
	Serial    string          `json:"serial"`
	Config    BasicConfig     `json:"config"`
	Checks    []ConfigCheck   `json:"checks"`
	Artifacts configArtifacts `json:"artifacts"`
	Version   int             `json:"version"`
	UpdatedAt int64           `json:"updated_at_unix"`
	UpdatedBy string          `json:"updated_by,omitempty"`

	// Published is what the device has been TOLD to install; Version above is
	// only what was saved. They differ whenever a tenant edits after publishing,
	// and the page must show both rather than assuming the newest draft won.
	PublishedVersion int              `json:"published_version,omitempty"`
	PublishedAt      int64            `json:"published_at_unix,omitempty"`
	PublishedBy      string           `json:"published_by,omitempty"`
	Device           *wire.ConfigState `json:"device_state,omitempty"`
}

func buildConfigView(n *NodeRecord, dc *DeviceConfig) configView {
	cfg := defaultBasicConfig()
	v := configView{NodeID: n.NodeID, Serial: n.Serial}
	if dc != nil {
		cfg = dc.Draft
		v.Version = dc.Version
		v.UpdatedAt = dc.UpdatedAt
		v.UpdatedBy = dc.UpdatedBy
		if dc.Published != nil {
			v.PublishedVersion = dc.Published.Version
			v.PublishedAt = dc.Published.PublishedAt
			v.PublishedBy = dc.Published.PublishedBy
		}
	}
	v.Device = n.Config
	normalizeBasicConfig(&cfg)
	v.Config = cfg
	v.Checks = validateBasicConfig(&cfg)
	v.Artifacts = renderArtifacts(&cfg)
	return v
}

type saveConfigRequest struct {
	NodeID uint16      `json:"node_id"`
	Config BasicConfig `json:"config"`
}

// configBundle is what the device-side applier consumes. It deliberately
// carries the rendered artifacts, not the model: the controller owns the
// FRR/VPP/kernel split, and the box must not re-implement it. Checks travel
// with the bundle so the applier can refuse a draft that still has errors.
// The JSON shape is shared with internal/apply.Bundle.
type configBundle struct {
	NodeID      uint16          `json:"node_id"`
	Version     int             `json:"version"`
	GeneratedAt int64           `json:"generated_at_unix"`
	Checks      []ConfigCheck   `json:"checks"`
	Artifacts   configArtifacts `json:"artifacts"`
}

// handleAdminConfig is the basic-config draft endpoint. It is tenant-only:
// the operator role may see that a device exists, never how it is configured.
// Saving accepts a config that still has errors -- a half-written policy is a
// normal intermediate state -- but the response carries the checks so the page
// can show exactly what blocks a future apply.
func (s *server) handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	if id.Role != roleTenant {
		http.Error(w, "tenant role required", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPut:
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
			return
		}
	default:
		http.Error(w, "GET or PUT required", http.StatusMethodNotAllowed)
		return
	}

	var nodeID uint16
	var req saveConfigRequest
	if r.Method == http.MethodGet {
		n, err := strconv.ParseUint(strings.TrimSpace(r.URL.Query().Get("node")), 10, 16)
		if err != nil || n == 0 {
			http.Error(w, "node must be 1..65535", http.StatusBadRequest)
			return
		}
		nodeID = uint16(n)
	} else {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, cfgMaxBody)).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.NodeID == 0 {
			http.Error(w, "node_id must be 1..65535", http.StatusBadRequest)
			return
		}
		nodeID = req.NodeID
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.state.Nodes[key(id.TenantID, nodeID)]
	if n == nil || n.TenantID != id.TenantID {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	ck := configKey(id.TenantID, nodeID)
	if r.Method == http.MethodGet {
		writeJSON(w, buildConfigView(n, s.state.Configs[ck]))
		return
	}

	cfg := req.Config
	normalizeBasicConfig(&cfg)
	dc := s.state.Configs[ck]
	if dc == nil {
		dc = &DeviceConfig{TenantID: id.TenantID, NodeID: nodeID}
		s.state.Configs[ck] = dc
	}
	dc.Draft = cfg
	dc.Version++
	dc.UpdatedAt = time.Now().Unix()
	dc.UpdatedBy = id.Username
	s.save()
	writeJSON(w, buildConfigView(n, dc))
}

// handleAdminConfigBundle hands a tenant the exact artifacts the box should
// install. Delivery is still manual (curl the bundle, copy it to the device),
// so this is a download endpoint, not a push channel. An unsaved node has no
// bundle: applying version 0 would claim the device has something it does not.
func (s *server) handleAdminConfigBundle(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	if id.Role != roleTenant {
		http.Error(w, "tenant role required", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	n, err := strconv.ParseUint(strings.TrimSpace(r.URL.Query().Get("node")), 10, 16)
	if err != nil || n == 0 {
		http.Error(w, "node must be 1..65535", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.state.Nodes[key(id.TenantID, uint16(n))]
	if node == nil || node.TenantID != id.TenantID {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	view := buildConfigView(node, s.state.Configs[configKey(id.TenantID, uint16(n))])
	if view.Version == 0 {
		http.Error(w, "no saved draft for this node", http.StatusConflict)
		return
	}
	writeJSON(w, configBundle{
		NodeID:      view.NodeID,
		Version:     view.Version,
		GeneratedAt: time.Now().Unix(),
		Checks:      view.Checks,
		Artifacts:   view.Artifacts,
	})
}
