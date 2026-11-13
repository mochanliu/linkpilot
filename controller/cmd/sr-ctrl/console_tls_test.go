package main

// console_tls_test.go —— 控制台 HTTPS 的三件事：证书怎么来、显式配置出错时
// 会不会偷偷降级、以及真实握手下 cookie 的属性。
// 最后一条必须在真实 listener 上跑：httptest 的 recorder 里 r.TLS 永远是 nil，
// 那样测出来的 Secure 只是个常量，证明不了"浏览器拿到的 cookie 带 Secure"。

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestConsoleCertSelfSignedSANs 覆盖首次启动的自签路径：证书要覆盖本机固有名字、
// 要能被当成根证书装进钥匙串（IsCA + CertSign），私钥落盘权限必须是 0600。
func TestConsoleCertSelfSignedSANs(t *testing.T) {
	dir := t.TempDir()
	info, err := loadConsoleCert(dir, "", "", "10.9.8.7,edge.example.test")
	if err != nil {
		t.Fatalf("loadConsoleCert: %v", err)
	}
	if info.reused {
		t.Fatal("首次启动标成了复用")
	}
	joined := strings.Join(info.sans, ",")
	for _, want := range []string{"localhost", "127.0.0.1", "::1", "10.9.8.7", "edge.example.test"} {
		if !strings.Contains(joined, want) {
			t.Errorf("证书 SAN 里缺 %s：%s", want, joined)
		}
	}
	if h, err := os.Hostname(); err == nil && h != "" && !strings.Contains(joined, h) {
		t.Errorf("证书 SAN 里缺主机名 %s：%s", h, joined)
	}

	leaf := info.cert.Leaf
	if leaf == nil {
		t.Fatal("没有解析出 leaf 证书")
	}
	if !leaf.IsCA || leaf.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("不是可当作根证书的形态（浏览器装不进钥匙串）")
	}
	if !hasExtKeyUsage(leaf, x509.ExtKeyUsageServerAuth) {
		t.Error("缺 ServerAuth 用途，握手会被浏览器拒")
	}
	if d := time.Until(leaf.NotAfter); d > consoleCertValidity+time.Hour || d < consoleCertValidity-48*time.Hour {
		t.Errorf("有效期 %v，期望约 %v", d, consoleCertValidity)
	}
	// 这两个名字必须过校验，否则浏览器连"点过警告"之后都还是红的。
	for _, name := range []string{"127.0.0.1", "localhost", "edge.example.test"} {
		if err := leaf.VerifyHostname(name); err != nil {
			t.Errorf("证书不覆盖 %s: %v", name, err)
		}
	}

	certFile, keyFile := consoleCertPath(dir)
	if _, err := os.Stat(certFile); err != nil {
		t.Fatalf("证书没落盘: %v", err)
	}
	st, err := os.Stat(keyFile)
	if err != nil {
		t.Fatalf("私钥没落盘: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("私钥权限 %o，期望 600", perm)
	}
}

// TestConsoleCertReusedAcrossRestart 是这条设计的关键：每次重启换一张新证书，
// 等于把已经信任它的浏览器反复踢回警告页。
func TestConsoleCertReusedAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	first, err := loadConsoleCert(dir, "", "", "")
	if err != nil {
		t.Fatalf("第一次: %v", err)
	}
	second, err := loadConsoleCert(dir, "", "", "")
	if err != nil {
		t.Fatalf("第二次: %v", err)
	}
	if !second.reused {
		t.Fatal("第二次启动没有复用证书")
	}
	if got, want := second.cert.Leaf.SerialNumber.String(), first.cert.Leaf.SerialNumber.String(); got != want {
		t.Fatalf("重启后换了证书：序列号 %s != %s", got, want)
	}
}

// TestConsoleCertExplicitPairGuard：显式给了证书就不能有任何"悄悄换成自签"的
// 余地 —— 那会让操作员以为自己在用公司证书。
func TestConsoleCertExplicitPairGuard(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadConsoleCert(dir, filepath.Join(dir, "c.pem"), "", ""); err == nil ||
		!strings.Contains(err.Error(), "成对") {
		t.Fatalf("只给证书没报错: %v", err)
	}
	if _, err := loadConsoleCert(dir, filepath.Join(dir, "nope.pem"), filepath.Join(dir, "nope.key"), ""); err == nil {
		t.Fatal("显式证书读不出来却成功了（偷偷自签？）")
	}
	if _, err := loadConsoleCert(dir, "", filepath.Join(dir, "nope.key"), ""); err == nil {
		t.Fatal("只给私钥没报错")
	}
}

// TestConsoleTLSLoginIssuesSecureCookie 走真实监听器 + 真实握手：
// 不带 CA 的客户端必须失败（证明它真的在说 TLS），带 CA 的客户端登录后拿到的
// 会话 cookie 必须带 Secure/HttpOnly，且用它取得到控制台首页。
func TestConsoleTLSLoginIssuesSecureCookie(t *testing.T) {
	s := newTestServerWithOps(t)
	s.adminToken = "e2e-operator-token"
	dir := t.TempDir()
	info, err := loadConsoleCert(dir, "", "", "")
	if err != nil {
		t.Fatalf("loadConsoleCert: %v", err)
	}

	srv := &http.Server{
		Handler:   s.adminHandler(),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{info.cert}, MinVersion: tls.VersionTLS12},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.ServeTLS(ln, "", "")
	defer srv.Close()
	base := "https://" + ln.Addr().String()

	// 不信任这张自签证书的客户端必须握手失败。这条断言防止"其实在跑明文"。
	blind := &http.Client{Timeout: 3 * time.Second}
	if _, err := blind.Get(base + "/"); err == nil {
		t.Fatal("未受信的客户端居然连上了 —— 说明这条链路不是真 TLS")
	}

	pemBytes, err := os.ReadFile(filepath.Join(dir, "console-tls", "cert.pem"))
	if err != nil {
		t.Fatalf("读证书: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatal("证书没进池")
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Timeout: 5 * time.Second,
		Jar:     jar,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}

	body, _ := json.Marshal(map[string]string{"token": "e2e-operator-token"})
	resp, err := client.Post(base+"/api/login", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("登录请求: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("登录返回 %d", resp.StatusCode)
	}
	var got *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			got = c
		}
	}
	if got == nil {
		t.Fatal("登录没发会话 cookie")
	}
	if !got.Secure || !got.HttpOnly {
		t.Errorf("cookie 属性不对：Secure=%v HttpOnly=%v（TLS 下必须都是 true）", got.Secure, got.HttpOnly)
	}

	// 带 cookie 取首页：能拿到页面说明会话在 TLS 监听器上是通的。
	page, err := client.Get(base + "/")
	if err != nil {
		t.Fatalf("取首页: %v", err)
	}
	defer page.Body.Close()
	if page.StatusCode != 200 {
		t.Fatalf("首页 %d", page.StatusCode)
	}
}

// TestConsolePlainCookieNotSecure 是上一条的反面：同一个控制器上还开着一个
// loopback 明文监听器（-console-http，给本机脚本用），那条路径发的 cookie 不能
// 带 Secure，否则浏览器在 http://127.0.0.1 上根本存不下它。
func TestConsolePlainCookieNotSecure(t *testing.T) {
	s := newTestServerWithOps(t)
	s.adminToken = "plain-token"
	h := s.adminHandler()

	body, _ := json.Marshal(map[string]string{"token": "plain-token"})
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("明文登录 %d", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Secure {
			t.Fatal("明文监听器发出的 cookie 带了 Secure，浏览器会丢弃它")
		}
	}
}

func hasExtKeyUsage(cert *x509.Certificate, want x509.ExtKeyUsage) bool {
	for _, got := range cert.ExtKeyUsage {
		if got == want {
			return true
		}
	}
	return false
}
