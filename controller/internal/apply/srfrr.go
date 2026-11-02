// The 5th artifact: the half of /etc/sr-frr.json the controller owns.
//
// Why an overlay and not a whole file (same reasoning as route.go, docs/o6e-leak-plan.md
// §4): status_url, zserv_socket, state_file and poll_sec name things on THIS box
// -- an sr-route address, a zebra socket, a state file -- which the controller
// has no business inventing. The controller sends `export_vrfs` and
// `import_rules`, and those two keys are the instruction.
//
// Two differences from the route overlay:
//
//   - The push is mandatory. §5.3: PUT first, verify the hash sr-frr reports,
//     and only then write the file. A PUT that fails fails the apply; there is
//     no "已落盘、未热更" note here, because sr-frr is the only process that
//     turns these keys into kernel routes and saying "done" while it still runs
//     the old table would be a lie.
//   - The old schema keys (vrfs/import/distance/metric) are deleted on merge.
//     This is the one-time migration away from the ZAPI/1250 exporter (§5.5); a
//     file still holding them is exactly what makes sr-frr run with both
//     directions empty.
package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"sdwan/controller/internal/frr"
)

const (
	defaultSrfrrConf = "/etc/sr-frr.json"
	defaultSrfrrSock = "/run/sr-frr/control.sock"

	srfrrPolicyPrefix = "redistribute kernel route-map SDWAN2CPE-vpn"
)

// srfrrKeys are the only keys this version copies out of the 5th artifact. A
// key sr-apply does not know is dropped and logged rather than written: the
// box-owned keys above must not be overwritten by a controller guess.
var srfrrKeys = map[string]bool{"export_vrfs": true, "import_rules": true}

// srfrrLegacyKeys are the retired schema (§5.5). Leaving one in the file makes
// sr-frr refuse a mixed config, so the merge deletes them.
var srfrrLegacyKeys = []string{"vrfs", "import", "distance", "metric"}

// srfrrSegment is the decoded 5th artifact.
type srfrrSegment struct {
	doc       map[string]json.RawMessage
	exportRaw []byte
	importRaw []byte
}

func decodeSrfrr(seg string) (srfrrSegment, error) {
	var out srfrrSegment
	if err := json.Unmarshal([]byte(seg), &out.doc); err != nil {
		return out, fmt.Errorf("配置包第 5 段不是 JSON 对象: %w", err)
	}
	exportRaw, ok := out.doc["export_vrfs"]
	if !ok {
		return out, fmt.Errorf("配置包第 5 段没有 export_vrfs（没有就发空数组）")
	}
	importRaw, ok := out.doc["import_rules"]
	if !ok {
		return out, fmt.Errorf("配置包第 5 段没有 import_rules（没有就发空数组）")
	}
	out.exportRaw = bytes.TrimSpace(exportRaw)
	out.importRaw = bytes.TrimSpace(importRaw)
	if len(out.exportRaw) == 0 || len(out.importRaw) == 0 ||
		string(out.exportRaw) == "null" || string(out.importRaw) == "null" {
		return out, fmt.Errorf("配置包第 5 段的 export_vrfs/import_rules 不能是空值（空数组表示没有）")
	}
	return out, nil
}

// applySrfrr merges the segment into the box's sr-frr.json and -- when `push`
// -- hands it to the running sr-frr. prev is the version installed before this
// one; its FRR artifact says which VPNs gained a redistribute instance and
// therefore need one delete+add of their existing K routes (§5.3 touch).
func (a *Applier) applySrfrr(ctx context.Context, inst, prev Installed, push bool) error {
	if inst.Srfrr == "" {
		a.Logf("sr-frr: 这份配置包没有第 5 段，%s 未改动", a.SrfrrConf)
		return nil
	}
	seg, err := decodeSrfrr(inst.Srfrr)
	if err != nil {
		return err
	}
	for k := range seg.doc {
		if !srfrrKeys[k] {
			a.Logf("sr-frr: 控制器多发了一个键 %q，这个版本的 sr-apply 不认识，已忽略（%s 没被改写）", k, a.SrfrrConf)
		}
	}
	base, err := readSrfrrFile(a.SrfrrConf)
	if err != nil {
		return err
	}
	if a.DryRun {
		a.Logf("would merge 第 5 段 into %s and push PUT /config", a.SrfrrConf)
		return nil
	}
	sock := a.SrfrrSock
	if sock == "" {
		sock = srfrrSocketOf(base)
	}
	if push {
		refresh := refreshVRFs(prev.FRR, inst.FRR, seg.exportRaw)
		if err := a.pushSrfrr(ctx, sock, seg, refresh); err != nil {
			return fmt.Errorf("sr-frr 热更失败（%s 未改动，活进程还是旧表）: %w", a.SrfrrConf, err)
		}
		if len(refresh) > 0 {
			a.Logf("sr-frr: 已热更 %s，并 touch vpn%v（协议集合变化，触发再分发重算）", sock, refresh)
		} else {
			a.Logf("sr-frr: 已热更 %s", sock)
		}
	}
	merged, err := mergeSrfrr(base, seg)
	if err != nil {
		return err
	}
	if err := writeFileSync(a.SrfrrConf, merged, 0o644); err != nil {
		return fmt.Errorf("写 %s: %w", a.SrfrrConf, err)
	}
	if !push {
		a.Logf("sr-frr: 已落盘 %s（开机重放不热更，sr-frr 起来就是这张表）", a.SrfrrConf)
	}
	return nil
}

func readSrfrrFile(path string) (map[string]json.RawMessage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("盘上没有 %s：控制器只叠加 export_vrfs/import_rules，"+
				"status_url/zserv_socket 是盒子自己的键，得先由运维写一份", path)
		}
		return nil, fmt.Errorf("读 %s: %w", path, err)
	}
	var base map[string]json.RawMessage
	if err := json.Unmarshal(raw, &base); err != nil {
		return nil, fmt.Errorf("%s 不是一个 JSON 对象（%v）：这一版需要盘上那份先合法", path, err)
	}
	return base, nil
}

func srfrrSocketOf(base map[string]json.RawMessage) string {
	var s string
	if raw, ok := base["control_socket"]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultSrfrrSock
	}
	return s
}

func mergeSrfrr(base map[string]json.RawMessage, seg srfrrSegment) ([]byte, error) {
	for k, v := range seg.doc {
		if srfrrKeys[k] {
			base[k] = v
		}
	}
	for _, k := range srfrrLegacyKeys {
		delete(base, k)
	}
	out, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("合并 sr-frr.json: %w", err)
	}
	return append(out, '\n'), nil
}

// pushSrfrr PUTs the two keys to sr-frr's control socket and refuses to accept
// a 2xx without the expected config hash: "it answered" is not "it applied".
func (a *Applier) pushSrfrr(ctx context.Context, sock string, seg srfrrSegment, refresh []uint32) error {
	if refresh == nil {
		refresh = []uint32{}
	}
	body, err := json.Marshal(map[string]any{
		"export_vrfs":  json.RawMessage(seg.exportRaw),
		"import_rules": json.RawMessage(seg.importRaw),
		"refresh_vrfs": refresh,
	})
	if err != nil {
		return err
	}
	hc := &http.Client{
		Timeout: routePushTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://unix/config", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("连 %s: %w", sock, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("PUT /config 返回 %s: %s", resp.Status, strings.TrimSpace(buf.String()))
	}
	var out struct {
		ConfigHash string `json:"config_hash"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return fmt.Errorf("PUT /config 的回执不是 JSON: %w", err)
	}
	want, err := frr.ConfigHash(seg.exportRaw, seg.importRaw)
	if err != nil {
		return err
	}
	if out.ConfigHash != want {
		return fmt.Errorf("sr-frr 应用的配置 hash 是 %q，下发的是 %q：拒绝落盘", out.ConfigHash, want)
	}
	return nil
}

// refreshVRFs returns the VPNs that were already exported and gained or lost a
// redistribute instance in this version. For those the K routes are already in
// the kernel, so zebra will not look at them again when `redistribute kernel`
// appears in the new instance -- a delete+add is the only way to make it
// recount (§5.3). A VPN exported for the first time is skipped: its routes are
// added after FRR is in place, and zebra sees those additions.
func refreshVRFs(prevFRR, nextFRR string, exportRaw []byte) []uint32 {
	var vrfs []uint32
	if err := json.Unmarshal(exportRaw, &vrfs); err != nil {
		return nil
	}
	old := exportProtocolSets(prevFRR)
	now := exportProtocolSets(nextFRR)
	var out []uint32
	for _, v := range vrfs {
		if old[v] == "" {
			continue
		}
		if old[v] != now[v] {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// exportProtocolSets maps a VPN to the set of FRR instances that carry its
// `redistribute kernel route-map SDWAN2CPE-vpn<N>` line ("bgp", "bgp+ospf").
// Empty output means FRR was not asked to redistribute for that VPN.
func exportProtocolSets(frrConf string) map[uint32]string {
	out := map[uint32]string{}
	proto := ""
	inBlock := false
	for _, line := range strings.Split(frrConf, "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			proto, inBlock = "", false
			switch {
			case strings.HasPrefix(line, "router bgp"):
				proto = "bgp"
			case strings.HasPrefix(line, "router ospf"):
				proto = "ospf"
			default:
				continue
			}
			inBlock = true
			continue
		}
		if !inBlock {
			continue
		}
		if i := strings.Index(line, srfrrPolicyPrefix); i >= 0 {
			n, err := strconv.Atoi(strings.TrimSpace(line[i+len(srfrrPolicyPrefix):]))
			if err != nil {
				continue
			}
			out[uint32(n)] = addProto(out[uint32(n)], proto)
		}
	}
	return out
}

func addProto(set, proto string) string {
	if proto == "" || strings.Contains(set, proto) {
		return set
	}
	if set == "" {
		return proto
	}
	// Only two instances matter; a stable order keeps old/new comparisons exact.
	if set == "bgp" && proto == "ospf" || set == "ospf" && proto == "bgp" {
		return "bgp+ospf"
	}
	return set + "+" + proto
}
