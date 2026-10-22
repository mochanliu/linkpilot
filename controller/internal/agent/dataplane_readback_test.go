package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"sdwan/controller/internal/vppapi"
)

// 这一组锁的是 **verifyLanded**：Open/adopt 结尾必须把 core 的接口表读回来对一遍。
//
// 三个判据每一个都对上一个真实故障形态，而不是"以防万一"：
//
//  1. 出口号在接口表里不存在。这个号是操作员/调用方给的（本层没有 FIB dump，算不出
//     默认出口），填错的可能性一直存在。以前只有一半概率被发现：给了 UnderlayMTU
//     时那次写 MTU 会回 retval=-2，没给时 EnableEncap 一路放行。
//  2. 出口号就是隧道接口自己。这是本项目最老的那种坏：EnableEncap 回成功、计数器
//     也动、overlay 小包也通，唯独一个 SR 头都不加（encap 挂在 WG 接口上不在
//     ip4-output 的 feature 链里，见 LinkConfig.EncapIfIndex）。
//  3. 记录不是我们写进去的那两个数。retval=0 只说明 VPP 收下了请求；同一台节点上
//     手敲配置的操作员可以在我们写完之后再改（"我手动敲配置也要能测"是明确需求），
//     而那两种后果都不轻：underlay 记大了 → 外层不分片、sendto 静默失败、这张口
//     从此只收不发；隧道记错了 → 内层按错的数切，大包黑洞。

// swIf 造一条假接口表项。测试用它把"VPP 眼下的记录"摆成任意形状。
func swIf(idx uint32, name string, l3MTU uint32) vppapi.SwInterface {
	return vppapi.SwInterface{
		SWIfIndex: idx, Name: name, L3MTU: l3MTU, AdminUp: true, LinkUp: true,
	}
}

// openFor 走一遍 NewLink → InstallKeys → Open，把 Open 的错误交回给调用方判。
func openFor(t *testing.T, mut func(*LinkConfig), tune func(*fakeVPP)) (*Link, error) {
	t.Helper()
	f := newFakeVPP()
	if tune != nil {
		tune(f)
	}
	cfg := testConfig(t, f, newTestPKI(t))
	if mut != nil {
		mut(&cfg)
	}
	l, err := NewLink(cfg)
	if err != nil {
		t.Fatalf("NewLink 在这些用例里不该报错: %v", err)
	}
	ctx := context.Background()
	if err := l.InstallKeys(ctx, []Key{{HmacID: 9001, Key: randomBytes(t, 16)}}); err != nil {
		t.Fatalf("InstallKeys: %v", err)
	}
	_, err = l.Open(ctx)
	return l, err
}

func TestOpenRejectsUnknownEgressIndex(t *testing.T) {
	// 假数据面的接口表里 underlay 出口是 5，这里给一个表里没有的号。
	l, err := openFor(t, func(c *LinkConfig) { c.EncapIfIndex = 77 }, nil)
	if err == nil {
		t.Fatal("出口号不存在却 Open 成功 —— 读回这一层白加了")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("错误要说清是出口号不存在，got: %v", err)
	}
	// 报错还不够：不能把自己标成 ready，否则 ApplyPeer 照样往下走。
	if l.ready {
		t.Error("verifyLanded 失败之后这条链路不该是 ready")
	}
}

func TestOpenRejectsEgressThatIsTheTunnelItself(t *testing.T) {
	// 假数据面给第一个新建接口分配的号是 1，把出口也填成 1：这正是"挂到 WG 接口上"
	// 那个静默坑，必须当场拦住。
	_, err := openFor(t, func(c *LinkConfig) { c.EncapIfIndex = 1 }, nil)
	if err == nil {
		t.Fatal("出口号等于隧道号却 Open 成功")
	}
	if !strings.Contains(err.Error(), "隧道接口自己") {
		t.Errorf("错误要说清是挂到了隧道自己身上，got: %v", err)
	}
}

func TestOpenRejectsStaleUnderlayRecord(t *testing.T) {
	// VPP 收下 1500 之后表里还是 9000（有人在我们写完又改回去）。记大了就是
	// sendto 那条路：外层不分片、内核静默失败、这张网卡从此只收不发。
	_, err := openFor(t,
		func(c *LinkConfig) { c.UnderlayMTU = 1500 },
		func(f *fakeVPP) {
			f.dumpIfs = []vppapi.SwInterface{swIf(1, "wg7", 1360), swIf(encapIfIndex, "host-eth0", bornVIFMTU)}
		})
	if err == nil {
		t.Fatal("underlay 记录没落地却报成功")
	}
	if !strings.Contains(err.Error(), "underlay") {
		t.Errorf("错误要点名 underlay 那条，got: %v", err)
	}
}

func TestOpenRejectsStaleTunnelRecord(t *testing.T) {
	// 隧道口记成了 1400，我们要 1360：内层分片按表里那个数走，于是外层随时可能
	// 撑破 underlay —— 这条同样不能放行。
	_, err := openFor(t, nil, func(f *fakeVPP) {
		f.dumpIfs = []vppapi.SwInterface{swIf(1, "wg7", 1400), swIf(encapIfIndex, "host-eth0", 1360)}
	})
	if err == nil {
		t.Fatal("隧道记录不符却报成功")
	}
	// 认的就是"记录不符"这条分支，不是"挂到隧道自己身上"那条 —— 两句都含"隧道接口"，
	// 所以这里要比"MTU 记录是"这个只属于前者的片段。
	if !strings.Contains(err.Error(), "MTU 记录是 1400") {
		t.Errorf("错误要点名隧道那条的记录值，got: %v", err)
	}
}

func TestOpenPropagatesInterfaceDumpFailure(t *testing.T) {
	// 读不回来就当没配好：retval 一路为 0 而表读不到时，"配成功了"是不可知的。
	_, err := openFor(t, nil, func(f *fakeVPP) {
		f.dumpErr = errors.New("假数据面：dump 超时")
	})
	if err == nil {
		t.Fatal("读回接口表失败必须让 Open 失败")
	}
	if !strings.Contains(err.Error(), "读回接口表") {
		t.Errorf("错误要说明是读回这一步，got: %v", err)
	}
}

func TestAdoptAlsoVerifies(t *testing.T) {
	// 认回接口那条路径（agent 重启、VPP 里接口还在）也必须核对：那时出口号还是
	// 操作员给的，MTU 记录更是上一世写的。
	l, err := openFor(t, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := l.dp.(*fakeVPP)
	// 第二次 Open 走 adopt：先把表换成"记录不对"的那份。
	f.dumpIfs = []vppapi.SwInterface{
		swIf(l.SWIfIndex(), "wg7", 1400),
		swIf(encapIfIndex, "host-eth0", bornVIFMTU),
	}
	if _, err := l.Open(context.Background()); err == nil {
		t.Fatal("认回接口时没核对记录，Open 照样成功")
	} else if !strings.Contains(err.Error(), "MTU 记录是 1400") {
		t.Errorf("认回路径上也要报出那条对不上的记录，got: %v", err)
	}
}
