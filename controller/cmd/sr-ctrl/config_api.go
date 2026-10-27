package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
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
}

func buildConfigView(n *NodeRecord, dc *DeviceConfig) configView {
	cfg := defaultBasicConfig()
	v := configView{NodeID: n.NodeID, Serial: n.Serial}
	if dc != nil {
		cfg = dc.Draft
		v.Version = dc.Version
		v.UpdatedAt = dc.UpdatedAt
		v.UpdatedBy = dc.UpdatedBy
	}
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
