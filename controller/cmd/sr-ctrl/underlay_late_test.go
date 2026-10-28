// Late enrollment. A customer does not put every device on the network at once,
// and the tenant's global configuration is usually published before the last
// box arrives. Publishing only handed transit ports to the devices that existed
// at that moment, so without the enrollment/heartbeat hook a later device carries
// transit_port 0 forever: nothing can relay through it, and "click publish again"
// is not something an operator should have to remember.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"sdwan/controller/internal/underlay"
	"sdwan/controller/internal/wire"
)

func enrollSerial(t *testing.T, s *server, claim, serial string) uint16 {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleEnroll(rec, enrollReqSerial(t, claim, serial))
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll %s = %d (%s)", serial, rec.Code, rec.Body.String())
	}
	var resp wire.EnrollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("enroll response: %v (%s)", err, rec.Body.String())
	}
	return resp.NodeID
}

func underlayBundleReq(t *testing.T, s *server, tenant string, node uint16) wire.UnderlayBundle {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleUnderlayBundle(rec, peerReq(http.MethodGet, "/v1/underlay/bundle", "", tenant, node))
	if rec.Code != http.StatusOK {
		t.Fatalf("bundle for node %d = %d (%s)", node, rec.Code, rec.Body.String())
	}
	var b wire.UnderlayBundle
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("bundle: %v (%s)", err, rec.Body.String())
	}
	return b
}

// The first device enrolls while nothing is published -- no port may be invented
// for it then. The tenant publishes with only that device enrolled; the second
// device, arriving afterwards, must get its port from the enrollment path and see
// it in the bundle it fetches.
func TestLateEnrollmentIsGivenItsTransitPort(t *testing.T) {
	claim := &Claim{ID: "clm_test", TenantID: "t", MaxUses: 5, CodeHash: hashClaimCode("c")}
	s := enrollFixture(t, claimMap{"clm_test": claim})

	first := enrollSerial(t, s, "c", "cpe-1")
	s.mu.Lock()
	if uc := s.underlayConfigLocked("t", false); uc != nil {
		t.Fatalf("入网不该凭空创建 underlay 记录: %+v", uc)
	}
	uc := s.underlayConfigLocked("t", true)
	if err := uc.State.PublishSettings(underlay.DefaultSettings(), []uint16{first}, 1000); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if uc.State.TransitPorts[first] != 15000 {
		t.Fatalf("发布时入网的设备应有端口，实际 %v", uc.State.TransitPorts)
	}
	gen := uc.State.Gen
	s.mu.Unlock()

	late := enrollSerial(t, s, "c", "cpe-2")
	s.mu.Lock()
	if uc.State.TransitPorts[late] != 15000 {
		t.Fatalf("发布后入网的设备 %d 没拿到中转端口: %v", late, uc.State.TransitPorts)
	}
	// The version must move: that is what tells every already-running device that
	// its snapshot changed, and what makes this device pull in the first place.
	if uc.State.Gen != gen+1 {
		t.Fatalf("补分配没有推进下发版本: %d -> %d", gen, uc.State.Gen)
	}
	wantGen := uc.State.Gen
	s.mu.Unlock()

	b := underlayBundleReq(t, s, "t", late)
	if b.TransitPort != 15000 || b.Version != wantGen {
		t.Fatalf("晚入网设备的 bundle 应带上下自己的端口，实际 %+v", b)
	}
}

// A device that enrolled while the pool could not cover it -- or one whose row
// predates this hook entirely -- is repaired by its next heartbeat rather than
// waiting for someone to re-publish.
func TestHeartbeatRepairsAMissingTransitPort(t *testing.T) {
	s := newTestServer()
	s.store = nopStore{}
	s.admin = newAdminHub()
	node := addNode(s, "t", 1, nil, "198.51.100.1:40000")

	s.mu.Lock()
	uc := s.underlayConfigLocked("t", true)
	// Published with nobody enrolled: exactly the state a restored or older row
	// can be in.
	if err := uc.State.PublishSettings(underlay.DefaultSettings(), nil, 1000); err != nil {
		t.Fatalf("publish: %v", err)
	}
	gen := uc.State.Gen
	s.mu.Unlock()

	out := httptest.NewRecorder()
	body, _ := json.Marshal(wire.HeartbeatRequest{NodeID: node.NodeID})
	s.handleHeartbeat(out, peerReq(http.MethodPost, "/v1/heartbeat", string(body), "t", node.NodeID))
	if out.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d (%s)", out.Code, out.Body.String())
	}
	var resp wire.HeartbeatResponse
	if err := json.Unmarshal(out.Body.Bytes(), &resp); err != nil {
		t.Fatalf("heartbeat response: %v", err)
	}

	s.mu.Lock()
	assigned := uc.State.TransitPorts[node.NodeID]
	newGen := uc.State.Gen
	s.mu.Unlock()
	if assigned != 15000 {
		t.Fatalf("心跳没有补分配中转端口: %v", uc.State.TransitPorts)
	}
	if resp.TunnelVersion != newGen || newGen != gen+1 {
		t.Fatalf("补分配后的版本没有告知设备: beat=%d gen=%d->%d", resp.TunnelVersion, gen, newGen)
	}

	// Idempotent: the next beat must not bump the version again and make every
	// device re-pull for nothing.
	out = httptest.NewRecorder()
	s.handleHeartbeat(out, peerReq(http.MethodPost, "/v1/heartbeat", string(body), "t", node.NodeID))
	s.mu.Lock()
	after := uc.State.Gen
	s.mu.Unlock()
	if after != newGen {
		t.Fatalf("第二次心跳重复推进了版本: %d -> %d", newGen, after)
	}
}

// Publishing after a late device enrolled must not renumber it: the port is in
// peers' SR segments already.
func TestLateEnrollmentPortSurvivesRepublish(t *testing.T) {
	claim := &Claim{ID: "clm_test", TenantID: "t", MaxUses: 5, CodeHash: hashClaimCode("c")}
	s := enrollFixture(t, claimMap{"clm_test": claim})
	first := enrollSerial(t, s, "c", "cpe-1")

	s.mu.Lock()
	uc := s.underlayConfigLocked("t", true)
	if err := uc.State.PublishSettings(underlay.DefaultSettings(), []uint16{first}, 1000); err != nil {
		t.Fatalf("publish: %v", err)
	}
	s.mu.Unlock()

	late := enrollSerial(t, s, "c", "cpe-2")
	s.mu.Lock()
	defer s.mu.Unlock()
	next := uc.State.Settings
	next.ProbeIntervalMS = 400
	if err := uc.State.PublishSettings(next, []uint16{first, late}, 2000); err != nil {
		t.Fatalf("re-publish: %v", err)
	}
	if uc.State.TransitPorts[late] != 15000 || uc.State.TransitPorts[first] != 15000 {
		t.Fatalf("重新发布移动了已分配的端口: %v", uc.State.TransitPorts)
	}
}
