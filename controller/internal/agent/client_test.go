package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"sdwan/controller/internal/keybinding"
	"sdwan/controller/internal/wire"
)

// stubVBOND 是 cmd/sr-ctrl 的替身，照抄真服务端要命的三件事：身份只从客户端
// 证书的 URI SAN 读、非 200 一律是 http.Error 的纯文本、应答体字段与
// internal/wire 的类型同名。
//
// 它**不是**契约测试：真契约由 test/scripts 下对真控制器跑的 e2e 把关。
// 这里只证明客户端这一侧的编码、解码与错误分支不撒谎。
type stubVBOND struct {
	wantTen string // 期望客户端证书里的租户
	wantNod uint16 // 期望客户端证书里的 node_id
	mu      sync.Mutex
	bodies  map[string]json.RawMessage // 路径 -> 最后一次收到的请求体
	resps   map[string]any             // 路径 -> 应答体
	details map[string]string          // 路径 -> 强制拒绝时的原文（模拟真服务端的措辞）
	watch   []string                   // /v1/watch 逐行写出的 JSON
}

func newStub() *stubVBOND {
	return &stubVBOND{
		bodies:  map[string]json.RawMessage{},
		resps:   map[string]any{},
		details: map[string]string{},
	}
}

func (s *stubVBOND) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/enroll", func(w http.ResponseWriter, r *http.Request) {
		// enroll 是公开入口：设备此刻还没有证书，能靠的只有 claim code。
		s.record(r)
		s.writeResp(w, "/v1/enroll")
	})
	mux.HandleFunc("/v1/watch", func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := s.identity(r); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		if d := s.reject("/v1/watch"); d != "" {
			http.Error(w, d, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/jsonl")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, line := range s.watchLines() {
			fmt.Fprint(w, line)
			fl.Flush()
		}
	})
	for _, p := range []string{"/v1/heartbeat", "/v1/mapping", "/v1/observe", "/v1/reach", "/v1/registry"} {
		path := p
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			certTenant, certNode, err := s.identity(r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			raw := s.record(r)
			if d := s.reject(path); d != "" {
				http.Error(w, fmt.Sprintf("%s (%s/node/%d): %s", path, certTenant, certNode, d), http.StatusForbidden)
				return
			}
			if path == "/v1/observe" {
				// 真服务端的铁律：node 5 的证书不能替 node 7 报观测。客户端
				// 自动按身份填 node_id，所以这条分支只在状态脏了时才走得到。
				var rep wire.ObserveReport
				if json.Unmarshal(raw, &rep) == nil && rep.NodeID != certNode {
					http.Error(w, "node id does not match certificate", http.StatusForbidden)
					return
				}
			}
			s.writeResp(w, path)
		})
	}
	return mux
}

// identity 与真服务端的 peerTenantFromCert 同构：证书就是授权，
// 请求参数里写了什么说了不算。
func (s *stubVBOND) identity(r *http.Request) (string, uint16, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", 0, errors.New("client certificate required")
	}
	tenant, node, err := keybinding.IdentityOf(r.TLS.PeerCertificates[0])
	if err != nil {
		return "", 0, err
	}
	if tenant != s.wantTen || node != s.wantNod {
		return "", 0, fmt.Errorf("certificate is for %s/node/%d, expected %s/node/%d",
			tenant, node, s.wantTen, s.wantNod)
	}
	return tenant, node, nil
}

func (s *stubVBOND) record(r *http.Request) json.RawMessage {
	var raw json.RawMessage
	if r.Method == http.MethodPost && r.Body != nil {
		raw, _ = io.ReadAll(r.Body)
		r.Body.Close()
	}
	s.mu.Lock()
	s.bodies[r.URL.Path] = raw
	s.mu.Unlock()
	return raw
}

func (s *stubVBOND) writeResp(w http.ResponseWriter, path string) {
	s.mu.Lock()
	resp := s.resps[path]
	s.mu.Unlock()
	if resp == nil {
		resp = map[string]any{"ok": true}
	}
	json.NewEncoder(w).Encode(resp)
}

// reject 让测试按需把某个路径变成"服务端拒绝"，并控制拒绝原文 ——
// Detail 会被 agent 打进日志，措辞得能一路带出去。
func (s *stubVBOND) reject(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.details[path]
}

func (s *stubVBOND) setWatch(lines []string) {
	s.mu.Lock()
	s.watch = lines
	s.mu.Unlock()
}

func (s *stubVBOND) watchLines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.watch...)
}

// lastBody 取出替身收到的最后一个请求体，让断言写在结构体上而不是字符串上。
func (s *stubVBOND) lastBody(t *testing.T, path string, out any) {
	t.Helper()
	s.mu.Lock()
	raw := s.bodies[path]
	s.mu.Unlock()
	if len(raw) == 0 {
		t.Fatalf("%s 没收到请求体", path)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("解 %s 请求体: %v（原文 %s）", path, err, raw)
	}
}

func jsonLine(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("编事件: %v", err)
	}
	return string(b) + "\n"
}

// enrolledPair 造出"已入网"的一套：身份 + 替身控制器 + 能上 mTLS 的客户端。
func enrolledPair(t *testing.T) (*Identity, *stubVBOND, *Client) {
	t.Helper()
	pki := newTestPKI(t)
	id := newIdentity(t, "CPE-0001")
	stub := newStub()
	stub.wantTen, stub.wantNod = "t1", 7
	srv := pki.serve(t, stub.handler())

	stub.resps["/v1/enroll"] = wire.EnrollResponse{
		TenantID:     "t1",
		NodeID:       7,
		CertPEM:      pki.issueDevice(t, id.IdentityPublicKey(), "t1", 7),
		CAPEM:        pki.caPEM,
		TLSAddrs:     []string{srv.URL},
		HeartbeatSec: 5,
		Epoch:        3,
	}
	c, resp := enrollAndDial(t, id, srv.URL, pki.pool)
	if resp.Epoch != 3 || resp.HeartbeatSec != 5 {
		t.Errorf("enroll 应答少了身份事实: %+v", resp)
	}
	return id, stub, c
}

func enrollAndDial(t *testing.T, id *Identity, base string, roots *x509.CertPool) (*Client, wire.EnrollResponse) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := Enroll(ctx, base, roots, id.EnrollRequest("CLAIM-1", "0.3.0", 2, ""))
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if err := id.SetEnrolled(resp.TenantID, resp.NodeID, resp.CertPEM, resp.CAPEM); err != nil {
		t.Fatalf("SetEnrolled: %v", err)
	}
	c, err := NewClient(base, id)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, resp
}

// callCtx 给每个调用一个 5 秒期限：测试卡住要能看出是哪个调用卡的，
// 不该等 go test 的全局超时。
func callCtx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestEnrollRequestCarriesIdentityKeyNotWGKey(t *testing.T) {
	pki := newTestPKI(t)
	id := newIdentity(t, "CPE-0001")
	stub := newStub()
	srv := pki.serve(t, stub.handler())
	stub.resps["/v1/enroll"] = wire.EnrollResponse{
		TenantID: "t1", NodeID: 7,
		CertPEM: pki.issueDevice(t, id.IdentityPublicKey(), "t1", 7),
		CAPEM:   pki.caPEM,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Enroll(ctx, srv.URL, pki.pool, id.EnrollRequest("CLAIM-1", "0.3.0", 2, "")); err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	var got wire.EnrollRequest
	stub.lastBody(t, "/v1/enroll", &got)
	if got.ProtocolVer != wire.ProtocolVersion {
		t.Errorf("protocol_ver=%d，控制器要求 %d（版本不符会被回 400）", got.ProtocolVer, wire.ProtocolVersion)
	}
	if got.Serial != "CPE-0001" || got.ClaimCode != "CLAIM-1" || got.UDPRelfectorN != 2 {
		t.Errorf("请求字段不对: %+v", got)
	}
	// public_key 交的是身份密钥：此刻 (tenant, node_id) 还不存在，
	// WG 公钥只能等 enroll 之后再走 key binding（见 wire.EnrollRequest 注释）。
	pub, err := base64.StdEncoding.DecodeString(got.PublicKey)
	if err != nil {
		t.Fatalf("public_key 不是 base64: %v", err)
	}
	if !ed25519.PublicKey(pub).Equal(id.IdentityPublicKey()) {
		t.Error("public_key 不是本机身份公钥")
	}
	wgPub, err := id.WGPublicKey(0)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(pub, wgPub) {
		t.Error("public_key 交成了 WG 公钥")
	}
}

func TestEnrollRefusesUnpinnedCA(t *testing.T) {
	pki := newTestPKI(t)
	other := newTestPKI(t)
	id := newIdentity(t, "CPE-0001")
	stub := newStub()
	srv := pki.serve(t, stub.handler())
	stub.resps["/v1/enroll"] = wire.EnrollResponse{
		TenantID: "t1", NodeID: 7,
		CertPEM: pki.issueDevice(t, id.IdentityPublicKey(), "t1", 7),
		CAPEM:   pki.caPEM,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 拿别人的 CA 当根去连：必须握手失败。这里没有 trust-on-first-use ——
	// enroll 应答里装的是整套信任根，第一次就无条件接受，等于把"伪造整个租户"
	// 的成本降到一次抓包。
	if _, err := Enroll(ctx, srv.URL, other.pool, id.EnrollRequest("CLAIM-1", "0.3.0", 2, "")); err == nil {
		t.Fatal("用了没固定过的 CA 居然 enroll 成功")
	} else if !strings.Contains(err.Error(), "unknown authority") {
		t.Errorf("错误没点明是证书链问题: %v", err)
	}
	if _, err := Enroll(ctx, srv.URL, nil, id.EnrollRequest("CLAIM-1", "0.3.0", 2, "")); err == nil {
		t.Error("roots 为 nil 也照样发请求")
	}
	if _, err := Enroll(ctx, "no-scheme-host", pki.pool, id.EnrollRequest("CLAIM-1", "0.3.0", 2, "")); err == nil {
		t.Error("地址里没有主机名却成功了")
	}
}

func TestAuthenticatedCallsFillNodeIDAndCarryBinding(t *testing.T) {
	id, stub, c := enrolledPair(t)

	stub.resps["/v1/heartbeat"] = wire.HeartbeatResponse{Epoch: 4, PeerCount: 2, BindingErr: "bad sig"}
	hb, err := c.Heartbeat(callCtx(t), wire.HeartbeatRequest{UDPMapping: "203.0.113.5:41111"})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if hb.Epoch != 4 || hb.PeerCount != 2 || hb.BindingErr != "bad sig" {
		t.Errorf("应答解码不对: %+v", hb)
	}
	var gotHB wire.HeartbeatRequest
	stub.lastBody(t, "/v1/heartbeat", &gotHB)
	if gotHB.NodeID != 7 {
		t.Errorf("心跳里的 node_id=%d，客户端本该按证书填 7", gotHB.NodeID)
	}

	// 一次交八份，而不是逐槽点调：控制器要能回答"这条隧道我用第 n 把"，
	// 前提是它手里这台设备的八份担保都是齐的。
	bindings, err := id.Bindings()
	if err != nil {
		t.Fatalf("Bindings: %v", err)
	}
	stub.resps["/v1/mapping"] = map[string]any{"ok": true, "binding_err": "binding signature does not cover (tenant=\"t1\" node=7 slot=0)"}
	ack, err := c.ReportMapping(callCtx(t), wire.MappingReport{
		UDPMapping:  "203.0.113.5:41111",
		LocalPort:   51820,
		Bindings:    bindings,
		VpnPort:     51820,
		TransitPort: 51821,
		LocalAddrs:  []string{"10.10.1.2/24"},
	})
	if err != nil {
		t.Fatalf("ReportMapping: %v", err)
	}
	// binding_err 是非致命上报：节点照常在册，但对端配不出它的 peer。
	// 把它丢了，症状就退化成"隧道在但没流量"，谁也不知道为什么。
	if !ack.OK || ack.BindingErr == "" {
		t.Errorf("binding_err 没透传: %+v", ack)
	}
	var gotMap wire.MappingReport
	stub.lastBody(t, "/v1/mapping", &gotMap)
	// 下标就是槽号，所以这里既查长度也查每一格：少一份或错位，控制器就会把
	// 槽 5 的公钥装进槽 2 的隧道。
	if len(gotMap.Bindings) != keybinding.WGKeySlots {
		t.Fatalf("映射报告里的担保 %d 份，期望 %d 份", len(gotMap.Bindings), keybinding.WGKeySlots)
	}
	for slot := range bindings {
		got := gotMap.Bindings[slot]
		if got.Slot != uint8(slot) || got.X25519Pub != bindings[slot].X25519Pub || got.Sig != bindings[slot].Sig {
			t.Errorf("槽 %d 的担保在上报里丢了或换了: %+v", slot, got)
		}
	}
	if gotMap.VpnPort != 51820 || gotMap.TransitPort != 51821 {
		t.Errorf("端口没送出去（重绑过就必须告诉对端在哪找我）: %+v", gotMap)
	}

	stub.resps["/v1/observe"] = wire.ObserveResponse{
		Epoch: 5, Accepted: 2,
		Attributed:   []wire.Attribution{{Mapping: "203.0.113.9:45000", DstPort: 51820, NodeID: 3, Reason: "claimed"}},
		Unattributed: []wire.Attribution{{Mapping: "198.51.100.4:5060", Reason: "no node claimed this address"}},
	}
	obs, err := c.ReportObservations(callCtx(t), wire.ObserveReport{Reset: true, Observations: []wire.Observation{
		{SrcIP: "203.0.113.9", SrcPort: 45000, DstPort: 51820, AgeSec: 12, Verified: true, Hits: 40},
	}})
	if err != nil {
		t.Fatalf("ReportObservations: %v", err)
	}
	if len(obs.Attributed) != 1 || obs.Attributed[0].NodeID != 3 || len(obs.Unattributed) != 1 {
		t.Errorf("归因结果丢了: %+v", obs)
	}
	var gotObs wire.ObserveReport
	stub.lastBody(t, "/v1/observe", &gotObs)
	if gotObs.NodeID != 7 || !gotObs.Reset {
		t.Errorf("观测表的 node_id/reset 不对: node=%d reset=%v", gotObs.NodeID, gotObs.Reset)
	}
	// reset=true 是"我清过表"的承诺：没有它，缺席的条目只能当成"没看见"，
	// 不能当成"已经没了"，控制器的表就只增不减。

	if err := c.ReportReach(callCtx(t), wire.ReachReport{PeerNodeID: 3, Result: "ready", RTTms: 21}); err != nil {
		t.Fatalf("ReportReach: %v", err)
	}
	var gotReach wire.ReachReport
	stub.lastBody(t, "/v1/reach", &gotReach)
	if gotReach.PeerNodeID != 3 || gotReach.Result != "ready" {
		t.Errorf("打洞结果没送到: %+v", gotReach)
	}

	stub.resps["/v1/registry"] = RegistryView{
		TenantID: "t1", Self: 7, Epoch: 6,
		Peers: []wire.PeerInfo{{NodeID: 3, Reach: "ready", ObservedMapping: "203.0.113.9:45000", ObservedVerified: true}},
	}
	rv, err := c.Registry(callCtx(t))
	if err != nil {
		t.Fatalf("Registry: %v", err)
	}
	if len(rv.Peers) != 1 || rv.Peers[0].ObservedMapping != "203.0.113.9:45000" || !rv.Peers[0].ObservedVerified {
		t.Errorf("registry 解码不对: %+v", rv)
	}
}

func TestServerErrorStatusesSurviveAsBranchable(t *testing.T) {
	_, stub, c := enrolledPair(t)

	stub.details["/v1/mapping"] = "node id does not match certificate"
	_, err := c.ReportMapping(callCtx(t), wire.MappingReport{UDPMapping: "203.0.113.5:1"})
	if StatusOf(err) != http.StatusForbidden {
		t.Fatalf("403 该能分支出来，实际 StatusOf=%d err=%v", StatusOf(err), err)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("错误不是 *Error: %T", err)
	}
	if e.Path != "/v1/mapping" || !strings.Contains(e.Detail, "node id does not match") {
		t.Errorf("状态码对了但细节丢了: %+v", e)
	}

	// 404 = 这台设备还没注册过，401 = 证书坏了。三者处置完全不同，
	// 全糊成"请求失败"的话 agent 的自愈就只剩重启。
	stub.details["/v1/heartbeat"] = "unknown node"
	if _, err := c.Heartbeat(callCtx(t), wire.HeartbeatRequest{}); StatusOf(err) != http.StatusForbidden {
		t.Errorf("心跳的拒绝没透传: %v", err)
	}
	if StatusOf(errors.New("plain")) != 0 {
		t.Error("StatusOf 给普通错误编出了状态码")
	}
}

func TestTenantComesFromCertNotFromClientState(t *testing.T) {
	// 设备合法地注册在 t2（证书写的就是 t2），控制器的替身只认 t1。
	// 客户端无论传什么参数都改不了这件事：租户由证书决定，
	// 这也是真服务端 peerTenantFromCert 的唯一判据。
	pki := newTestPKI(t)
	id := newIdentity(t, "CPE-0002")
	stub := newStub()
	stub.wantTen, stub.wantNod = "t1", 7
	srv := pki.serve(t, stub.handler())
	stub.resps["/v1/enroll"] = wire.EnrollResponse{
		TenantID: "t2", NodeID: 7,
		CertPEM: pki.issueDevice(t, id.IdentityPublicKey(), "t2", 7),
		CAPEM:   pki.caPEM,
	}

	c, _ := enrollAndDial(t, id, srv.URL, pki.pool)
	if id.TenantID() != "t2" {
		t.Fatalf("前置条件坏了: 本机自认 %q", id.TenantID())
	}
	if _, err := c.Registry(callCtx(t)); StatusOf(err) != http.StatusUnauthorized {
		t.Errorf("t2 的证书在只认 t1 的控制器上拿到了 registry: %v", err)
	}
	// GET 连请求体都没有，能 spoof 的只剩证书本身 —— 而它换不掉。
	if _, err := c.Heartbeat(callCtx(t), wire.HeartbeatRequest{UDPMapping: "203.0.113.5:1"}); StatusOf(err) != http.StatusUnauthorized {
		t.Errorf("跨租户心跳没被 401 挡住: %v", err)
	}
}

func TestWatchDecodesEveryEventIncludingOversized(t *testing.T) {
	_, stub, c := enrolledPair(t)
	// 单行 ~380KB：远超 bufio.Scanner 的 64KB token 上限。真控制器的 snapshot
	// 每行是一个完整 PeerInfo（含证书 PEM），行数多、单行也可能很长，
	// 客户端按行切的那一层迟早会把合法事件报成错误。
	big := strings.Repeat("srou:cert-body/", 20000)
	stub.setWatch([]string{
		jsonLine(t, wire.WatchEvent{Type: "snapshot", TenantID: "t1", Epoch: 6, Peer: wire.PeerInfo{NodeID: 7, IsSelf: true}}),
		jsonLine(t, wire.WatchEvent{Type: "snapshot", TenantID: "t1", Epoch: 6, Peer: wire.PeerInfo{NodeID: 3, UDPMapping: "203.0.113.9:45000", Reach: "punching"}}),
		jsonLine(t, wire.WatchEvent{Type: "update", TenantID: "t1", Epoch: 7, Peer: wire.PeerInfo{NodeID: 3, Reach: "ready"}}),
		jsonLine(t, wire.WatchEvent{Type: "ping", Epoch: 7, Message: big}),
		jsonLine(t, wire.WatchEvent{Type: "stale", TenantID: "t1", Peer: wire.PeerInfo{NodeID: 9, Stale: true}}),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := c.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer stream.Close()

	for i, wantType := range []string{"snapshot", "snapshot", "update", "ping", "stale"} {
		ev, err := stream.Next()
		if err != nil {
			t.Fatalf("第 %d 个事件: %v", i, err)
		}
		if ev.Type != wantType {
			t.Errorf("第 %d 个事件 type=%q，期望 %q", i, ev.Type, wantType)
		}
		// 租户必须逐条带着：node_id 只在租户内唯一，光看 id 会串门。
		if wantType != "ping" && ev.TenantID != "t1" {
			t.Errorf("第 %d 个事件没带 tenant_id: %+v", i, ev)
		}
		if wantType == "ping" && len(ev.Message) < 300000 {
			t.Errorf("大事件被截断了，长度 %d", len(ev.Message))
		}
		if wantType == "update" && ev.Epoch != 7 {
			t.Errorf("epoch 没解出来: %+v", ev)
		}
	}
	if _, err := stream.Next(); !errors.Is(err, ErrStreamClosed) {
		t.Errorf("流结束时该报 ErrStreamClosed，实际 %v", err)
	}
}

func TestWatchEmptyStreamAndRejection(t *testing.T) {
	_, stub, c := enrolledPair(t)

	// 空流：服务端收下连接就收尾。客户端第一口就该拿到"流已关闭"，
	// 零事件不能被当成"租户里没有别人"。
	stub.setWatch(nil)
	stream, err := c.Watch(callCtx(t))
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer stream.Close()
	if _, err := stream.Next(); !errors.Is(err, ErrStreamClosed) {
		t.Errorf("空流第一口该报关闭，实际 %v", err)
	}

	stub.details["/v1/watch"] = "client certificate required"
	if _, err := c.Watch(callCtx(t)); StatusOf(err) != http.StatusUnauthorized {
		t.Errorf("watch 的拒绝没包成 *Error: %v", err)
	}
}

func TestCanceledContextStopsCalls(t *testing.T) {
	_, _, c := enrolledPair(t)
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Heartbeat(cctx, wire.HeartbeatRequest{})
	if err == nil {
		t.Fatal("context 已取消却调用成功")
	}
	if StatusOf(err) != 0 {
		t.Errorf("本地取消不该带状态码: %v", err)
	}
	// 上层要能分清"我自己关的"和"网络坏了"：前者不该重连，后者该。
	if !errors.Is(err, context.Canceled) {
		t.Errorf("取消原因丢了: %v", err)
	}
}

func TestNewClientNeedsEnrolledIdentity(t *testing.T) {
	id := newIdentity(t, "CPE-0001")
	if _, err := NewClient("https://127.0.0.1:1", id); err == nil {
		t.Error("未 enroll 的身份配出了 mTLS 客户端")
	}
}
