// Package agent 是 CPE 侧的那一半：设备身份、与 vBOND 的对话，
// 以及把控制器意图落到 VPP 上的接缝。
//
// 这个包里的东西都只依赖标准库（和 go.mod 的零第三方约束一致）。
// 协议类型一律来自 internal/wire —— 服务端与 agent 共用同一份定义，
// 字段不可能两边各写各的。
package agent

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"sdwan/controller/internal/keybinding"
)

// 落盘文件名。身份私钥与 WG 私钥分开存：前者是"这台设备是谁"，
// 后者是"这条隧道怎么加密"，轮换节奏与备份语义都不同。
const (
	deviceKeyFile = "device_ed25519.key"
	wgKeyFile     = "wg_x25519.key"
	stateFile     = "state.json"
	nodeCertFile  = "node_cert.pem"
	caCertFile    = "ca_cert.pem"
	keyPerm       = 0o600
	dirPerm       = 0o700
)

// deviceSigner 把磁盘上的 Ed25519 私钥适配成 keybinding.Signer。
//
// 之所以要有这层薄壳而不是直接传 ed25519.PrivateKey：换成安全元件（PKCS#11 /
// TEE）时，"签名"这一步挪出进程，但上面所有代码只认 Signer 的两个方法。
type deviceSigner struct{ k ed25519.PrivateKey }

func (d deviceSigner) Sign(msg []byte) ([]byte, error) {
	return ed25519.Sign(d.k, msg), nil
}

func (d deviceSigner) Public() ed25519.PublicKey {
	return d.k.Public().(ed25519.PublicKey)
}

// state 是 enroll 之后才知道的那部分身份；私钥不进这个文件。
type state struct {
	Serial   string `json:"serial"`
	KeyGen   uint32 `json:"key_gen"`
	TenantID string `json:"tenant_id"`
	NodeID   uint16 `json:"node_id"`
}

// Identity 是一台 CPE 的完整身份：签名密钥、WG 密钥对、以及控制器签发的证书。
//
// 顺序有硬性依赖：X25519 公钥要用**身份密钥**签名（keybinding），而签名消息里
// 含 (tenant, node_id) —— 这两个值只有 enroll 之后才知道。所以私钥先生成、
// 先落盘，绑定留到 SetEnrolled 之后才谈（见 wire.EnrollRequest 的同一说明）。
type Identity struct {
	dir    string
	signer deviceSigner
	wgPriv []byte // 32 字节，交给 VPP 的那把（定案 3）
	wgPub  []byte
	state  state
	ready  bool // SetEnrolled 成功 = 有可用证书与 CA

	cert   *tls.Certificate
	caPool *x509.CertPool
	caPEM  []byte
}

// LoadOrCreate 读 dir 下的身份，缺什么补什么（私钥只会在第一次生成）。
// serial 必须与用户在控制器侧登记的一致：claim 阶段就是按它查的。
func LoadOrCreate(dir, serial string) (*Identity, error) {
	if serial == "" {
		return nil, errors.New("agent: serial 不能为空")
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("agent: 建身份目录 %s: %w", dir, err)
	}
	id := &Identity{dir: dir, state: state{Serial: serial}}

	priv, err := loadOrCreateEd25519(filepath.Join(dir, deviceKeyFile))
	if err != nil {
		return nil, err
	}
	id.signer = deviceSigner{k: priv}

	wg, err := loadOrCreateX25519(filepath.Join(dir, wgKeyFile))
	if err != nil {
		return nil, err
	}
	if err := id.setWGKey(wg); err != nil {
		return nil, err
	}

	if raw, err := os.ReadFile(filepath.Join(dir, stateFile)); err == nil {
		if err := json.Unmarshal(raw, &id.state); err != nil {
			return nil, fmt.Errorf("agent: 读 %s: %w", stateFile, err)
		}
		// serial 以调用方为准：它是操作者输入的标识，不该被磁盘上的旧值顶掉。
		id.state.Serial = serial
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("agent: 读 %s: %w", stateFile, err)
	}

	// 证书在私钥之后加载：只有两者都在，这份身份才能真正上 mTLS。
	if err := id.loadCerts(); err != nil {
		return nil, err
	}
	return id, nil
}

func loadOrCreateEd25519(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("agent: 生成设备身份密钥: %w", err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			return nil, err
		}
		block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		if err := os.WriteFile(path, block, keyPerm); err != nil {
			return nil, fmt.Errorf("agent: 写身份密钥: %w", err)
		}
		return priv, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agent: 读身份密钥: %w", err)
	}
	blk, _ := pem.Decode(raw)
	if blk == nil {
		return nil, fmt.Errorf("agent: %s 不是 PEM", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("agent: 解析身份密钥 %s: %w", path, err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("agent: %s 里的密钥类型不是 Ed25519", path)
	}
	return priv, nil
}

func loadOrCreateX25519(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		k, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("agent: 生成 WG 密钥: %w", err)
		}
		priv := k.Bytes()
		if err := os.WriteFile(path, priv, keyPerm); err != nil {
			return nil, fmt.Errorf("agent: 写 WG 私钥: %w", err)
		}
		return priv, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agent: 读 WG 私钥: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("agent: %s 应是 32 字节 X25519 私钥，实际 %d 字节", path, len(raw))
	}
	return raw, nil
}

// setWGKey 由私钥推出公钥并存下来。公钥是唯一对外可见的那半。
func (id *Identity) setWGKey(priv []byte) error {
	k, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("agent: WG 私钥不合法: %w", err)
	}
	id.wgPriv = append([]byte(nil), priv...)
	id.wgPub = k.PublicKey().Bytes()
	return nil
}

// WGPrivateKey 交给数据面装入 VPP（定案 3：agent 生成，控制器只是中继）。
func (id *Identity) WGPrivateKey() []byte { return append([]byte(nil), id.wgPriv...) }

// WGPublicKey 是要签进 keybinding、再交给对端做 `wireguard peer add` 的那 32 字节。
func (id *Identity) WGPublicKey() []byte { return append([]byte(nil), id.wgPub...) }

func (id *Identity) Serial() string   { return id.state.Serial }
func (id *Identity) TenantID() string { return id.state.TenantID }
func (id *Identity) NodeID() uint16   { return id.state.NodeID }
func (id *Identity) KeyGen() uint32   { return id.state.KeyGen }

// IdentityPublicKey 是 enroll 请求里那个 base64 public_key 的来源。
func (id *Identity) IdentityPublicKey() ed25519.PublicKey { return id.signer.Public() }

// SetEnrolled 收下控制器发来的证书与 CA，并**自己验一遍再说数**。
//
// 这一步不能省：enroll 走的是 claim code 鉴权，回来的 cert_pem/ca_pem 完全可以是
// 中间人换过的，而后续所有 mTLS 身份、以及"对端证书能不能信"都建立在这份 CA 上。
// 三件事必须同时成立：证书链到这份 CA、证书里的 srou:// URI 与控制器口头声明的
// (tenant, node_id) 一致、证书能用私钥加载。
func (id *Identity) SetEnrolled(tenantID string, nodeID uint16, certPEM, caPEM string) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return errors.New("agent: ca_pem 里没有可用的 PEM 证书")
	}
	leaf, err := tls.X509KeyPair([]byte(certPEM), mustPEM(id.signer.k))
	if err != nil {
		return fmt.Errorf("agent: 证书与本机身份密钥不配对: %w", err)
	}
	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		return fmt.Errorf("agent: 解析证书: %w", err)
	}
	if _, err := parsed.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return fmt.Errorf("agent: 证书链不到发回来的 CA: %w", err)
	}
	certTenant, certNode, err := keybinding.IdentityOf(parsed)
	if err != nil {
		return fmt.Errorf("agent: 证书里没有身份 URI: %w", err)
	}
	if certTenant != tenantID || certNode != nodeID {
		return fmt.Errorf("agent: 证书写的身份是 %s/node/%d，控制器口头说的是 %s/node/%d —— 拒收",
			certTenant, certNode, tenantID, nodeID)
	}

	id.cert = &leaf
	id.caPool = pool
	id.caPEM = []byte(caPEM)
	id.state.TenantID = tenantID
	id.state.NodeID = nodeID
	if err := id.save(); err != nil {
		return err
	}
	// 证书与 CA 也落盘：私钥不在里面，重启后不必重新 enroll。
	if err := os.WriteFile(filepath.Join(id.dir, nodeCertFile), []byte(certPEM), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(id.dir, caCertFile), []byte(caPEM), 0o644); err != nil {
		return err
	}
	id.ready = true
	return nil
}

// loadCerts 在 LoadOrCreate 里恢复已 enroll 的身份。缺文件是正常状态（还没注册），
// 但**存在却不自洽**必须报错：那说明目录被动过。
func (id *Identity) loadCerts() error {
	certPEM, err := os.ReadFile(filepath.Join(id.dir, nodeCertFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("agent: 读证书: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(id.dir, caCertFile))
	if err != nil {
		return fmt.Errorf("agent: 读 CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("agent: ca_cert.pem 里没有可用的 PEM 证书")
	}
	leaf, err := tls.X509KeyPair(certPEM, mustPEM(id.signer.k))
	if err != nil {
		return fmt.Errorf("agent: 已存的证书与本机身份密钥不配对: %w", err)
	}
	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		return fmt.Errorf("agent: 解析已存证书: %w", err)
	}
	// 重启时同样要验链：CA 文件被换过就该报错，而不是拿着一个不认识的证书去连。
	if _, err := parsed.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return fmt.Errorf("agent: 已存证书链不到已存 CA: %w", err)
	}
	certTenant, certNode, err := keybinding.IdentityOf(parsed)
	if err != nil {
		return fmt.Errorf("agent: 已存证书没有身份 URI: %w", err)
	}
	if certTenant != id.state.TenantID || certNode != id.state.NodeID {
		return fmt.Errorf("agent: 证书身份 %s/node/%d 与 %s 里记的 %s/node/%d 不一致",
			certTenant, certNode, stateFile, id.state.TenantID, id.state.NodeID)
	}
	id.cert = &leaf
	id.caPool = pool
	id.caPEM = caPEM
	id.ready = true
	return nil
}

// Enrolled 表示这台设备已经有可用证书，能走 mTLS 侧的所有接口。
func (id *Identity) Enrolled() bool { return id.ready }

func (id *Identity) save() error {
	raw, err := json.MarshalIndent(id.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(id.dir, stateFile), raw, 0o644)
}

// Binding 产出当前 WG 公钥的身份担保。未 enroll 时调用是错误，不是空值 ——
// 绑定消息里含 (tenant, node_id)，缺了它们签出来的东西对端一定验不过。
func (id *Identity) Binding() (keybinding.Binding, error) {
	if !id.ready {
		return keybinding.Binding{}, errors.New("agent: 尚未 enroll，签不出有效的 key binding")
	}
	return keybinding.Sign(id.signer, id.state.TenantID, id.state.NodeID, id.state.KeyGen, id.wgPub)
}

// RotateWGKey 换一把 WG 私钥并递增 key_gen。
//
// key_gen 必须单调递增：控制器与对端都靠它把"轮换"和"重放旧绑定"分开
// （见 keybinding.Binding 的注释）。落盘顺序是私钥先、state 后 —— 中途崩溃
// 只会导致公钥比 key_gen 新一次，多签一次无害；反过来则会用旧 gen 签新钥，
// 对端会当成重放丢掉。
func (id *Identity) RotateWGKey() error {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("agent: 生成新 WG 密钥: %w", err)
	}
	if err := os.WriteFile(filepath.Join(id.dir, wgKeyFile), k.Bytes(), keyPerm); err != nil {
		return fmt.Errorf("agent: 写新 WG 私钥: %w", err)
	}
	if err := id.setWGKey(k.Bytes()); err != nil {
		return err
	}
	id.state.KeyGen++
	return id.save()
}

// TLSConfig 是给控制面客户端用的配置：只认这份 CA，只出示这张证书。
//
// 不关校验、不设 InsecureSkipVerify —— 那条开关会把"证书被换过"变成正常运行。
func (id *Identity) TLSConfig(serverName string) (*tls.Config, error) {
	if !id.ready {
		return nil, errors.New("agent: 尚未 enroll，配不出 mTLS 客户端")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{*id.cert},
		RootCAs:      id.caPool,
		ServerName:   serverName,
	}, nil
}

// CAPool 是"我信任哪个租户 CA"的唯一来源：以后校验对端证书要用它，
// 而不是各自从响应里临时拼一份。
func (id *Identity) CAPool() (*x509.CertPool, error) {
	if !id.ready {
		return nil, errors.New("agent: 尚未 enroll，没有 CA 可用")
	}
	return id.caPool, nil
}

// CAPEM 是给数据面/日志用的原文（CA 是公开材料，可以落日志）。
func (id *Identity) CAPEM() string { return string(id.caPEM) }

// mustPEM 把身份私钥编成 tls.X509KeyPair 要的 PEM。
//
// 名字叫 must 是因为这里不可能失败：私钥是本包自己生成并编码过的。
func mustPEM(priv ed25519.PrivateKey) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		panic("agent: 自己生成的 Ed25519 私钥编不出 PKCS#8: " + err.Error())
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// serverNameFromURL 从 https://host:port 取出主机名，供 tls.Config.ServerName 用。
//
// ServerName 必须是主机名而不是 "host:port"，也不能带 IP 之外的花活；
// 留空则 TLS 校验直接失败，所以这里宁可报错也不要静默传一个错的。
func serverNameFromURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("agent: 地址 %q 解析不了: %w", base, err)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("agent: 地址里没有主机名: %q", base)
	}
	return host, nil
}
