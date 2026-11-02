// TestLiveVRRPCapture reads a **real** vrrp_vr_details frame off the box and prints
// it as hex, so vrrp_test.go can pin parseVRRPDetails against VPP's own bytes
// instead of against the layout I assumed. Same reason wireguard_peers_dump has a
// real-frame fixture (vppapi_test.go): a hand-built frame and a hand-written
// decoder can be wrong together and the test still passes.
//
// It is also the vrrp half of the live suite: add, start, dump, read back, and
// delete again, all against the running VPP, with the cleanup in defers so a
// failed assertion does not leave a VR on the box.
//
// Runs only when both variables are set, and the interface must be an experiment
// port -- this test creates and deletes a VR on it:
//
//	docker exec lab-cpe1 bash -c 'cd /workspace/controller && PATH=/usr/local/go/bin:$PATH \
//	  SRWG_VPP_SOCK=/run/vpp/api.sock SRWG_VRRP_IFACE=tap99 \
//	  go test ./internal/vppapi -run TestLiveVRRPCapture -v'
//
// tap99 needs its address before the VR is useful (tools/vrrp-probe documents why):
//
//	vppctl create tap id 99; vppctl set interface state tap99 up
//	vppctl set interface ip address tap99 192.0.2.1/24
package vppapi

import (
	"context"
	"encoding/hex"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveVRRPCapture(t *testing.T) {
	sock := os.Getenv("SRWG_VPP_SOCK")
	iface := os.Getenv("SRWG_VRRP_IFACE")
	if sock == "" || iface == "" {
		t.Skip("未设 SRWG_VPP_SOCK / SRWG_VRRP_IFACE，跳过抓帧")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := Dial(ctx, sock, "vrrp-capture")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	ifs, err := c.DumpInterfaces(ctx)
	if err != nil {
		t.Fatalf("读接口清单: %v", err)
	}
	var idx uint32
	var name string
	for _, i := range ifs {
		if i.Name == iface {
			idx, name = i.SWIfIndex, i.Name
		}
	}
	if name == "" {
		t.Fatalf("%s 在 VPP 里不存在（先 create tap 并配地址，见文件头）", iface)
	}
	t.Logf("实验口 %s = sw_if_index #%d", name, idx)

	addrs := []netip.Addr{netip.MustParseAddr("192.0.2.250"), netip.MustParseAddr("192.0.2.251")}
	p := VRRPParams{
		SWIfIndex:  idx,
		VRID:       77,
		Priority:   120,
		IntervalCS: 100,
		Preempt:    true,
		Addrs:      addrs,
	}
	if err := c.VRRPAdd(ctx, p); err != nil {
		t.Fatalf("add: %v", err)
	}
	defer func() {
		if err := c.VRRPDelByKey(context.Background(), p); err != nil {
			t.Errorf("清理失败，VR（#%d vr77）还挂在 %s 上: %v", idx, iface, err)
		}
	}()
	if err := c.VRRPStartStop(ctx, idx, 77, false, true); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		if err := c.VRRPStartStop(context.Background(), idx, 77, false, false); err != nil {
			t.Errorf("stop 失败: %v", err)
		}
	}()

	// Wait past master_down (3*interval + skew): the point of the capture is a
	// frame whose timers, state and MAC are all non-zero, so a wrong offset cannot
	// hide behind a run of zero bytes.
	time.Sleep(4 * time.Second)

	frames, err := c.dump(ctx, "vrrp_vr_dump", "vrrp_vr_details", appendU32(nil, ^uint32(0)))
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	var ours *VRRPVR
	for _, f := range frames {
		vr, perr := parseVRRPDetails(f)
		if perr != nil {
			t.Errorf("这条帧解析失败（长度 %d）: %v", len(f), perr)
			continue
		}
		t.Logf("口 #%d vr%d prio=%d int=%dcs state=%s adv=%d skew=%d down=%d mac=%s track=%d/%d addrs=%v",
			vr.SWIfIndex, vr.VRID, vr.Priority, vr.IntervalCS, vr.State,
			vr.MasterAdvCS, vr.SkewCS, vr.MasterDownCS, vr.MasterMAC,
			vr.TrackIfDown, vr.TrackPriority, vr.Addrs)
		if vr.SWIfIndex != idx || vr.VRID != 77 {
			continue
		}
		for i := 0; i < len(f); i += 34 {
			end := i + 34
			if end > len(f) {
				end = len(f)
			}
			t.Logf("HEX %s", strings.ToLower(hex.EncodeToString(f[i:end])))
		}
		row := vr
		ours = &row
	}
	if ours == nil {
		t.Fatalf("dump(~0) 里没有刚加的那条 VR（%s=#%d vr77）", iface, idx)
	}
	// 下面每一条都是"结构变了要立刻失败"，不是打印出来靠人眼看：这一版 VPP 的
	// details 长度、conf 的五个字段、以及 VPP 自己算的三个定时器，是 vrrp_test.go
	// 那份夹具的全部依据。
	if len(ours.Addrs) != len(addrs) || ours.Addrs[0] != addrs[0] || ours.Addrs[1] != addrs[1] {
		t.Errorf("VIP 读回来 = %v，期望 %v", ours.Addrs, addrs)
	}
	if ours.Priority != 120 || ours.IntervalCS != 100 || !ours.Preempt || ours.Accept || ours.Unicast {
		t.Errorf("conf 读回来 = prio %d int %dcs preempt %v accept %v unicast %v",
			ours.Priority, ours.IntervalCS, ours.Preempt, ours.Accept, ours.Unicast)
	}
	if ours.MasterMAC.String() != "00:00:5e:00:01:4d" {
		t.Errorf("虚拟 MAC = %s，期望 00:00:5e:00:01:4d（RFC 5798 前缀 + vr_id 77）", ours.MasterMAC)
	}
	if ours.MasterAdvCS != 100 {
		t.Errorf("master_adv = %d，期望等于 interval（100cs）", ours.MasterAdvCS)
	}
	if skew := (256 - 120) * 100 / 256; ours.SkewCS != uint16(skew) {
		t.Errorf("skew = %d，期望 %d", ours.SkewCS, skew)
	}
	if down := 3*100 + int(ours.SkewCS); ours.MasterDownCS != uint16(down) {
		t.Errorf("master_down = %d，期望 3*interval+skew = %d", ours.MasterDownCS, down)
	}
	if ours.State == VRRPStateInit {
		t.Error("抓到的是 INIT 态：定时器和 MAC 全是 0，这条帧当夹具证明不了偏移")
	}

	// C7 的另一半：这一版的 dump 确实按口号过滤，所以问别的口不能把我们的行也交出来。
	// 客户端自己也会再滤一遍（vrrp_test.go），两边都要在。
	other, err := c.VRRPDump(ctx, idx+1000)
	if err != nil {
		t.Fatalf("dump(#%d): %v", idx+1000, err)
	}
	for _, v := range other {
		if v.SWIfIndex == idx && v.VRID == 77 {
			t.Errorf("问 #%d 却把 #7 的行也给了：%+v", idx+1000, v)
		}
	}
}
