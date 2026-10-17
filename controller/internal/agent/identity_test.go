package agent

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sdwan/controller/internal/keybinding"
)

func newIdentity(t *testing.T, serial string) *Identity {
	t.Helper()
	id, err := LoadOrCreate(filepath.Join(t.TempDir(), "id"), serial)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	return id
}

func TestLoadOrCreateKeysAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "id")
	id, err := LoadOrCreate(dir, "CPE-0001")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if id.Enrolled() {
		t.Error("刚生成密钥就 Enrolled()=true：此刻还没有任何证书")
	}
	if got := len(id.IdentityPublicKey()); got != ed25519.PublicKeySize {
		t.Errorf("身份公钥 %d 字节，期望 %d", got, ed25519.PublicKeySize)
	}
	if got := len(id.WGPublicKey()); got != keybinding.X25519PubLen {
		t.Errorf("WG 公钥 %d 字节，期望 %d", got, keybinding.X25519PubLen)
	}

	// 私钥权限必须是 0600：身份密钥一旦泄露，攻击者就能给任意 X25519 公钥签出
	// 合法 binding —— 定案 3 依赖的正是"只有设备自己能担保自己的隧道公钥"。
	for _, f := range []string{deviceKeyFile, wgKeyFile} {
		info, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if perm := info.Mode().Perm(); perm != keyPerm {
			t.Errorf("%s 权限 %o，期望 %o", f, perm, keyPerm)
		}
	}
}

func TestReloadKeepsSameKeys(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "id")
	first, err := LoadOrCreate(dir, "CPE-0001")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	second, err := LoadOrCreate(dir, "CPE-0001")
	if err != nil {
		t.Fatalf("重新加载: %v", err)
	}
	// 重启换密钥等于换身份：证书签在旧的 Ed25519 公钥上，WG 公钥还得重新注册一遍。
	if !bytes.Equal(first.IdentityPublicKey(), second.IdentityPublicKey()) {
		t.Error("重新加载后身份公钥变了")
	}
	if !bytes.Equal(first.WGPublicKey(), second.WGPublicKey()) ||
		!bytes.Equal(first.WGPrivateKey(), second.WGPrivateKey()) {
		t.Error("重新加载后 WG 密钥变了")
	}
}

func TestUnenrolledRefusesToSignOrDial(t *testing.T) {
	id := newIdentity(t, "CPE-0001")

	// 绑定消息里含 (tenant, node_id)，enroll 之前这两个值还不存在；
	// mTLS 客户端要有证书才能建。空值返回会让 agent 带着垃圾往下跑。
	if _, err := id.Binding(); err == nil {
		t.Error("未 enroll 却签出了 binding")
	}
	if _, err := id.TLSConfig("127.0.0.1"); err == nil {
		t.Error("未 enroll 却配出了 mTLS")
	}
	if _, err := id.CAPool(); err == nil {
		t.Error("未 enroll 却有 CA 可用")
	}
}

func TestSetEnrolledVerifiesChainAndSpokenIdentity(t *testing.T) {
	pki := newTestPKI(t)
	id := newIdentity(t, "CPE-0001")
	certPEM := pki.issueDevice(t, id.IdentityPublicKey(), "t1", 7)

	// 控制器口头声明的 node_id 与证书里写的不一致：拒收。这是 agent 侧唯一的
	// "控制器别把别人塞给我"检查，跳过它等于让控制器随意指认身份。
	if err := id.SetEnrolled("t1", 9, certPEM, pki.caPEM); err == nil {
		t.Fatal("证书是 node/7、控制器说 node/9，居然收下了")
	} else if !strings.Contains(err.Error(), "拒收") {
		t.Errorf("错误信息没点明是身份不符: %v", err)
	}
	if id.Enrolled() {
		t.Error("SetEnrolled 失败之后 Enrolled() 仍为 true")
	}

	// 链不到应答里的 CA：中间人用自己 CA 签一张证书冒充控制器，死在这里。
	other := newTestPKI(t)
	if err := id.SetEnrolled("t1", 7, certPEM, other.caPEM); err == nil {
		t.Fatal("证书链不到应答里的 CA，居然收下了")
	}

	// 证书不是本机身份密钥的：私钥与证书不配对，tls.X509KeyPair 那一层就挡住。
	foreign := newIdentity(t, "CPE-9999")
	foreignCert := pki.issueDevice(t, foreign.IdentityPublicKey(), "t1", 7)
	if err := id.SetEnrolled("t1", 7, foreignCert, pki.caPEM); err == nil {
		t.Fatal("收下了一张本机私钥解不开的证书")
	}

	if err := id.SetEnrolled("t1", 7, certPEM, pki.caPEM); err != nil {
		t.Fatalf("正常 enroll: %v", err)
	}
	if !id.Enrolled() || id.TenantID() != "t1" || id.NodeID() != 7 {
		t.Fatalf("enroll 后状态不对: enrolled=%v tenant=%q node=%d", id.Enrolled(), id.TenantID(), id.NodeID())
	}

	// 以"对端"的视角重放一遍定案 3 的信任链：固定 CA + 控制器转发的证书 +
	// agent 自签的绑定，三者必须合成一个可信的 X25519 公钥。
	b, err := id.Binding()
	if err != nil {
		t.Fatalf("Binding: %v", err)
	}
	peerCert, err := keybinding.VerifyPeerCert(pki.pool, certPEM, "t1", 7, b)
	if err != nil {
		t.Fatalf("对端视角校验失败: %v", err)
	}
	identity, ok := peerCert.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("节点证书公钥类型是 %T，不是 Ed25519", peerCert.PublicKey)
	}
	attested, err := b.Verify("t1", 7, identity)
	if err != nil {
		t.Fatalf("绑定校验: %v", err)
	}
	if !bytes.Equal(attested, id.WGPublicKey()) {
		t.Errorf("绑定担保的公钥 %x 与本机 WG 公钥 %x 不一致", attested, id.WGPublicKey())
	}
	// 换一个人问：同一个绑定不能既算作 node/7 又算作 node/8。
	if _, err := b.Verify("t1", 8, id.IdentityPublicKey()); err == nil {
		t.Error("为 node/7 签的绑定被 node/8 接受了")
	}
}

func TestReloadAfterEnrollRestoresIdentity(t *testing.T) {
	pki := newTestPKI(t)
	dir := filepath.Join(t.TempDir(), "id")
	id, err := LoadOrCreate(dir, "CPE-0001")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	certPEM := pki.issueDevice(t, id.IdentityPublicKey(), "t1", 7)
	if err := id.SetEnrolled("t1", 7, certPEM, pki.caPEM); err != nil {
		t.Fatalf("SetEnrolled: %v", err)
	}
	before, err := id.Binding()
	if err != nil {
		t.Fatalf("Binding: %v", err)
	}

	reloaded, err := LoadOrCreate(dir, "CPE-0001")
	if err != nil {
		t.Fatalf("重新加载已 enroll 的身份: %v", err)
	}
	if !reloaded.Enrolled() || reloaded.NodeID() != 7 || reloaded.TenantID() != "t1" {
		t.Fatalf("重启后身份丢了: enrolled=%v tenant=%q node=%d", reloaded.Enrolled(), reloaded.TenantID(), reloaded.NodeID())
	}
	after, err := reloaded.Binding()
	if err != nil {
		t.Fatalf("重启后 Binding: %v", err)
	}
	// 重启不该触发轮换：key_gen 一涨，对端就要重装一次 WG peer。
	if after.KeyGen != before.KeyGen || after.X25519Pub != before.X25519Pub {
		t.Errorf("重启后绑定漂移: key_gen %d->%d, pub %s->%s",
			before.KeyGen, after.KeyGen, before.X25519Pub, after.X25519Pub)
	}
	if _, err := reloaded.TLSConfig("127.0.0.1"); err != nil {
		t.Errorf("重启后配不出 mTLS: %v", err)
	}
	if reloaded.CAPEM() != pki.caPEM {
		t.Error("重启后 CA 原文与 enroll 时收到的不是同一份")
	}
}

func TestReloadRefusesTamperedMaterial(t *testing.T) {
	pki := newTestPKI(t)
	dir := filepath.Join(t.TempDir(), "id")
	id, err := LoadOrCreate(dir, "CPE-0001")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if err := id.SetEnrolled("t1", 7, pki.issueDevice(t, id.IdentityPublicKey(), "t1", 7), pki.caPEM); err != nil {
		t.Fatalf("SetEnrolled: %v", err)
	}

	// 情况一：ca_cert.pem 被换成别人的 CA。证书还在，但不再有人担保它 ——
	// 必须报错，而不是拿一张不认识的东西去连控制器。
	other := newTestPKI(t)
	caPath := filepath.Join(dir, caCertFile)
	if err := os.WriteFile(caPath, []byte(other.caPEM), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir, "CPE-0001"); err == nil {
		t.Error("CA 被换过后仍能加载身份")
	} else if !strings.Contains(err.Error(), "链不到") {
		t.Errorf("错误没点明是链校验失败: %v", err)
	}
	if err := os.WriteFile(caPath, []byte(pki.caPEM), 0o644); err != nil {
		t.Fatal(err)
	}

	// 情况二：node_cert.pem 被换成同 CA 下另一台设备的证书（同租户、node/8）。
	// state.json 记的是 node/7，两者不符就是目录被动过，不是正常重启。
	foreign := pki.issueDevice(t, id.IdentityPublicKey(), "t1", 8)
	if err := os.WriteFile(filepath.Join(dir, nodeCertFile), []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir, "CPE-0001"); err == nil {
		t.Error("证书被换成 node/8 之后仍能加载身份")
	} else if !strings.Contains(err.Error(), "不一致") {
		t.Errorf("错误没点明是身份不符: %v", err)
	}
}

func TestRotateWGKeyBumpsKeyGen(t *testing.T) {
	pki := newTestPKI(t)
	dir := filepath.Join(t.TempDir(), "id")
	id, err := LoadOrCreate(dir, "CPE-0001")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if err := id.SetEnrolled("t1", 7, pki.issueDevice(t, id.IdentityPublicKey(), "t1", 7), pki.caPEM); err != nil {
		t.Fatalf("SetEnrolled: %v", err)
	}
	before, err := id.Binding()
	if err != nil {
		t.Fatalf("Binding: %v", err)
	}
	oldPub := append([]byte(nil), id.WGPublicKey()...)

	if err := id.RotateWGKey(); err != nil {
		t.Fatalf("RotateWGKey: %v", err)
	}
	after, err := id.Binding()
	if err != nil {
		t.Fatalf("轮换后 Binding: %v", err)
	}
	if bytes.Equal(oldPub, id.WGPublicKey()) {
		t.Error("轮换后 WG 公钥没变")
	}
	if after.KeyGen != before.KeyGen+1 {
		t.Errorf("key_gen %d -> %d，期望 +1", before.KeyGen, after.KeyGen)
	}

	// key_gen 的作用在这里：旧绑定在签名层面依然"合法"（身份密钥没换），
	// 但它担保的公钥已经不是当前那把。对端靠 gen 单调递增把"轮换"和"重放"分开。
	rotatedPub, err := before.Verify("t1", 7, id.IdentityPublicKey())
	if err != nil {
		t.Fatalf("旧绑定按旧 gen 仍应可验: %v", err)
	}
	if !bytes.Equal(rotatedPub, oldPub) || bytes.Equal(rotatedPub, id.WGPublicKey()) {
		t.Error("旧绑定担保的公钥不是轮换前那把")
	}
	newPub, err := after.Verify("t1", 7, id.IdentityPublicKey())
	if err != nil {
		t.Fatalf("新绑定校验: %v", err)
	}
	if !bytes.Equal(newPub, id.WGPublicKey()) {
		t.Error("新绑定担保的公钥不是轮换后那把")
	}

	// 私钥与 state 都必须落盘：中途崩溃后 key_gen 不能倒退，
	// 否则新公钥会顶着旧 gen 出现，对端只会当成重放丢掉。
	reloaded, err := LoadOrCreate(dir, "CPE-0001")
	if err != nil {
		t.Fatalf("轮换后重新加载: %v", err)
	}
	if reloaded.KeyGen() != after.KeyGen {
		t.Errorf("key_gen 没落盘：磁盘 %d，内存 %d", reloaded.KeyGen(), after.KeyGen)
	}
	if !bytes.Equal(reloaded.WGPrivateKey(), id.WGPrivateKey()) {
		t.Error("轮换后的新私钥没落盘")
	}
}
