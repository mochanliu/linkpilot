package agent

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
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

// slotFiles 按槽号列出八个私钥文件名。测试里反复要用"第 n 把落在哪个文件"，
// 而这件事恰恰是存量升级的关键（槽 0 必须是老名字），所以把它写成一处可断言的东西。
func slotFiles() []string {
	out := make([]string, keybinding.WGKeySlots)
	for slot := range out {
		out[slot] = wgKeyPath(slot)
	}
	return out
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

	// 八把在启动这一次就全部生成，不留"建到第 n 条隧道再补钥匙"的分支：那条路上
	// 隧道规格已经发给对端了，本端才发现没钥匙，对端会拿着一个永远装不上的公钥重试。
	seen := map[string]int{}
	for slot, name := range slotFiles() {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("槽 %d 的私钥文件 %s: %v", slot, name, err)
		}
		// 私钥权限必须是 0600：身份密钥一旦泄露，攻击者就能给任意 X25519 公钥签出
		// 合法 binding —— 定案 3 依赖的正是"只有设备自己能担保自己的隧道公钥"。
		if perm := info.Mode().Perm(); perm != keyPerm {
			t.Errorf("%s 权限 %o，期望 %o", name, perm, keyPerm)
		}
		pub, err := id.WGPublicKey(slot)
		if err != nil {
			t.Fatalf("槽 %d 的 WG 公钥: %v", slot, err)
		}
		if len(pub) != keybinding.X25519PubLen {
			t.Errorf("槽 %d 的 WG 公钥 %d 字节，期望 %d", slot, len(pub), keybinding.X25519PubLen)
		}
		if prev, dup := seen[string(pub)]; dup {
			t.Fatalf("槽 %d 与槽 %d 的公钥相同：同设备对的两条隧道会在数据面撞 peer 去重", prev, slot)
		}
		seen[string(pub)] = slot
	}

	// 槽 0 沿用升级前那个文件名。这一条看着像命名洁癖，其实是"存量零中断"的根据：
	// 存量设备上那条文件里的密钥正被每一条在跑的隧道用着，给它换名字等于换密钥，
	// 对端 peer 里记的公钥就对不上 —— 那是一次真握手。
	if wgKeyPath(0) != wgKeyFile {
		t.Errorf("槽 0 的文件名是 %q，期望 %q：换名会让存量隧道重新握手", wgKeyPath(0), wgKeyFile)
	}

	// 身份私钥同样要 0600：它给八把 WG 公钥做担保，泄露一次就等于八条隧道同时失守。
	dk, err := os.Stat(filepath.Join(dir, deviceKeyFile))
	if err != nil {
		t.Fatalf("%s: %v", deviceKeyFile, err)
	}
	if perm := dk.Mode().Perm(); perm != keyPerm {
		t.Errorf("%s 权限 %o，期望 %o", deviceKeyFile, perm, keyPerm)
	}
}

// TestLoadOrCreateRefusesDuplicateSlotKeys 是启动自检的那一条：两把其实是同一把时，
// 同设备对的两条隧道在 VPP 里就是同一个人，第二条 -116。这件事 agent 自己查得到，
// 所以不该留到隧道装一半由数据面报错。
func TestLoadOrCreateRefusesDuplicateSlotKeys(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "id")
	if _, err := LoadOrCreate(dir, "CPE-0001"); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	// 模拟"运维手工把槽 0 的密钥拷进槽 4"（或者一次半途的 cp 把两个文件弄成同一份）。
	raw, err := os.ReadFile(filepath.Join(dir, wgKeyPath(0)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, wgKeyPath(4)), raw, keyPerm); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir, "CPE-0001"); err == nil {
		t.Fatal("槽 0 与槽 4 是同一把密钥，加载居然成功了")
	} else if !strings.Contains(err.Error(), "槽 0 与槽 4") {
		t.Errorf("错误没点出撞在一起的两个槽号: %v", err)
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
	for slot := range slotFiles() {
		pubA, err := first.WGPublicKey(slot)
		if err != nil {
			t.Fatalf("槽 %d 公钥: %v", slot, err)
		}
		pubB, err := second.WGPublicKey(slot)
		if err != nil {
			t.Fatalf("槽 %d 公钥: %v", slot, err)
		}
		privA, err := first.WGPrivateKey(slot)
		if err != nil {
			t.Fatalf("槽 %d 私钥: %v", slot, err)
		}
		privB, err := second.WGPrivateKey(slot)
		if err != nil {
			t.Fatalf("槽 %d 私钥: %v", slot, err)
		}
		if !bytes.Equal(pubA, pubB) || !bytes.Equal(privA, privB) {
			t.Errorf("重新加载后槽 %d 的 WG 密钥变了", slot)
		}
	}
}

func TestUnenrolledRefusesToSignOrDial(t *testing.T) {
	id := newIdentity(t, "CPE-0001")

	// 绑定消息里含 (tenant, node_id)，enroll 之前这两个值还不存在；
	// mTLS 客户端要有证书才能建。空值返回会让 agent 带着垃圾往下跑。
	if _, err := id.Binding(0); err == nil {
		t.Error("未 enroll 却签出了 binding")
	}
	if _, err := id.Bindings(); err == nil {
		t.Error("未 enroll 却签出了一整套 binding")
	}
	if _, err := id.TLSConfig("127.0.0.1"); err == nil {
		t.Error("未 enroll 却配出了 mTLS")
	}
	if _, err := id.CAPool(); err == nil {
		t.Error("未 enroll 却有 CA 可用")
	}
}

// TestSlotIsBoundsChecked 把槽号当它真正的样子看：它是控制器下发的隧道规格里
// 带过来的外部输入。agent 是长期进程，一个越界字段不该 panic，否则会把已经在
// 转发的其它隧道一起带走。
func TestSlotIsBoundsChecked(t *testing.T) {
	pki := newTestPKI(t)
	id := newIdentity(t, "CPE-0001")
	if err := id.SetEnrolled("t1", 7, pki.issueDevice(t, id.IdentityPublicKey(), "t1", 7), pki.caPEM); err != nil {
		t.Fatalf("SetEnrolled: %v", err)
	}
	for _, bad := range []int{-1, keybinding.WGKeySlots, keybinding.WGKeySlots + 7} {
		if _, err := id.WGPublicKey(bad); err == nil {
			t.Errorf("槽 %d 越界却返回了公钥", bad)
		}
		if _, err := id.WGPrivateKey(bad); err == nil {
			t.Errorf("槽 %d 越界却返回了私钥", bad)
		}
		if _, err := id.KeyGen(bad); err == nil {
			t.Errorf("槽 %d 越界却返回了 key_gen", bad)
		}
		if _, err := id.Binding(bad); err == nil {
			t.Errorf("槽 %d 越界却签出了 binding", bad)
		}
		if err := id.RotateWGKey(bad); err == nil {
			t.Errorf("槽 %d 越界却轮换成功了（轮换会直接写文件）", bad)
		}
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
	// 故意用槽 3 而不是槽 0：如果哪一层把槽号丢了，担保会退回 attesting 某一把
	// 别的密钥，而这个只在槽 0 上测不出来。
	const slot = 3
	b, err := id.Binding(slot)
	if err != nil {
		t.Fatalf("Binding: %v", err)
	}
	if b.Slot != slot {
		t.Errorf("担保里的槽号是 %d，签的是 %d", b.Slot, slot)
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
	wantPub, err := id.WGPublicKey(slot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(attested, wantPub) {
		t.Errorf("绑定担保的公钥与本机槽 %d 的 WG 公钥不一致", slot)
	}
	// 一份担保只担保一把：它不能同时算作别的任何槽。
	for other := range slotFiles() {
		if other == slot {
			continue
		}
		otherPub, err := id.WGPublicKey(other)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(attested, otherPub) {
			t.Errorf("槽 %d 的担保也算作槽 %d", slot, other)
		}
	}
	// 换一个人问：同一个绑定不能既算作 node/7 又算作 node/8。
	if _, err := b.Verify("t1", 8, id.IdentityPublicKey()); err == nil {
		t.Error("为 node/7 签的绑定被 node/8 接受了")
	}
}

func TestBindingsAreAllSlotsInOrder(t *testing.T) {
	pki := newTestPKI(t)
	id := newIdentity(t, "CPE-0001")
	if err := id.SetEnrolled("t1", 7, pki.issueDevice(t, id.IdentityPublicKey(), "t1", 7), pki.caPEM); err != nil {
		t.Fatalf("SetEnrolled: %v", err)
	}
	bs, err := id.Bindings()
	if err != nil {
		t.Fatalf("Bindings: %v", err)
	}
	// 心跳/映射一次交八份，控制器按"下标 = 槽号"存。长度或顺序一错，
	// 控制器就会把槽 5 的公钥装到槽 2 的隧道上。
	if len(bs) != keybinding.WGKeySlots {
		t.Fatalf("担保 %d 份，期望 %d 份", len(bs), keybinding.WGKeySlots)
	}
	for slot := range bs {
		if bs[slot].Slot != uint8(slot) {
			t.Fatalf("下标 %d 的担保写的是槽 %d", slot, bs[slot].Slot)
		}
		one, err := id.Binding(slot)
		if err != nil {
			t.Fatalf("槽 %d Binding: %v", slot, err)
		}
		if bs[slot] != one {
			t.Errorf("槽 %d 的批量担保与逐槽签的不一致（key_gen 或公钥漂移）", slot)
		}
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
	before, err := id.Bindings()
	if err != nil {
		t.Fatalf("Bindings: %v", err)
	}

	reloaded, err := LoadOrCreate(dir, "CPE-0001")
	if err != nil {
		t.Fatalf("重新加载已 enroll 的身份: %v", err)
	}
	if !reloaded.Enrolled() || reloaded.NodeID() != 7 || reloaded.TenantID() != "t1" {
		t.Fatalf("重启后身份丢了: enrolled=%v tenant=%q node=%d", reloaded.Enrolled(), reloaded.TenantID(), reloaded.NodeID())
	}
	after, err := reloaded.Bindings()
	if err != nil {
		t.Fatalf("重启后 Bindings: %v", err)
	}
	// 重启不该触发轮换：任一格 key_gen 一涨，对端就要重装一次 WG peer。
	for slot := range before {
		if before[slot] != after[slot] {
			t.Errorf("重启后槽 %d 的绑定漂移: key_gen %d->%d, pub %s->%s",
				slot, before[slot].KeyGen, after[slot].KeyGen, before[slot].X25519Pub, after[slot].X25519Pub)
		}
	}
	if _, err := reloaded.TLSConfig("127.0.0.1"); err != nil {
		t.Errorf("重启后配不出 mTLS: %v", err)
	}
	if reloaded.CAPEM() != pki.caPEM {
		t.Error("重启后 CA 原文与 enroll 时收到的不是同一份")
	}
}

// TestDecodeStateMigratesV1KeyGen 接住的是 lab 里真实存在的那种存档：v1 设备只有
// 一把 WG 密钥，key_gen 是一个标量。那把就是今天的槽 0，不接住这个值，升级后的
// 第一次轮换会把计数从 0 起，控制器会把它当成重放拒掉。
//
// 这是**本地存档**的迁移，不是线上协议的兼容 —— v1 的签名在这里不认（见
// internal/keybinding/binding_test.go 的 TestV1SignatureIsRefused）。
func TestDecodeStateMigratesV1KeyGen(t *testing.T) {
	raw := []byte(`{"serial":"CPE-0001","key_gen":5,"tenant_id":"t1","node_id":7}`)
	st, err := decodeState(raw)
	if err != nil {
		t.Fatalf("decodeState(v1 存档): %v", err)
	}
	if st.KeyGens[0] != 5 {
		t.Errorf("v1 的 key_gen 没迁到槽 0：%d", st.KeyGens[0])
	}
	for slot := 1; slot < keybinding.WGKeySlots; slot++ {
		if st.KeyGens[slot] != 0 {
			t.Errorf("槽 %d 的计数本该是 0（这把密钥还没换过），实际 %d", slot, st.KeyGens[slot])
		}
	}

	// 两种字段同时出现时以 key_gens 为准：能写出 key_gens 的 agent 必然是 v2，
	// 那个标量只是它顺手留在磁盘上的旧痕迹。
	raw = []byte(`{"serial":"CPE-0001","key_gen":5,"key_gens":[0,0,3,0,0,0,0,0],"tenant_id":"t1","node_id":7}`)
	st, err = decodeState(raw)
	if err != nil {
		t.Fatalf("decodeState(v2 存档): %v", err)
	}
	if st.KeyGens[2] != 3 || st.KeyGens[0] != 0 {
		t.Errorf("v2 存档没按 key_gens 读: %+v", st.KeyGens)
	}

	// 落盘再读回来，必须还是同一套计数 —— 否则 RotateWGKey 的递增活不过一次重启。
	// 先正常入网（担保签得出来的前提是磁盘上有 CA 签的那张证书），再把 v2 那份
	// 存档写回 state.json，模拟"agent 重启后读到的磁盘"。
	dir := filepath.Join(t.TempDir(), "id")
	pki := newTestPKI(t)
	id, err := LoadOrCreate(dir, "CPE-0001")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if err := id.SetEnrolled("t1", 7, pki.issueDevice(t, id.IdentityPublicKey(), "t1", 7), pki.caPEM); err != nil {
		t.Fatalf("SetEnrolled: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	id, err = LoadOrCreate(dir, "CPE-0001")
	if err != nil {
		t.Fatalf("重启后 LoadOrCreate: %v", err)
	}
	if !id.Enrolled() {
		t.Fatal("重启后身份不再是已入网状态")
	}
	gen, err := id.KeyGen(2)
	if err != nil {
		t.Fatal(err)
	}
	if gen != 3 {
		t.Errorf("加载后槽 2 的 key_gen 是 %d，期望 3", gen)
	}
	b, err := id.Binding(2)
	if err != nil {
		t.Fatalf("Binding: %v", err)
	}
	if b.KeyGen != 3 {
		t.Errorf("担保里的 key_gen 是 %d，与磁盘计数 %d 不一致", b.KeyGen, gen)
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

// TestRotateWGKeyBumpsOnlyThatSlot 是每槽独立计数的意义：换槽 2 的密钥，
// 其余七条隧道的担保仍是旧 gen、旧公钥，对端不必重握手。
func TestRotateWGKeyBumpsOnlyThatSlot(t *testing.T) {
	pki := newTestPKI(t)
	dir := filepath.Join(t.TempDir(), "id")
	id, err := LoadOrCreate(dir, "CPE-0001")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if err := id.SetEnrolled("t1", 7, pki.issueDevice(t, id.IdentityPublicKey(), "t1", 7), pki.caPEM); err != nil {
		t.Fatalf("SetEnrolled: %v", err)
	}
	const slot = 2
	before, err := id.Bindings()
	if err != nil {
		t.Fatalf("Bindings: %v", err)
	}
	oldPub, err := id.WGPublicKey(slot)
	if err != nil {
		t.Fatal(err)
	}
	// 轮换前把八个文件的原文都抄一份：判据是"磁盘上只有那一个文件变了"，
	// 光比内存里的公钥证明不了这一点。
	beforeFiles := map[string][]byte{}
	for _, name := range slotFiles() {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("读 %s: %v", name, err)
		}
		beforeFiles[name] = raw
	}

	if err := id.RotateWGKey(slot); err != nil {
		t.Fatalf("RotateWGKey: %v", err)
	}
	after, err := id.Bindings()
	if err != nil {
		t.Fatalf("轮换后 Bindings: %v", err)
	}
	newPub, err := id.WGPublicKey(slot)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(oldPub, newPub) {
		t.Error("轮换后 WG 公钥没变")
	}
	if after[slot].KeyGen != before[slot].KeyGen+1 {
		t.Errorf("槽 %d 的 key_gen %d -> %d，期望 +1", slot, before[slot].KeyGen, after[slot].KeyGen)
	}
	for other := range before {
		if other == slot {
			continue
		}
		if after[other] != before[other] {
			t.Errorf("只该换槽 %d，槽 %d 的绑定却变了（key_gen %d->%d）",
				slot, other, before[other].KeyGen, after[other].KeyGen)
		}
	}

	// key_gen 的作用在这里：旧绑定在签名层面依然"合法"（身份密钥没换），
	// 但它担保的公钥已经不是当前那把。对端靠 gen 单调递增把"轮换"和"重放"分开。
	rotatedPub, err := before[slot].Verify("t1", 7, id.IdentityPublicKey())
	if err != nil {
		t.Fatalf("旧绑定按旧 gen 仍应可验: %v", err)
	}
	if !bytes.Equal(rotatedPub, oldPub) || bytes.Equal(rotatedPub, newPub) {
		t.Error("旧绑定担保的公钥不是轮换前那把")
	}
	attested, err := after[slot].Verify("t1", 7, id.IdentityPublicKey())
	if err != nil {
		t.Fatalf("新绑定校验: %v", err)
	}
	if !bytes.Equal(attested, newPub) {
		t.Error("新绑定担保的公钥不是轮换后那把")
	}

	// 私钥与 state 都必须落盘：中途崩溃后 key_gen 不能倒退，
	// 否则新公钥会顶着旧 gen 出现，对端只会当成重放丢掉。
	reloaded, err := LoadOrCreate(dir, "CPE-0001")
	if err != nil {
		t.Fatalf("轮换后重新加载: %v", err)
	}
	gen, err := reloaded.KeyGen(slot)
	if err != nil {
		t.Fatal(err)
	}
	if gen != after[slot].KeyGen {
		t.Errorf("key_gen 没落盘：磁盘 %d，内存 %d", gen, after[slot].KeyGen)
	}
	rp, err := reloaded.WGPrivateKey(slot)
	if err != nil {
		t.Fatal(err)
	}
	ip, err := id.WGPrivateKey(slot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rp, ip) {
		t.Errorf("轮换后的新私钥没落盘：槽 %d", slot)
	}

	// 轮换只该重写那一个文件：其余七个文件的字节没动过，那七条隧道的对端因此
	// 不需要重装 peer。
	for other := range slotFiles() {
		name := wgKeyPath(other)
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("槽 %d: %v", other, err)
		}
		if other == slot {
			if bytes.Equal(raw, beforeFiles[name]) {
				t.Errorf("槽 %d 的私钥文件还是轮换前那份", other)
			}
			continue
		}
		if !bytes.Equal(raw, beforeFiles[name]) {
			t.Errorf("只该动槽 %d 的文件，槽 %d（%s）的私钥却被改写了", slot, other, name)
		}
	}
}

// TestStateJSONCarriesPerSlotGens 把 state.json 的字段形状钉住：控制器与旧 agent
// 都是按名字读这个文件的，字段一改，lab 里手工写的存档就读不出来了。
func TestStateJSONCarriesPerSlotGens(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "id")
	id, err := LoadOrCreate(dir, "CPE-0001")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	pki := newTestPKI(t)
	if err := id.SetEnrolled("t1", 7, pki.issueDevice(t, id.IdentityPublicKey(), "t1", 7), pki.caPEM); err != nil {
		t.Fatalf("SetEnrolled: %v", err)
	}
	if err := id.RotateWGKey(5); err != nil {
		t.Fatalf("RotateWGKey: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]json.RawMessage
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("state.json 不是合法 JSON: %v", err)
	}
	var gens []uint32
	if err := json.Unmarshal(onDisk["key_gens"], &gens); err != nil {
		t.Fatalf("state.json 里没有 key_gens 数组: %v（原文 %s）", err, raw)
	}
	if len(gens) != keybinding.WGKeySlots {
		t.Fatalf("key_gens 长度 %d，期望 %d", len(gens), keybinding.WGKeySlots)
	}
	if gens[5] != 1 || gens[0] != 0 {
		t.Errorf("key_gens 没体现出「只换了槽 5」: %+v", gens)
	}
	if _, ok := onDisk["key_gen"]; ok {
		t.Error("v2 的存档里不该再有单值 key_gen 字段")
	}
}
