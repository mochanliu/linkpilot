// Package agent 是 CPE 侧的那一半：设备身份、与 vBOND 的对话，
// 以及把控制器意图落到 VPP 上的接缝。
//
// 这个包里的东西都只依赖标准库（和 go.mod 的零第三方约束一致）。
// 协议类型一律来自 internal/wire —— 服务端与 agent 共用同一份定义，
// 字段不可能两边各写各的。
package agent

import (
	"bytes"
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
	wgKeySlotFmt  = "wg_x25519.slot%d.key"
	stateFile     = "state.json"
	nodeCertFile  = "node_cert.pem"
	caCertFile    = "ca_cert.pem"
	keyPerm       = 0o600
	dirPerm       = 0o700
)

// wgKeyPath 是第 slot 把 WG 私钥的文件名。
//
// 槽 0 沿用 wg_x25519.key、字节不动，不只是省事：存量设备上那条文件里的密钥正被
// 每一条在跑的隧道用着，给它换个名字等于换密钥 —— 对端 peer 里记的公钥就对不上了，
// 那是一次真握手。所以升级只新增槽 1..7 的文件。
func wgKeyPath(slot int) string {
	if slot == 0 {
		return wgKeyFile
	}
	return fmt.Sprintf(wgKeySlotFmt, slot)
}

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
//
// KeyGens 按槽下标存：一把密钥一个轮换计数，换槽 3 的密钥不会让槽 0 的绑定
// 看起来也换了（控制器就是按这个计数把"轮换"和"重放旧绑定"分开的）。
type state struct {
	Serial   string                        `json:"serial"`
	KeyGens  [keybinding.WGKeySlots]uint32 `json:"key_gens"`
	TenantID string                        `json:"tenant_id"`
	NodeID   uint16                        `json:"node_id"`
}

// decodeState 读 state.json，把两种历史格式并成一种。
//
// v1 的设备只有一把 WG 密钥，key_gen 是一个标量 —— 那把就是今天的槽 0。
// 不接住这个值，升级后的第一次 rotate 会把计数从 0 起，控制器会把它当成重放
// 拒掉（见 cmd/sr-ctrl/main.go 的 applyBinding）。这是本地存档的迁移，不是
// 线上协议的兼容：v1 的**签名**在这里不认。
func decodeState(raw []byte) (state, error) {
	var onDisk struct {
		Serial   string                        `json:"serial"`
		KeyGen   uint32                        `json:"key_gen"`
		KeyGens  [keybinding.WGKeySlots]uint32 `json:"key_gens"`
		TenantID string                        `json:"tenant_id"`
		NodeID   uint16                        `json:"node_id"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		return state{}, fmt.Errorf("agent: 读 %s: %w", stateFile, err)
	}
	st := state{Serial: onDisk.Serial, KeyGens: onDisk.KeyGens, TenantID: onDisk.TenantID, NodeID: onDisk.NodeID}
	if st.KeyGens == [keybinding.WGKeySlots]uint32{} {
		st.KeyGens[0] = onDisk.KeyGen
	}
	return st, nil
}

// Identity 是一台 CPE 的完整身份：签名密钥、八把 WG 密钥对、以及控制器签发的证书。
//
// 顺序有硬性依赖：X25519 公钥要用**身份密钥**签名（keybinding），而签名消息里
// 含 (tenant, node_id) —— 这两个值只有 enroll 之后才知道。所以私钥先生成、
// 先落盘，绑定留到 SetEnrolled 之后才谈（见 wire.EnrollRequest 的同一说明）。
type Identity struct {
	dir    string
	signer deviceSigner
	// wgPriv/wgPub 按槽下标存，长度恒为 keybinding.WGKeySlots。第 n 条隧道
	// （同一设备对内）两端各用第 n 把 —— 这是"同对设备能建 8 条隧道"的根据。
	wgPriv [keybinding.WGKeySlots][]byte // 32 字节，交给 VPP 的那把（定案 3）
	wgPub  [keybinding.WGKeySlots][]byte
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

	for slot := 0; slot < keybinding.WGKeySlots; slot++ {
		wg, err := loadOrCreateX25519(filepath.Join(dir, wgKeyPath(slot)))
		if err != nil {
			return nil, err
		}
		if err := id.setWGKey(slot, wg); err != nil {
			return nil, err
		}
	}
	// 「八把两两不同」是同设备对第 n 条隧道各用第 n 把不撞 VPP -116 的前提之一，
	// 而这件事 agent 自己就能查。放在启动时查，不留到隧道建到一半由数据面报错。
	if err := id.checkWGKeysDistinct(); err != nil {
		return nil, err
	}

	if raw, err := os.ReadFile(filepath.Join(dir, stateFile)); err == nil {
		st, err := decodeState(raw)
		if err != nil {
			return nil, err
		}
		id.state = st
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
func (id *Identity) setWGKey(slot int, priv []byte) error {
	k, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("agent: 槽 %d 的 WG 私钥不合法: %w", slot, err)
	}
	id.wgPriv[slot] = append([]byte(nil), priv...)
	id.wgPub[slot] = k.PublicKey().Bytes()
	return nil
}

// checkWGKeysDistinct 保证八把公钥互不相同。
//
// 这是密钥槽模型的前提：同设备对的两条隧道靠"各自用第 n 把"来让对端公钥不同，
// 如果两把其实是同一把，那两条隧道在 VPP 里就是同一个人 —— 第二条 -116。
// 报错只带槽号：公钥本身是公开材料，但这里没必要让日志变成密钥清单。
func (id *Identity) checkWGKeysDistinct() error {
	for a := 0; a < keybinding.WGKeySlots; a++ {
		for b := a + 1; b < keybinding.WGKeySlots; b++ {
			if !bytes.Equal(id.wgPub[a], id.wgPub[b]) {
				continue
			}
			return fmt.Errorf("agent: 槽 %d 与槽 %d 的 WG 公钥相同（%s 与 %s 是同一把密钥）。"+
				"同设备对的两条隧道会因此撞在数据面的 peer 去重上，删掉其中一个文件让 agent 重新生成",
				a, b, wgKeyPath(a), wgKeyPath(b))
		}
	}
	return nil
}

// WGPrivateKey 交给数据面装入 VPP（定案 3：agent 生成，控制器只是中继）。
//
// slot 是从控制器下发的隧道规格里来的，属于外部输入，所以越界要报错而不是 panic：
// agent 是长期进程，一个坏字段不该把已经在转发的隧道一起带走。
func (id *Identity) WGPrivateKey(slot int) ([]byte, error) {
	if err := checkSlot(slot); err != nil {
		return nil, err
	}
	return append([]byte(nil), id.wgPriv[slot]...), nil
}

// WGPublicKey 是要签进 keybinding、再交给对端做 `wireguard peer add` 的那 32 字节。
func (id *Identity) WGPublicKey(slot int) ([]byte, error) {
	if err := checkSlot(slot); err != nil {
		return nil, err
	}
	return append([]byte(nil), id.wgPub[slot]...), nil
}

func checkSlot(slot int) error {
	if slot < 0 || slot >= keybinding.WGKeySlots {
		return fmt.Errorf("agent: 密钥槽号 %d 越界（本机只有 %d 把 WG 密钥，槽号 0..%d）",
			slot, keybinding.WGKeySlots, keybinding.WGKeySlots-1)
	}
	return nil
}

func (id *Identity) Serial() string   { return id.state.Serial }
func (id *Identity) TenantID() string { return id.state.TenantID }
func (id *Identity) NodeID() uint16   { return id.state.NodeID }

// KeyGen 是第 slot 把密钥的轮换计数。每槽独立：换槽 3 不会让槽 0 的绑定看起来也换了。
func (id *Identity) KeyGen(slot int) (uint32, error) {
	if err := checkSlot(slot); err != nil {
		return 0, err
	}
	return id.state.KeyGens[slot], nil
}

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

// Binding 产出第 slot 把 WG 公钥的身份担保。未 enroll 时调用是错误，不是空值 ——
// 绑定消息里含 (tenant, node_id)，缺了它们签出来的东西对端一定验不过。
func (id *Identity) Binding(slot int) (keybinding.Binding, error) {
	if !id.ready {
		return keybinding.Binding{}, errors.New("agent: 尚未 enroll，签不出有效的 key binding")
	}
	pub, err := id.WGPublicKey(slot)
	if err != nil {
		return keybinding.Binding{}, err
	}
	gen, err := id.KeyGen(slot)
	if err != nil {
		return keybinding.Binding{}, err
	}
	return keybinding.Sign(id.signer, id.state.TenantID, id.state.NodeID, uint8(slot), gen, pub)
}

// Bindings 一次交出八份担保，下标就是槽号。
//
// 心跳/上报走这个而不是逐槽点调：一份隧道规格里对端用哪把是控制器决定的，
// 而控制器只有拿到全部八份才能回答"这条隧道我用第 n 把"。八份不可互换，
// 所以这里也不能压成一份。
func (id *Identity) Bindings() ([]keybinding.Binding, error) {
	out := make([]keybinding.Binding, keybinding.WGKeySlots)
	for slot := range out {
		b, err := id.Binding(slot)
		if err != nil {
			return nil, err
		}
		out[slot] = b
	}
	return out, nil
}

// RotateWGKey 换第 slot 把 WG 私钥并递增那一槽的 key_gen。
//
// key_gen 必须单调递增：控制器与对端都靠它把"轮换"和"重放旧绑定"分开
// （见 keybinding.Binding 的注释）。落盘顺序是私钥先、state 后 —— 中途崩溃
// 只会导致公钥比 key_gen 新一次，多签一次无害；反过来则会用旧 gen 签新钥，
// 对端会当成重放丢掉。
//
// 只换一把：其余七把的隧道不受影响，这正是每槽独立计数的意义 —— 换槽 3 之后
// 槽 0 的担保仍是旧 gen、旧公钥，对端不必重握手。
func (id *Identity) RotateWGKey(slot int) error {
	if err := checkSlot(slot); err != nil {
		return err
	}
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("agent: 生成新 WG 密钥: %w", err)
	}
	if err := os.WriteFile(filepath.Join(id.dir, wgKeyPath(slot)), k.Bytes(), keyPerm); err != nil {
		return fmt.Errorf("agent: 写新 WG 私钥: %w", err)
	}
	if err := id.setWGKey(slot, k.Bytes()); err != nil {
		return err
	}
	id.state.KeyGens[slot]++
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
