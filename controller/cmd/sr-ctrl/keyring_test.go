// The controller's side of the keyring: authenticate before storing, per slot.
//
// These tests hold two rules the single-binding model never had to express.
//
// One: a bad attestation costs exactly its own slot. The old code returned on the
// first failure, which was harmless when there was one key; with eight it means
// "one stale key blacks out seven tunnels". A device that reports eight keys, one
// of which is bad, must end up with the other seven stored.
//
// Two: a keyring that is not the full eight is refused whole. A device reporting
// three keys is a device running an older build, and accepting its three would
// retire five slots that tunnels in the registry still point at -- those tunnels
// would then fail to deliver with "this slot has no binding", which reads like a
// dataplane fault and is really version skew.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"testing"

	"sdwan/controller/internal/keybinding"
)

// keyringFixture returns a signer (the device's identity key), a node record whose
// stored PublicKey is that key's other half, and nothing reported yet.
func keyringFixture(t *testing.T) (keybinding.Signer, *NodeRecord) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := keybinding.NewSoftwareSigner(der)
	if err != nil {
		t.Fatal(err)
	}
	return signer, &NodeRecord{
		TenantID:  "t",
		NodeID:    2,
		PublicKey: base64.StdEncoding.EncodeToString(pub),
	}
}

// fullRing signs a keyring the way an agent does: one entry per slot, in slot
// order, each attesting a distinct public key.
func fullRing(t *testing.T, signer keybinding.Signer, rec *NodeRecord, gen uint32) []keybinding.Binding {
	t.Helper()
	out := make([]keybinding.Binding, keybinding.WGKeySlots)
	for slot := range out {
		b, err := keybinding.Sign(signer, rec.TenantID, rec.NodeID, uint8(slot), gen, slotPub(slot))
		if err != nil {
			t.Fatalf("槽 %d 签署: %v", slot, err)
		}
		out[slot] = b
	}
	return out
}

// slotPub 是第 slot 把"公钥"：内容无所谓，但每把必须互不相同 —— 两把相同正是
// 同设备对两条隧道撞在数据面 peer 去重上的那件事。
func slotPub(slot int) []byte {
	pub := make([]byte, keybinding.X25519PubLen)
	pub[0] = byte(slot + 1)
	return pub
}

// applyKeyring 是对 s.applyBindings 的一层薄壳：那个方法要求在持锁状态下调用，
// 这里借一把真锁把前置条件做齐，而不是把它的逻辑复制一遍。名字不能叫 apply ——
// 这个包的测试文件里导入了 internal/apply 包，同名会把它的导入遮掉。
func applyKeyring(t *testing.T, rec *NodeRecord, ring []keybinding.Binding) (bool, error) {
	t.Helper()
	s := newTestServer()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyBindings(rec, ring)
}

func TestApplyBindingsStoresEverySlot(t *testing.T) {
	signer, rec := keyringFixture(t)

	changed, err := applyKeyring(t, rec, fullRing(t, signer, rec, 1))
	if err != nil {
		t.Fatalf("一份完整且合法的关键环被拒: %v", err)
	}
	if !changed {
		t.Error("第一次上报却说「没变化」：对端不会被告知这八把新公钥")
	}
	if len(rec.Bindings) != keybinding.WGKeySlots {
		t.Fatalf("存了 %d 格，期望 %d 格", len(rec.Bindings), keybinding.WGKeySlots)
	}
	for slot := range rec.Bindings {
		if rec.Bindings[slot] == nil {
			t.Fatalf("槽 %d 是空的", slot)
		}
		if rec.Bindings[slot].Slot != uint8(slot) {
			t.Errorf("槽 %d 存的是写着槽 %d 的担保", slot, rec.Bindings[slot].Slot)
		}
	}

	// 同一份再来一次：既不该说"变了"（否则每次心跳都要重推一遍对端），也不该丢东西。
	changed, err = applyKeyring(t, rec, fullRing(t, signer, rec, 1))
	if err != nil {
		t.Fatalf("重复上报被拒: %v", err)
	}
	if changed {
		t.Error("重复上报被当成变化")
	}
}

// 一把坏担保只砸那一格，其余七把照常入库。
func TestApplyBindingsKeepsGoodSlotsWhenOneIsBad(t *testing.T) {
	signer, rec := keyringFixture(t)
	ring := fullRing(t, signer, rec, 1)

	// 槽 5 换成"别人签的"：签名自洽，用的却是另一台设备（另一租户/编号也一样）的
	// 身份密钥，本机存的那把公钥对不上 —— 这正是控制器要当场挡住的伪造。
	otherSigner, other := keyringFixture(t)
	rogue, err := keybinding.Sign(otherSigner, other.TenantID, other.NodeID, 5, 1, slotPub(5))
	if err != nil {
		t.Fatal(err)
	}
	ring[5] = rogue

	changed, err := applyKeyring(t, rec, ring)
	if err == nil {
		t.Fatal("槽 5 的担保验不过，却没报任何错")
	}
	if !changed {
		t.Error("其余七把明明存了，changed 却是 false（对端因此不会被告知）")
	}
	for slot := range ring {
		if slot == 5 {
			if rec.Bindings[slot] != nil {
				t.Error("验不过的那格照样存了")
			}
			continue
		}
		if rec.Bindings[slot] == nil {
			t.Errorf("槽 %d 本该存上，却因为槽 5 坏了没存", slot)
		}
	}

	// 坏的那格补回来：只有槽 5 是新的，其余七格不该被重新算成"变化"。
	ring = fullRing(t, signer, rec, 1)
	changed, err = applyKeyring(t, rec, ring)
	if err != nil {
		t.Fatalf("补齐后被拒: %v", err)
	}
	if !changed || rec.Bindings[5] == nil {
		t.Errorf("补齐槽 5 后 changed=%v、该格是否为空=%v", changed, rec.Bindings[5] == nil)
	}
}

// 错位的一格：下标 3 的位置上装着写着槽 7 的担保。按位置存就会把"第 8 把"的公钥
// 记成"第 4 把"，于是第 4 条隧道拨的是第 8 把钥匙 —— 隧道在、没流量。
func TestApplyBindingsRefusesMisplacedSlot(t *testing.T) {
	signer, rec := keyringFixture(t)
	ring := fullRing(t, signer, rec, 1)
	ring[3].Slot = 7 // 签名仍自洽（签的时候就写着槽 7），但它站错了位置

	changed, err := applyKeyring(t, rec, ring)
	if err == nil {
		t.Fatal("错位的担保被接受了")
	}
	if rec.Bindings[3] != nil {
		t.Error("位置 3 存下了一份写着槽 7 的担保")
	}
	// 槽 7 自己那份（位置 7 上的）仍然是合法的，照常入库。
	if rec.Bindings[7] == nil {
		t.Error("槽 7 被位置 3 的错位牵连没存")
	}
	if !changed {
		t.Error("七格存上了却说「没变化」")
	}
}

// 长度不是八 = 设备在报一份过期的关键环（老版本 agent）。整份拒绝，一格都不动。
func TestApplyBindingsRefusesPartialKeyring(t *testing.T) {
	signer, rec := keyringFixture(t)
	full := fullRing(t, signer, rec, 1)

	for _, n := range []int{1, 3, keybinding.WGKeySlots - 1, keybinding.WGKeySlots + 1} {
		ring := append([]keybinding.Binding(nil), full...)
		if n < len(ring) {
			ring = ring[:n]
		} else {
			ring = append(ring, ring[0]) // 多出来的一份：同样不是八把
		}
		changed, err := applyKeyring(t, rec, ring)
		if err == nil {
			t.Errorf("%d 份的担保集被接受了", n)
		}
		if changed {
			t.Errorf("%d 份被拒的同时还改了存量", n)
		}
	}
	if rec.Bindings != nil {
		t.Error("整份拒绝的语义是「一格都不动」，可存量里出现了担保")
	}

	// 空 = 没上报，而不是"上报了一把都没有"：既不报错也不动存量。
	if changed, err := applyKeyring(t, rec, nil); err != nil || changed {
		t.Errorf("空担保集应该什么都不做: changed=%v err=%v", changed, err)
	}
}

// 设备身份密钥不可用（老 state.json 里那格是空的或不是 base64）时整份拒绝：
// 没有可信的公钥，八份担保就一份都算不出"是谁签的"。
func TestApplyBindingsRefusesUnusableDeviceKey(t *testing.T) {
	signer, rec := keyringFixture(t)
	for _, bad := range []string{"", "!!!", base64.StdEncoding.EncodeToString(make([]byte, 8))} {
		rec.PublicKey = bad
		changed, err := applyKeyring(t, rec, fullRing(t, signer, rec, 1))
		if err == nil {
			t.Errorf("设备公钥 %q 不可用却接受了担保", bad)
		}
		if changed || rec.Bindings != nil {
			t.Error("这种拒绝不许留下任何存量改动")
		}
	}
}

// 轮换与重放只有 key_gen 分得开，而计数是每槽一把一个：
// 槽 2 涨到 2 不该让槽 0 看起来也换了。
func TestApplyBindingsRotationAndReplayPerSlot(t *testing.T) {
	signer, rec := keyringFixture(t)
	if _, err := applyKeyring(t, rec, fullRing(t, signer, rec, 1)); err != nil {
		t.Fatalf("首次上报: %v", err)
	}

	// 只把槽 2 往前推：换成新公钥、gen 2，其余七格仍是 gen 1。
	rotated := fullRing(t, signer, rec, 1)
	fresh, err := keybinding.Sign(signer, rec.TenantID, rec.NodeID, 2, 2, []byte("012345678901234567890123456789ab"))
	if err != nil {
		t.Fatal(err)
	}
	rotated[2] = fresh
	changed, err := applyKeyring(t, rec, rotated)
	if err != nil {
		t.Fatalf("合法的轮换被拒: %v", err)
	}
	if !changed {
		t.Error("换了密钥却没报变化：对端会拿着旧公钥装不上")
	}
	if rec.Bindings[2].KeyGen != 2 {
		t.Errorf("槽 2 存的是 gen %d，期望 2", rec.Bindings[2].KeyGen)
	}
	if rec.Bindings[0].KeyGen != 1 {
		t.Errorf("槽 0 被牵连：gen %d，期望仍是 1", rec.Bindings[0].KeyGen)
	}

	// 槽 2 退回 gen 1：那是重放旧绑定，签名仍然合法，必须拒收且不改存量。
	before := rec.Bindings[2].X25519Pub
	changed, err = applyKeyring(t, rec, fullRing(t, signer, rec, 1))
	if err == nil {
		t.Fatal("gen 从 2 退回 1 被当成正常上报")
	}
	if rec.Bindings[2].X25519Pub != before {
		t.Error("被拒的重放改掉了存量担保")
	}
	if changed {
		t.Error("重放那一份里没有任何合法变化，changed 不该是 true")
	}
}

// storedBindings 是给北向读的那份：没上报过的格子是"缺席"，不是"一把空的担保"。
// 后者会声称"有把密钥什么都没盖住"，那是另一件假话。
func TestStoredBindingsSkipsEmptySlots(t *testing.T) {
	if got := storedBindings(nil); len(got) != 0 {
		t.Errorf("nil 关键环读出来 %d 条", len(got))
	}
	list := make([]*keybinding.Binding, keybinding.WGKeySlots)
	for _, slot := range []int{0, 5} {
		cp := keybinding.Binding{Slot: uint8(slot), X25519Pub: "abc"}
		list[slot] = &cp
	}
	got := storedBindings(list)
	if len(got) != 2 {
		t.Fatalf("只上报了两格，读出来 %d 条", len(got))
	}
	if got[0].Slot != 0 || got[1].Slot != 5 {
		t.Errorf("读出来的槽号 %d、%d 与存的位置不符", got[0].Slot, got[1].Slot)
	}
	if bindingAt(list, 1) != nil {
		t.Error("没上报过的槽 1 读出了一个担保")
	}
	if bindingAt(list, keybinding.WGKeySlots) != nil {
		t.Error("越界的槽号读出了东西")
	}
	if bindingAt(nil, 0) != nil {
		t.Error("空关键环里读出了担保")
	}
}

// 担保本身没问题，但它担保的是"别人家的 (tenant, node)"：控制器按本机记录里的
// 身份重算消息，签名的覆盖面就换了 —— 必须验不过。这一条钉住 applyBindings
// 用的是 rec 里的身份，而不是担保自带的字段。
func TestApplyBindingsRefusesOtherIdentity(t *testing.T) {
	signer, rec := keyringFixture(t)
	ring := fullRing(t, signer, rec, 1)
	// 把这份合法关键环塞进另一台设备的记录里：签名盖的是 node 2，记录说的是 node 3。
	rec.NodeID = 3
	changed, err := applyKeyring(t, rec, ring)
	if err == nil {
		t.Fatal("node 2 的关键环存进了 node 3 的记录")
	}
	if changed {
		t.Error("一份都没验过，changed 不该是 true")
	}
	if n := storedCount(rec); n != 0 {
		t.Errorf("这种拒绝不该存下任何担保，实际 %d 格", n)
	}
}

// storedCount 数出记录里真正存着的担保格数。不数 rec.Bindings 的长度：那份切片
// 在校验之前就会分配好，"八格全空"和"一格都没存"对读者是同一件事（北向都是
// 未上报、下发都取不到公钥）。
func storedCount(rec *NodeRecord) int {
	n := 0
	for _, b := range rec.Bindings {
		if b != nil {
			n++
		}
	}
	return n
}
