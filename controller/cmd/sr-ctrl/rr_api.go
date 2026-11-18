package main

// The RR list's HTTP surface, tenant-side (2026-11-01 定案: an RR belongs to one
// tenant, so its tenant is the only one who manages it).
//
// Reads do not live here. A tenant sees its list inside GET /api/overlay
// (overlayView.RRLibrary), together with the devices whose RR 1 / RR 2 name those
// rows -- filtered by ownership in one place, so "the page showed it" and "the
// device will be told about it" cannot drift apart.
//
// Nothing here publishes anything. A row is an input to the 4th artifact, so an
// edit changes what the NEXT publish renders for every device of this tenant, and
// a box that is running now keeps the addresses it already has until that publish
// lands.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"sdwan/controller/internal/keybinding"
)

// rrWriteRequest is the body for create and edit. The whole row is sent both
// times: a page that shows a row lets you change any part of it, and a PATCH-like
// "only what changed" contract would mean a field the page forgot to send keeps a
// value nobody is looking at.
//
// There is no 归属 field: the row belongs to the session's tenant, full stop, and
// accepting one from the body would be a second, forgeable source for the one
// thing that decides whose devices dial this reflector.
type rrWriteRequest struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	Addr     string `json:"addr"`
	Location string `json:"location"`
}

type rrIDRequest struct {
	ID string `json:"id"`
}

// rrChangeNote is what every write answers with, because the two facts in it are
// exactly the ones a tenant is going to ask about after adding a reflector: who
// is affected, and when it takes effect.
const rrChangeNote = "改动只影响下一次发布：设备要在「配置 › 基础配置」发布以后才会换成新的 RR 列表。" +
	"网格里的其它 RR 下一次来拉配置（≤ 轮询间隔，默认 30s）就按新清单增删邻居，已在跑的会话不重建。" +
	"控制器不会去连这些地址，所以这里没有任何连通性结论可给。"

// rrResponseLocked writes the tenant's whole list back after every mutation. The
// list comes back rather than just the touched row because a delete guard and a
// renamed id are exactly the things one edit changes for the other rows.
func (s *server) rrResponseLocked(w http.ResponseWriter, tenantID string, extra map[string]any) {
	out := map[string]any{
		"ok":       true,
		"rrs":      s.rrRowsLocked(tenantID),
		"max_rrs":  rrMaxPerTenant,
		"note":     rrChangeNote,
		"now_unix": time.Now().Unix(),
	}
	for k, v := range extra {
		out[k] = v
	}
	writeJSON(w, out)
}

// handleTenantRRs: GET lists this tenant's rows, POST adds one, PUT edits one.
func (s *server) handleTenantRRs(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		defer s.mu.Unlock()
		s.rrResponseLocked(w, id.TenantID, nil)
		return
	case http.MethodPost, http.MethodPut:
	default:
		methodNotAllowed(w, "GET, POST, PUT")
		return
	}

	var req rrWriteRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	addr := strings.TrimSpace(req.Addr)
	location := strings.TrimSpace(req.Location)
	if err := validRRName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validRRAddr(addr); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validRRLocation(location); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	lib := s.rrLibraryLocked(true)
	now := time.Now().Unix()
	rows := lib.visibleTo(id.TenantID)

	// One address, one session. Two rows with the same host:port would make the
	// device dial the same reflector twice and hear every prefix twice, which
	// looks like a second path and is not one. Checked on both create and edit,
	// against the rows that will remain.
	addrTaken := func(exceptID string) bool {
		for _, it := range rows {
			if it.ID != exceptID && it.Addr == addr {
				return true
			}
		}
		return false
	}

	if r.Method == http.MethodPost {
		if strings.TrimSpace(req.ID) != "" {
			http.Error(w, "新建 RR 不应带 id", http.StatusBadRequest)
			return
		}
		if len(rows) >= rrMaxPerTenant {
			// The list is also the default per-device fan-out, so the cap is not a
			// table limit -- it is a session count on every device of this tenant.
			http.Error(w, fmt.Sprintf("本租户已有 %d 条 RR，达到了上限：没被反射器分组点名的设备会拨每一条，"+
				"再多只会加重每台设备的会话数", rrMaxPerTenant), http.StatusConflict)
			return
		}
		if addrTaken("") {
			http.Error(w, fmt.Sprintf("本租户已经有一条 RR 用的是 %s：一台盒子两个身份会把同一份路由反射两遍", addr),
				http.StatusBadRequest)
			return
		}
		it := RRItem{ID: lib.nextID(), Name: name, Addr: addr, Location: location,
			TenantID: id.TenantID, CreatedAt: now}
		lib.Items = append(lib.Items, it)
		s.rrTouchLocked(lib, id.Username, now)
		s.save()
		log.Printf("rrlib: tenant=%s created %s addr=%s by %s", id.TenantID, it.ID, it.Addr, id.Username)
		s.rrResponseLocked(w, id.TenantID, map[string]any{"rr": s.rrRowOfLocked(it)})
		return
	}

	targetID := strings.TrimSpace(req.ID)
	it := lib.get(targetID)
	if it == nil || it.TenantID != id.TenantID {
		// Same answer as a row that never existed: whether another tenant has a row
		// with this id is not something this tenant may learn by probing.
		http.Error(w, "RR 不存在", http.StatusNotFound)
		return
	}
	if addrTaken(targetID) {
		http.Error(w, fmt.Sprintf("本租户已经有一条 RR 用的是 %s：一台盒子两个身份会把同一份路由反射两遍", addr),
			http.StatusBadRequest)
		return
	}
	before := *it
	it.Name, it.Addr, it.Location = name, addr, location
	it.UpdatedAt = now
	// The address is what the device dials, so a change here is the one edit that
	// is worth a log line even when nothing else moved.
	addrChanged := before.Addr != it.Addr
	s.rrTouchLocked(lib, id.Username, now)
	s.save()
	log.Printf("rrlib: tenant=%s updated %s addr=%s addrChanged=%t by %s",
		id.TenantID, it.ID, it.Addr, addrChanged, id.Username)
	s.rrResponseLocked(w, id.TenantID, map[string]any{
		"rr":           s.rrRowOfLocked(*it),
		"addr_changed": addrChanged,
	})
}

// handleTenantRRDelete refuses while one of this tenant's devices still selects
// the row (拍板 E5). The alternative -- letting the delete through and silently
// dropping that device's choice -- would leave the box rendering no rrs at all,
// and "my box has no reflector" is a far worse surprise than a delete button that
// explains itself.
func (s *server) handleTenantRRDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	var req rrIDRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	targetID := strings.TrimSpace(req.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	lib := s.rrLibraryLocked(false)
	it := lib.get(targetID)
	if it == nil || it.TenantID != id.TenantID {
		http.Error(w, "RR 不存在", http.StatusNotFound)
		return
	}
	if refs := s.rrBindRefsLocked(id.TenantID, targetID); len(refs) > 0 {
		names := make([]string, 0, len(refs))
		for _, ref := range refs {
			if ref.Name != "" {
				names = append(names, fmt.Sprintf("%s（node-%d）", ref.Name, ref.NodeID))
				continue
			}
			names = append(names, "node-"+strconv.Itoa(int(ref.NodeID)))
		}
		http.Error(w, fmt.Sprintf("这条 RR 还被 %d 台设备选着（%s）：先在导通策略的 RR 1 / RR 2 列里给这些设备换一条，再来删",
			len(refs), strings.Join(names, "、")), http.StatusConflict)
		return
	}
	idx := indexOfRRLocked(lib, targetID)
	// Copy before the splice: lib.Items is one array, so shifting the tail left
	// moves a *different* row onto this index, and a pointer taken before the delete
	// would name the wrong RR in the log.
	removed := lib.Items[idx]
	// The id is not handed back to the counter: lib.NextID keeps climbing, so a
	// row that appears later is rr<n+1> and never a resurrected rr<n>. That matters
	// because a device's RR 1 / RR 2 slot is a plain string that outlives nothing
	// else.
	lib.Items = append(lib.Items[:idx], lib.Items[idx+1:]...)
	s.rrTouchLocked(lib, id.Username, time.Now().Unix())
	s.save()
	log.Printf("rrlib: tenant=%s deleted %s addr=%s by %s", id.TenantID, removed.ID, removed.Addr, id.Username)
	s.rrResponseLocked(w, id.TenantID, map[string]any{"deleted": targetID})
}

// ---------------------------------------------------------------- identities

// rrIdentityRequest is all a signing request carries. The tenant and address are
// NOT accepted from the body: the session decides the tenant and the row decides
// the address, so no request can sign a certificate for somebody else's row or
// for an address the operator never typed.
type rrIdentityRequest struct {
	ID string `json:"id"`
}

// rrIdentityResponse is the one and only delivery of the private key. It is not
// cached, not logged, and not stored: once this response is gone the controller
// cannot reproduce it, and losing the box's copy means signing again.
type rrIdentityResponse struct {
	OK          bool   `json:"ok"`
	CertPEM     string `json:"cert_pem"`
	KeyPEM      string `json:"key_pem"`
	CAPEM       string `json:"ca_pem"`
	RRID        string `json:"rr_id"`
	Tenant      string `json:"tenant"`
	Addr        string `json:"addr"`
	Serial      string `json:"serial"`
	Fingerprint string `json:"fingerprint"`
	IssuedAt    int64  `json:"issued_at_unix"`
	Note        string `json:"note"`
	RR          rrRow  `json:"rr"`
}

const rrIdentityNote = "私钥只出现在这一次响应里，关掉就取不回来——控制器不缓存、日志里也没有。" +
	"把它和证书一起装到这台 RR 上；再点一次「签发身份」会换一对新密钥，旧证书在到期前仍然有效（控制器没有吊销）。"

// handleTenantRRIdentity signs the row's certificate. A second call is a
// re-issuance: new key, new serial, and the row's identity metadata is replaced.
//
// The old certificate is not revoked -- there is no CRL/OCSP in this platform
// (device certificates have the same gap). The one cheap close is on the RR
// side: a deleted row's /v1/rr/config is a 404, and sr-rr treats that as "I no
// longer exist" and stops serving.
func (s *server) handleTenantRRIdentity(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	var req rrIdentityRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	targetID := strings.TrimSpace(req.ID)
	if targetID == "" {
		http.Error(w, "缺少 id", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	lib := s.rrLibraryLocked(false)
	it := lib.get(targetID)
	if it == nil || it.TenantID != id.TenantID {
		http.Error(w, "RR 不存在", http.StatusNotFound)
		return
	}
	host := rrAddrHost(it.Addr)
	if host == "" {
		// Every address saved through the API has a host; reaching this means the
		// state file was edited. A certificate with no SAN is one no device can
		// complete a handshake with, so it is better to refuse than to hand out.
		http.Error(w, "这条 RR 的地址没有可用的主机部分，先改地址再签发", http.StatusConflict)
		return
	}
	if s.caCert == nil || len(s.caKey) == 0 {
		http.Error(w, "控制器没有 CA，签不出身份", http.StatusInternalServerError)
		return
	}
	caPEM, err := os.ReadFile(filepath.Join(s.dataDir, "ca.pem"))
	if err != nil {
		http.Error(w, "读不到控制器 CA", http.StatusInternalServerError)
		return
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		http.Error(w, "生成密钥失败", http.StatusInternalServerError)
		return
	}
	certPEM, err := signRR(s.caCert, s.caKey, pub, it.TenantID, it.ID, host)
	if err != nil {
		http.Error(w, "签发失败", http.StatusInternalServerError)
		return
	}
	blk, _ := pem.Decode([]byte(certPEM))
	if blk == nil {
		http.Error(w, "签发失败", http.StatusInternalServerError)
		return
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		http.Error(w, "签发失败", http.StatusInternalServerError)
		return
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		http.Error(w, "生成密钥失败", http.StatusInternalServerError)
		return
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))

	sum := sha256.Sum256(cert.Raw)
	now := time.Now().Unix()
	it.Identity = &RRIdentity{
		Serial:      cert.SerialNumber.Text(16),
		Fingerprint: hex.EncodeToString(sum[:16]),
		IssuedAt:    now,
		Tenant:      it.TenantID,
		RRID:        it.ID,
		SAN:         []string{host},
	}
	it.UpdatedAt = now
	s.rrTouchLocked(lib, id.Username, now)
	s.save()
	// The fingerprint identifies the certificate; the key never appears in a log
	// line, a state file, or any other response.
	log.Printf("rrlib: tenant=%s issued identity %s serial=%s fp=%s san=%s by %s",
		id.TenantID, it.ID, it.Identity.Serial, it.Identity.Fingerprint, host, id.Username)

	writeJSON(w, rrIdentityResponse{
		OK: true, CertPEM: certPEM, KeyPEM: keyPEM, CAPEM: string(caPEM),
		RRID: it.ID, Tenant: it.TenantID, Addr: it.Addr,
		Serial: it.Identity.Serial, Fingerprint: it.Identity.Fingerprint,
		IssuedAt: now, Note: rrIdentityNote, RR: s.rrRowOfLocked(*it),
	})
}

// ---------------------------------------------------------------- RR config

type rrPeer struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
}

// rrConfigResponse is what an RR pulls to learn its grid. Only the peers: which
// local interface to listen on and how often to poll are the box's own business
// and stay out of the controller's answer.
type rrConfigResponse struct {
	Tenant string   `json:"tenant"`
	RRID   string   `json:"rr_id"`
	Peers  []rrPeer `json:"peers"`
	// Controllers is where this RR can reach the control plane: the same list a
	// device gets on every heartbeat (the active members' endpoints). Without it
	// an RR is pinned to the one address in its config file -- the controller it
	// happens to start against -- and a controller that goes away freezes the
	// grid: no new rows, no address changes, only the same error every poll.
	//
	// Empty means an older controller (or a single node that cannot name
	// itself); the RR then keeps using its configured address, which is exactly
	// the old behaviour.
	Controllers     []string `json:"controllers,omitempty"`
	Leader          string   `json:"leader,omitempty"`
	GeneratedAtUnix int64    `json:"generated_at_unix"`
}

// handleRRConfig is the grid's read side: an RR presents its certificate and
// gets every other row of its tenant, sorted by id.
//
// The certificate is the authorization. A node certificate is refused (401)
// because "a device asking for RR peers" is not a thing, and a tenant other than
// the row's owner gets the same 404 as an unknown id -- whether some other
// tenant has a row with this id is not something a probe may learn.
func (s *server) handleRRConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "需要客户端证书", http.StatusUnauthorized)
		return
	}
	ref, err := keybinding.RefOf(r.TLS.PeerCertificates[0])
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if ref.Role != keybinding.RoleRR {
		http.Error(w, "这是设备身份，不是 RR 身份", http.StatusUnauthorized)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	lib := s.rrLibraryLocked(false)
	it := lib.get(ref.RRID)
	if it == nil || it.TenantID != ref.Tenant {
		// A deleted row must be able to tell that it is gone: this is the whole
		// replacement for a revocation list. sr-rr stops serving on this answer.
		http.Error(w, "RR 不存在", http.StatusNotFound)
		return
	}
	peers := []rrPeer{}
	for _, row := range lib.visibleTo(ref.Tenant) {
		if row.ID == it.ID {
			continue
		}
		peers = append(peers, rrPeer{ID: row.ID, Addr: row.Addr})
	}
	now := time.Now().Unix()
	// 只记在本机内存里：这是"它刚来过"的读数，不是配置。写进硬状态会让集群
	// 每 5 秒为所有 RR 的轮询提交一轮共识（lab 实测，见 server.rrFetch）。
	s.noteRRFetchLocked(it.ID, now, remoteHost(r.RemoteAddr))

	// 已经持有 s.mu（上面 defer 的就是它），所以取 Locked 变体：memberEndpoints()
	// 自己会再取一次锁，在这里就是自死锁。
	writeJSON(w, rrConfigResponse{
		Tenant: ref.Tenant, RRID: it.ID, Peers: peers,
		Controllers: s.memberEndpointsLocked(), Leader: s.leaderEndpointLocked(),
		GeneratedAtUnix: now,
	})
}

func remoteHost(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

func indexOfRRLocked(lib *RRLibrary, id string) int {
	for i := range lib.Items {
		if lib.Items[i].ID == id {
			return i
		}
	}
	return -1
}

func (s *server) rrTouchLocked(lib *RRLibrary, by string, now int64) {
	lib.UpdatedAt = now
	lib.UpdatedBy = by
}
