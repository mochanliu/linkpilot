package main

// cluster_ca_test.go —— CA 私钥不再进加入码/加入应答之后的两条边界：
//  1. handleClusterCA（内部补送入口）只收"与本机集群 CA 证书相配"的私钥；
//  2. 被转发给 leader 的写请求，本机 auditMiddleware 不再重复记台账。

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// caKeyPEM 把 CA 私钥编成 PEM（与 pushCAKey 的载荷同一格式）。
func caKeyPEM(t *testing.T, key ed25519.PrivateKey) string {
	t.Helper()
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}))
}

func postCA(t *testing.T, s *server, key ed25519.PrivateKey) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"ca_key": caKeyPEM(t, key)})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/cluster/ca", bytes.NewReader(body))
	req.Header.Set("X-Cluster-Secret", s.clusterSecret())
	rr := httptest.NewRecorder()
	s.handleClusterCA(rr, req)
	return rr
}

// TestClusterCAAcceptsMatchingKey 验收补送主路径：私钥与 ca.pem 相配 → 落盘、
// 内存就绪（s.caKey 不再是 nil）。
func TestClusterCAAcceptsMatchingKey(t *testing.T) {
	dir := t.TempDir()
	ca, key, pool, err := loadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟"加入方刚拿到证书、还没有私钥"的状态。
	if err := os.Remove(filepath.Join(dir, "ca.key")); err != nil {
		t.Fatal(err)
	}
	s := &server{dataDir: dir, secret: "sec", caCert: ca, caPool: pool}
	if s.caKey != nil {
		t.Fatal("前置条件不对：此刻不该有私钥")
	}
	rr := postCA(t, s, key)
	if rr.Code != 200 {
		t.Fatalf("相配的私钥被拒: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.key")); err != nil {
		t.Fatal("私钥没有落盘")
	}
	if s.caKey == nil {
		t.Fatal("私钥没有装进内存，签发能力仍然缺失")
	}
}

// TestClusterCARejectsForeignKey 别人的私钥不许写进来：没有这条校验，任何
// 拿到集群密钥的机器都能把本机的签发根换成自己控制的。
func TestClusterCARejectsForeignKey(t *testing.T) {
	dir := t.TempDir()
	ca, _, pool, err := loadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	// loadOrCreateCA 会生成自己的 ca.key（单机路径）；模拟"只有证书"的成员。
	if err := os.Remove(filepath.Join(dir, "ca.key")); err != nil {
		t.Fatal(err)
	}
	_, foreign, _ := ed25519.GenerateKey(rand.Reader)
	s := &server{dataDir: dir, secret: "sec", caCert: ca, caPool: pool}
	rr := postCA(t, s, foreign)
	if rr.Code == 200 {
		t.Fatal("别人的 CA 私钥居然被收下了")
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.key")); err == nil {
		t.Fatal("被拒的私钥不该落盘")
	}
}

// TestClusterCARequiresSecret 空密钥（没开集群）一律拒绝：不能把
// /v1/cluster/ca 变成匿名可写的私钥投递口。
func TestClusterCARequiresSecret(t *testing.T) {
	dir := t.TempDir()
	ca, key, pool, err := loadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{dataDir: dir, caCert: ca, caPool: pool} // secret 为空
	rr := postCA(t, s, key)
	if rr.Code != 503 {
		t.Fatalf("没开集群时应该 503，拿到 %d", rr.Code)
	}
}

// TestAuditSkipsForwardedRequests leader 已经记过的台账，follower 不重复记。
// 转发失败（leader 没应答）则照记 —— 只有本机见过这次尝试。
func TestAuditSkipsForwardedRequests(t *testing.T) {
	dir := t.TempDir()
	ops, err := openOpsLedger(filepath.Join(dir, "opslog"))
	if err != nil {
		t.Fatal(err)
	}
	s := &server{ops: ops, admin: newAdminHub(), state: &State{Nodes: map[string]*NodeRecord{}}}
	var seenStatus int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/publish", func(w http.ResponseWriter, r *http.Request) {
		// leaderGuard 在内层：只有"转发成功"的那些请求会带标记。
		if r.Header.Get("X-Test-Forward") != "" {
			r.Header.Set(xClusterForwarded, "1")
		}
		seenStatus = 200
	})
	h := s.auditMiddleware(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/publish", nil)
	req.Header.Set("X-Test-Forward", "1")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	time.Sleep(50 * time.Millisecond)
	if ents := ops.query(0, time.Now().Unix()+1, nil, "", 0); len(ents) != 0 {
		t.Fatalf("被转发的请求不该在本机记台账，记了 %d 条", len(ents))
	}

	// 对照组：没打标记的（转发失败、或登录这类 nodeLocal 路径）照记。
	req2 := httptest.NewRequest(http.MethodPost, "/api/publish", nil)
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	time.Sleep(50 * time.Millisecond)
	ents := ops.query(0, time.Now().Unix()+1, nil, "", 0)
	if len(ents) != 1 {
		t.Fatalf("未转发的请求应该记 1 条台账，记了 %d 条", len(ents))
	}
	if ents[0].Kind != "audit" || ents[0].Msg == "" {
		t.Fatalf("审计行内容不对: %+v", ents[0])
	}
	_ = seenStatus
}
