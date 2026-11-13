package main

// ---------------------------------------------------------------- console TLS
//
// 控制台一直是"明文 HTTP + 只绑 loopback"。设备面那条 8444 已经有 mTLS，但浏览器
// 不可能出示设备证书，所以控制台只能走自己的账号/令牌会话 —— 只要它不上外网，
// 明文是可以接受的。
//
// 要放到外网就不行了：密码、12 小时有效的会话 cookie、以及每次操作下发的租户
// 配置都会在链路里裸着。这里补的就是那一层 TLS，并且**默认自签**，理由三条：
//
//  1. 没有域名就没有 ACME，自签是唯一能立刻生效的答案（-console-cert/-console-key
//     留着，将来有域名直接换，不用改代码）；
//  2. 自签证书装进系统钥匙串之后浏览器不再报警，而 IsCA=true + KeyUsageCertSign
//     是"能被当成根证书装进去"的前提，不是随手加的字段；
//  3. 证书落盘在 <data>/console-tls/ 下并在重启时复用 —— 每次重启换一张新指纹，
//     等于把已经信任它的浏览器反复踢回警告页。
//
// SAN 默认覆盖 localhost、主机名、回环和本机所有网卡地址；公网 IP / 域名这类
// 不在网卡上的名字用 -console-san 追加。

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// consoleCertValidity 是一张自签证书的有效期。不要拉长：G3 之后苹果对 TLS 服务端
// 证书的上限是 398 天，超了整张证书直接不被信任；一年留出续签窗口又不越线。
const consoleCertValidity = 365 * 24 * time.Hour

// consoleCertRenewBefore 决定"还剩多久就提前重签"。自签没有 ACME 自动续期，靠
// 每次启动检查；一周足够人注意到日志里的重签记录。
const consoleCertRenewBefore = 7 * 24 * time.Hour

// consoleCertInfo 是启动时实际用上的那份证书，连同它的 SAN 列表（日志要打出来，
// 否则操作员不知道证书里到底有哪些名字、该拿哪个去装钥匙串）。
type consoleCertInfo struct {
	cert   tls.Certificate
	sans   []string
	reused bool
}

// consoleCertPath 是自签证书的默认落盘位置。
func consoleCertPath(dataDir string) (certFile, keyFile string) {
	dir := filepath.Join(dataDir, "console-tls")
	return filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
}

// loadConsoleCert 决定这次启动用哪张证书：显式给了就用给的，否则用 data 目录里
// 现成的，都没有（或已到期/读不出来）才签一张新的。
func loadConsoleCert(dataDir, certFile, keyFile, extraSAN string) (*consoleCertInfo, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, fmt.Errorf("-console-cert 与 -console-key 必须成对给出")
	}
	dns, ips := consoleSANs(extraSAN)
	explicit := certFile != ""
	if !explicit {
		certFile, keyFile = consoleCertPath(dataDir)
	}

	info, err := readConsoleCert(certFile, keyFile, dns, ips)
	switch {
	case err == nil:
		return info, nil
	case explicit:
		// 显式指定的证书读不出来就是配置错误，不能偷偷换成自签 —— 那会让"我以为
		// 在用公司证书"变成一句谎话。
		return nil, fmt.Errorf("读取 -console-cert 失败：%w", err)
	case !os.IsNotExist(err):
		log.Printf("console TLS：现有证书不可用（%v），重新签发", err)
	}
	return generateConsoleCert(certFile, keyFile, dns, ips)
}

// readConsoleCert 读取并校验现成的一对证书/私钥。返回的 error 若是 os.IsNotExist
// 表示还没有证书（首次启动的正常情况），其余情况都值得在日志里说一句。
func readConsoleCert(certFile, keyFile string, dns []string, ips []net.IP) (*consoleCertInfo, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("证书与私钥不配对：%w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("解析证书失败：%w", err)
	}
	if time.Now().After(leaf.NotAfter.Add(-consoleCertRenewBefore)) {
		return nil, fmt.Errorf("证书 %q 将于 %s 到期", leaf.Subject.CommonName, leaf.NotAfter.Format("2006-01-02"))
	}
	pair.Leaf = leaf
	info := &consoleCertInfo{cert: pair, sans: certSANList(leaf), reused: true}

	// 只警告、不重签：重签等于把已经装进钥匙串的那张作废，操作员会被莫名其妙
	// 弹回警告页。让他自己决定什么时候删。
	for _, name := range append(append([]string{}, dns...), ipStrings(ips)...) {
		if !sanCovered(leaf, name) {
			log.Printf("console TLS：证书里没有 %s —— -console-san 新加的？删掉 %s 和 %s 后重启会重新签发",
				name, certFile, keyFile)
			break
		}
	}
	return info, nil
}

// generateConsoleCert 签一张自签证书并落盘，权限按私钥最严的那一档来。
func generateConsoleCert(certFile, keyFile string, dns []string, ips []net.IP) (*consoleCertInfo, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	cn := "sr-ctrl console"
	if len(dns) > 0 {
		cn = dns[0]
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"sr-ctrl console (self-signed)"}},
		NotBefore:    now.Add(-time.Hour), // 容忍两端时钟小幅漂移
		NotAfter:     now.Add(consoleCertValidity),
		// CertSign + IsCA 是为了"能装进钥匙串当根证书"；DigitalSignature 是服务端
		// 自己用私钥签握手用的。
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(certFile), 0o700); err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		return nil, err
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &consoleCertInfo{
		cert:   tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf},
		sans:   certSANList(leaf),
		reused: false,
	}, nil
}

// consoleSANs 收集这张证书该覆盖的名字：本机天然成立的那些（localhost、主机名、
// 回环、网卡地址），加上 -console-san 里点名的公网 IP / 域名。
func consoleSANs(extra string) (dns []string, ips []net.IP) {
	dns = append(dns, "localhost")
	if h, err := os.Hostname(); err == nil && h != "" {
		dns = append(dns, h)
		// macOS 上主机名常常就是 "localhost"，再拼个 .local 会得到
		// localhost.local 这种没人会去访问的名字，只是把证书弄脏。
		if !strings.Contains(h, ".") && h != "localhost" {
			dns = append(dns, h+".local")
		}
	}
	ips = append(ips, net.IPv4(127, 0, 0, 1), net.IPv6loopback)
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			// 链路本地/回环已经单独加过；fe80:: 这类随接口状态变化，进证书只会
			// 让指纹无谓地变。
			if ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
				continue
			}
			ips = append(ips, ipnet.IP)
		}
	}
	for _, raw := range strings.Split(extra, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if ip := net.ParseIP(name); ip != nil {
			ips = append(ips, ip)
		} else {
			dns = append(dns, name)
		}
	}
	return dedupeStrings(dns), dedupeIPs(ips)
}

// certSANList 把一张证书里的名字摊平成日志里好读的一串。
func certSANList(cert *x509.Certificate) []string {
	out := append([]string{}, cert.DNSNames...)
	return append(out, ipStrings(cert.IPAddresses)...)
}

// sanCovered 判断某个名字是否已在证书里 —— 用来提示"-console-san 加了但没重签"。
func sanCovered(cert *x509.Certificate, name string) bool {
	if ip := net.ParseIP(name); ip != nil {
		for _, have := range cert.IPAddresses {
			if have.Equal(ip) {
				return true
			}
		}
		return false
	}
	for _, have := range cert.DNSNames {
		if strings.EqualFold(have, name) {
			return true
		}
	}
	return false
}

func ipStrings(ips []net.IP) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func dedupeIPs(in []net.IP) []net.IP {
	out := in[:0]
	for _, ip := range in {
		dup := false
		for _, have := range out {
			if have.Equal(ip) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, ip)
		}
	}
	return out
}
