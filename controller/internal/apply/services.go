// Bundle segment 8: the service plane (NAT egress / DHCP server / DNS forwarder).
//
// The split inside this segment:
//
//   - DHCP/DNS rows are fully resolved by the controller (port address, prefix
//     length, kernel TAP name), so the box only has to write them into
//     /etc/sdwan/svc.json and signal sr-svc -- sr-svc binds exactly what it is
//     told and re-derives nothing.
//   - NAT rows ride along in the artifact but are NOT applied here. 2026-11-12
//     exp-nat-bare 裁决：VPP 26.06 的 nat44_ei 数据面没有缺陷（裸 VPP + 纯 CLI
//     客户流量 8/8 通）；卡点是配置形状——pool 用外口自身地址会让所有入向管理
//     流量（ssh/控制器心跳）被 out2in "no translation" 丢掉，盒子表现为管理面
//     失联。下发前必须先落定：pool 改专用地址（外口 secondary）、外口改
//     nat44_ei_add_del_output_interface 注册、盒子自身服务要不要过 NAT 单独设计。
//     证据与机制账本：test/lab/vmlab/exp-nat-bare/README.md。
//
// The file write happens even when the segment carries empty tables: "删掉最后
// 一行 DHCP" has to stop sr-svc from serving, and the way that happens is the
// file saying so. sr-svc reloads on SIGHUP; at boot the checklist starts it
// after this replay, so a fresh boot needs no signal.
package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	// svcFile is where sr-svc reads its rows from. The apply keeps this path
	// stable: sr-svc's config flag points at it, and the checklist starts
	// sr-svc with it. servicesFile (the segment's file name in the version
	// dir) lives with the other artifact names in apply.go.
	svcFile = "/etc/sdwan/svc.json"
)

// NatPendingNote is the status note appended when a bundle carries NAT rows.
// The console shows the same text (cmd/sr-ctrl natPendingText); two sources of
// truth would drift, so the wording lives in both and says the same measured
// fact.
const NatPendingNote = "NAT 行已发布但暂不下发：实测 VPP 26.06 的 NAT44 本身可用（2026-11-12 exp-nat-bare），但产品配置形状未落定——pool 用外口自身地址会丢掉所有入向管理流量，需先改为专用 pool 地址 + 外口 output-interface 注册再下发。"

// svcDHCPRow / svcDNSRow mirror the controller's rendered rows (cmd/sr-ctrl
// config_render.go). The JSON shapes are the contract: the controller renders,
// this side decodes -- no re-derivation of addresses on the box.
type svcDHCPRow struct {
	VPN      uint32 `json:"vpn"`
	VPP      string `json:"vpp"`
	LCP      string `json:"lcp"`
	Listen   string `json:"listen"`
	Prefix   uint8  `json:"prefix"`
	Start    string `json:"start"`
	End      string `json:"end"`
	LeaseSec int    `json:"lease_sec"`
	Router   string `json:"router"`
	DNS      string `json:"dns"`
}

type svcDNSRow struct {
	VPN      uint32   `json:"vpn"`
	VPP      string   `json:"vpp"`
	LCP      string   `json:"lcp"`
	Listen   string   `json:"listen"`
	Prefix   uint8    `json:"prefix"`
	Upstream []string `json:"upstream"`
}

type svcNATRow struct {
	On       bool     `json:"on"`
	VPN      uint32   `json:"vpn"`
	Inside   string   `json:"inside"`
	Outside  string   `json:"outside"`
	Pool     []string `json:"pool"`
	MSSClamp uint16   `json:"mss_clamp,omitempty"`
}

type servicesSegment struct {
	NAT  []svcNATRow  `json:"nat"`
	DHCP []svcDHCPRow `json:"dhcp"`
	DNS  []svcDNSRow  `json:"dns"`
}

// srSvcConfig is what lands in svcFile: the DHCP/DNS half of the segment. NAT
// is deliberately absent -- sr-svc has no NAT surface, and a key nobody reads
// invites someone to start reading it.
type srSvcConfig struct {
	DHCP []svcDHCPRow `json:"dhcp"`
	DNS  []svcDNSRow  `json:"dns"`
}

func decodeServicesSegment(seg string) (servicesSegment, error) {
	var doc servicesSegment
	if strings.TrimSpace(seg) == "" {
		return doc, nil
	}
	if err := json.Unmarshal([]byte(seg), &doc); err != nil {
		return doc, fmt.Errorf("services 段不是合法 JSON: %w", err)
	}
	return doc, nil
}

// applyServices writes the svc file and reloads sr-svc. It returns a note when
// something travelled with the bundle that could not be applied (NAT rows).
func (a *Applier) applyServices(ctx context.Context, inst Installed) (string, error) {
	seg, err := decodeServicesSegment(inst.Services)
	if err != nil {
		return "", err
	}
	var note string
	if len(seg.NAT) > 0 {
		note = NatPendingNote
	}
	if a.DryRun {
		a.Logf("would write %s (dhcp %d 行, dns %d 行, nat %d 行[不下发])",
			a.SvcConf, len(seg.DHCP), len(seg.DNS), len(seg.NAT))
		return note, nil
	}
	svc := srSvcConfig{DHCP: seg.DHCP, DNS: seg.DNS}
	if svc.DHCP == nil {
		svc.DHCP = []svcDHCPRow{}
	}
	if svc.DNS == nil {
		svc.DNS = []svcDNSRow{}
	}
	data, err := json.MarshalIndent(svc, "", "  ")
	if err != nil {
		return note, err
	}
	data = append(data, '\n')
	if cur, rerr := os.ReadFile(a.SvcConf); rerr == nil && string(cur) == string(data) {
		// 文件没变就不用打扰 sr-svc：一次 apply 只该让它重载真正变化的配置。
		a.Logf("svc: %s 未变化，跳过重载", a.SvcConf)
		return note, nil
	}
	dir := filepath.Dir(a.SvcConf)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return note, err
	}
	tmp := a.SvcConf + ".tmp"
	if err := writeFileSync(tmp, data, 0o644); err != nil {
		return note, err
	}
	if err := os.Rename(tmp, a.SvcConf); err != nil {
		return note, err
	}
	a.Logf("svc: 写 %s（dhcp %d 行, dns %d 行）", a.SvcConf, len(svc.DHCP), len(svc.DNS))
	a.signalSvc()
	return note, nil
}

// signalSvc SIGHUPs the running sr-svc, if there is one. A running sr-svc that
// just lost its last row reloads to an empty server and stops answering; one
// that is not running simply gets started by the checklist later, which reads
// the same file.
func (a *Applier) signalSvc() {
	pid, ok := procSignal("sr-svc", syscall.SIGHUP)
	if !ok {
		a.Logf("svc: sr-svc 未在跑（开机清单会拿着新文件启动它）")
		return
	}
	a.Logf("svc: SIGHUP sr-svc (pid %d)", pid)
}

// procSignal sends sig to every /proc process whose comm equals name,
// reporting whether at least one was signalled. comm is exact --
// the same check start.sh's alive() uses -- so a stale pidfile or a recycled
// pid cannot cause a signal to an unrelated process.
func procSignal(name string, sig syscall.Signal) (int, bool) {
	dir, err := os.Open("/proc")
	if err != nil {
		return 0, false
	}
	defer dir.Close()
	entries, err := dir.Readdirnames(-1)
	if err != nil {
		return 0, false
	}
	sent := 0
	for _, e := range entries {
		pid, convErr := strconv.Atoi(e)
		if convErr != nil {
			continue
		}
		comm, rerr := os.ReadFile("/proc/" + e + "/comm")
		if rerr == nil && strings.TrimSpace(string(comm)) == name {
			if serr := syscall.Kill(pid, sig); serr == nil {
				sent++
			}
		}
	}
	return sent, sent > 0
}

// servicesNatRows reports whether the installed bundle carries NAT rows. The
// status page and the applier's log both key on it.
func (inst Installed) servicesNatRows() (int, error) {
	seg, err := decodeServicesSegment(inst.Services)
	if err != nil {
		return 0, err
	}
	return len(seg.NAT), nil
}

var errNoSvcSegment = errors.New("bundle has no services segment")
