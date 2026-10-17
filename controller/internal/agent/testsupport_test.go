package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// testPKI 是 agent 测试用的最小公钥基础设施：一把 CA、一张监听 127.0.0.1 的
// 控制器证书、以及"按 (tenant, node_id) 签节点证书"的能力。三者与
// cmd/sr-ctrl 里的 loadCA / loadOrCreateServerCert / signDevice 一一对应，
// 所以这些测试走的是真证书链、真 SAN，而不是把校验关掉后的自说自话。
type testPKI struct {
	ca     *x509.Certificate
	caKey  ed25519.PrivateKey
	caPEM  string
	pool   *x509.CertPool
	server tls.Certificate
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	p := &testPKI{}

	caPub, caPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成 CA 密钥: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-controller-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caPub, caPriv)
	if err != nil {
		t.Fatalf("签 CA: %v", err)
	}
	p.ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("解析 CA: %v", err)
	}
	p.caKey = caPriv
	p.caPEM = pemString("CERTIFICATE", caDER)
	p.pool = x509.NewCertPool()
	p.pool.AddCert(p.ca)

	// 服务器证书只带 IP SAN：httptest 的地址是 127.0.0.1，而客户端把主机名
	// 填进 tls.Config.ServerName。没有 SAN 的证书在 Go 里一律验不过，
	// CommonName 早就不足以当身份了。
	sPub, sPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成服务器密钥: %v", err)
	}
	sTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-controller"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1").To4()},
		DNSNames:     []string{"localhost"},
	}
	sDER, err := x509.CreateCertificate(rand.Reader, sTmpl, p.ca, sPub, caPriv)
	if err != nil {
		t.Fatalf("签服务器证书: %v", err)
	}
	sKeyDER, err := x509.MarshalPKCS8PrivateKey(sPriv)
	if err != nil {
		t.Fatalf("编码服务器私钥: %v", err)
	}
	p.server, err = tls.X509KeyPair(
		pemBlock("CERTIFICATE", sDER), pemBlock("PRIVATE KEY", sKeyDER))
	if err != nil {
		t.Fatalf("加载服务器密钥对: %v", err)
	}
	return p
}

// issueDevice 与 cmd/sr-ctrl 的 signDevice 同构：(tenant, node_id) 只写在
// URI SAN 里，别处没有。测试正是靠这一点构造"证书说 7、控制器口头说 9"的冲突。
func (p *testPKI) issueDevice(t *testing.T, pub ed25519.PublicKey, tenant string, node uint16) string {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: fmt.Sprintf("%s-node-%d", tenant, node)},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{{Scheme: "srou", Host: tenant, Path: fmt.Sprintf("/node/%d", node)}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, pub, p.caKey)
	if err != nil {
		t.Fatalf("签节点证书: %v", err)
	}
	return pemString("CERTIFICATE", der)
}

// serve 起一个真 TLS 服务：只认这把 CA，客户端证书"给了就必须验"
// —— 与 cmd/sr-ctrl 的 tls.VerifyClientCertIfGiven 同一档。
func (p *testPKI) serve(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	s := httptest.NewUnstartedServer(h)
	s.TLS = &tls.Config{
		Certificates: []tls.Certificate{p.server},
		ClientCAs:    p.pool,
		ClientAuth:   tls.VerifyClientCertIfGiven,
		MinVersion:   tls.VersionTLS12,
	}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

func pemBlock(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func pemString(typ string, der []byte) string { return string(pemBlock(typ, der)) }
