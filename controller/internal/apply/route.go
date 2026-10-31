// The overlay half of a bundle: the three keys of the box's route.json that the
// controller owns.
//
// Why an overlay and not a whole file (docs/overlay-console-plan.md §1.2):
// sr-route reads ONE file and calls DisallowUnknownFields, and its remaining keys
// are things the controller does not know -- identity_dir, status_addr, hold_sec
// and tls_server_name. Those name a certificate directory and a listen address on
// this box, which the controller has no business inventing. The controller sends
// `tunnels`, `vrfs` and (from O6-c) `rrs`, and which keys appear is the
// instruction: sr-apply copies exactly those into the file on disk and touches
// nothing else.
//
// Two entry points, both needed:
//
//   - write to disk, so a box that restarts while the controller is unreachable
//     comes back with the route targets it was published (the PUT alone is memory
//     only -- that was the defect O5-b left open);
//   - push to the running sr-route, so a config change does not restart anybody
//     (the disk file alone would mean "restart sr-route to take effect", which
//     violates the judgement this whole stage is measured against).
package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// overlayRouteKeys are the only keys this version copies out of the 4th artifact.
// A future controller key would be dropped rather than copied: writing a key
// sr-route does not know would make it refuse to START, which is far worse than
// one table not being taken over -- and the log says which key was ignored.
//
// `rrs` is on this list from O6-c: the tenant now picks its reflectors in the
// console, so the controller does know that key. From C-c it is hot-updated too,
// via sr-route's `PUT /rrs` -- but that one is only half of a success: a new
// reflector has to establish before the old one is dropped, so the response may
// say "已落盘、已开始、还没换完", and sr-apply says exactly that.
var overlayRouteKeys = map[string]bool{"tunnels": true, "vrfs": true, "rrs": true}

// routePushTimeout bounds one hot-update request. sr-route answers these in
// milliseconds; a box whose sr-route is wedged must not hang the whole apply.
const routePushTimeout = 10 * time.Second

// applyRoute writes the overlay tables into the box's route.json and -- when
// `push` -- hands the same tables to the running sr-route. It returns a note for
// status.json when the disk and the live table disagree ("已落盘、未热更"); a note
// is never an error, because the file on disk is already what sr-route will read.
//
// push is true on `apply` and false on `boot`: at boot sr-route has not been
// started yet (the checklist launches it after apply), so the file IS the update
// and a failed connection would be a false alarm.
func (a *Applier) applyRoute(ctx context.Context, inst Installed, push bool) (string, error) {
	if inst.Route == "" {
		a.Logf("overlay: 这份配置包没有第 4 段，%s 未改动", a.RouteConf)
		return "", nil
	}
	if a.DryRun {
		a.Logf("would merge 第 4 段 into %s and push the live table", a.RouteConf)
		return "", nil
	}
	merged, ignored, err := overlayRouteFile(a.RouteConf, inst.Route)
	if err != nil {
		return "", err
	}
	for _, k := range ignored {
		a.Logf("overlay: 控制器多发了一个键 %q，这个版本的 sr-apply 不认识，已忽略（%s 没被改写）", k, a.RouteConf)
	}
	if err := writeFileSync(a.RouteConf, merged, 0o644); err != nil {
		return "", fmt.Errorf("写 %s: %w", a.RouteConf, err)
	}
	doc, err := decodeOverlay(inst.Route)
	if err != nil {
		return "", err
	}
	if !push {
		a.Logf("overlay: 已落盘 %s（开机重放不热更，sr-route 起来就是这张表）", a.RouteConf)
		return "", nil
	}
	base, ok := routeStatusBase(merged)
	if !ok {
		a.Logf("overlay: 已落盘 %s，但那份里没有 status_addr，无法热更（sr-route 下次启动才读到）", a.RouteConf)
		return fmt.Sprintf("已落盘 %s、未热更：盘上那份没有 status_addr", a.RouteConf), nil
	}
	note, err := a.pushOverlay(ctx, base, doc)
	if err != nil {
		return "", err
	}
	return note, nil
}

// decodeOverlay parses the 4th artifact into its raw keys. Raw because the values
// are forwarded verbatim: re-encoding them through Go structs would drop fields
// this binary does not model (and `interface` is one of them).
func decodeOverlay(route string) (map[string]json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(route), &doc); err != nil {
		return nil, fmt.Errorf("配置包第 4 段不是 JSON 对象: %w", err)
	}
	return doc, nil
}

// overlayRouteFile merges the artifact into whatever is already on disk.
func overlayRouteFile(path, route string) (out []byte, ignored []string, err error) {
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			// Refuse rather than guess: an invented identity_dir is worse than no
			// update at all -- it would name a certificate directory this box may not
			// use, and a first RR list could only come from a controller that knows
			// which reflectors this tenant picked.
			return nil, nil, fmt.Errorf("盘上没有 %s：控制器只叠加 tunnels/vrfs/rrs，"+
				"identity_dir/status_addr 是盒子自己的键，得先由运维写一份", path)
		}
		return nil, nil, fmt.Errorf("读 %s: %w", path, rerr)
	}
	var base map[string]json.RawMessage
	if err := json.Unmarshal(raw, &base); err != nil {
		return nil, nil, fmt.Errorf("%s 不是一个 JSON 对象（%v）：这一版需要盘上那份先合法", path, err)
	}
	doc, err := decodeOverlay(route)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range doc {
		if !overlayRouteKeys[k] {
			ignored = append(ignored, k)
			continue
		}
		base[k] = v
	}
	sort.Strings(ignored)
	out, err = json.MarshalIndent(base, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("合并 %s: %w", path, err)
	}
	return append(out, '\n'), ignored, nil
}

// routeStatusBase turns the status_addr in the file just written into a URL this
// process can dial. status_addr is a box key, so the artifact cannot carry it --
// the merged file is the only place anyone can read it from.
//
// Listening on 0.0.0.0 is not the same as being reachable at 0.0.0.0, so a
// wildcard host (or a bare port) becomes loopback. That is the same rule the boot
// checklist uses (deploy/sr-box/start.sh: route_status_url); a concrete address
// (127.0.0.1, a management-IP) is left exactly as the operator wrote it.
func routeStatusBase(merged []byte) (string, bool) {
	var box struct {
		StatusAddr string `json:"status_addr"`
	}
	if err := json.Unmarshal(merged, &box); err != nil {
		return "", false
	}
	a := strings.TrimSpace(box.StatusAddr)
	if a == "" {
		return "", false
	}
	if strings.HasPrefix(a, "http://") || strings.HasPrefix(a, "https://") {
		return strings.TrimRight(a, "/"), true
	}
	if _, port, wildcard := splitStatusAddr(a); wildcard {
		a = "127.0.0.1:" + port
	}
	return "http://" + a, true
}

// splitStatusAddr splits host:port and reports whether the host means "any".
// IPv6 is bracketed here, so the last colon is always the separator.
func splitStatusAddr(a string) (host, port string, wildcard bool) {
	i := strings.LastIndex(a, ":")
	if i < 0 {
		// 只写一个端口（"8084"）：没有主机部分，和 0.0.0.0 是一回事。
		return "", a, true
	}
	host, port = a[:i], a[i+1:]
	switch host {
	case "", "0.0.0.0", "::", "[::]", "[::1]", "::1":
		return host, port, true
	}
	return host, port, false
}

// pushOverlay PUTs the hot-updatable tables to the running sr-route. `rrs` joins
// them in C-c, but it is a different kind of answer: the switch is
// make-before-break, so sr-route returns as soon as the new session has been
// started and the reflector it replaces may still be standing. The note says
// which half of that the box is in, because "已热更" would be a lie while the new
// RR is unreachable.
func (a *Applier) pushOverlay(ctx context.Context, base string, doc map[string]json.RawMessage) (string, error) {
	// tunnels first: a VRF whose import RT already matches a held-back route is
	// installed by the tunnel table arriving, and doing it the other way round
	// would log one pointless "still pending" between the two calls.
	var missed []string
	var rrNote string
	for _, key := range []string{"tunnels", "vrfs"} {
		val, ok := doc[key]
		if !ok {
			continue
		}
		body, err := json.Marshal(map[string]json.RawMessage{key: val})
		if err != nil {
			return "", err
		}
		url := strings.TrimRight(base, "/") + "/" + key
		if _, err := a.putRouteTable(ctx, url, body); err != nil {
			var rej *routeRejected
			if errors.As(err, &rej) {
				// sr-route said "no" about this table: that is a real complaint, and
				// the box's config delivery should stop and show it.
				return "", fmt.Errorf("sr-route 拒绝 %s（已落盘 %s，所以磁盘那份是对的、活表没跟上）: %s",
					key, a.RouteConf, rej.body)
			}
			// Unreachable is NOT an apply failure: the file on disk is already the
			// new table, and sr-route reads it when it starts. Failing here would
			// freeze this box's delivery over one transient -- but it is said out
			// loud and recorded, never reported as done.
			a.Logf("overlay: 已落盘 %s、未热更（%s 连不上：%v）", a.RouteConf, url, err)
			missed = append(missed, key)
		} else {
			a.Logf("overlay: 已热更 %s", url)
		}
	}
	// rrs last: the new session is built from the tunnel table and the local
	// prefixes the VRF table makes speakable, so pushing it before those two would
	// hand the new reflector a snapshot of the old policy.
	if raw, ok := doc["rrs"]; ok {
		miss, note, err := a.pushRRs(ctx, base, raw)
		if err != nil {
			return "", err
		}
		if miss {
			missed = append(missed, "rrs")
		}
		if note != "" {
			rrNote = note
		}
	}
	if len(missed) == 0 && rrNote == "" {
		return "", nil
	}
	var notes []string
	if len(missed) > 0 {
		notes = append(notes, fmt.Sprintf("已落盘 %s、未热更：%s（sr-route 连不上，下次它启动就是这张表）",
			a.RouteConf, strings.Join(missed, "/")))
	}
	if rrNote != "" {
		notes = append(notes, rrNote)
	}
	return strings.Join(notes, "；"), nil
}

// pushRRs PUTs the reflector set and turns sr-route's answer into what this box
// has to report. `miss` means the usual "not pushed, disk is right, unreachable"
// case; `note` is the sentence for status.json when the switch is real but not
// finished (or when this sr-route has no seam at all).
func (a *Applier) pushRRs(ctx context.Context, base string, raw json.RawMessage) (miss bool, note string, err error) {
	url := strings.TrimRight(base, "/") + "/rrs"
	body, err := json.Marshal(map[string]json.RawMessage{"rrs": raw})
	if err != nil {
		return false, "", err
	}
	resp, err := a.putRouteTable(ctx, url, body)
	if err == nil {
		wait := rrWaitOf(resp)
		if wait == nil {
			a.Logf("overlay: 已热更 %s（旧会话已摘）", url)
			return false, "", nil
		}
		a.Logf("overlay: 换 RR 已开始，新 RR %v 还没成立，%v 先保留着", wait.PendingAddrs, wait.RetiringAddrs)
		return false, fmt.Sprintf("rrs 已开始热更：新 RR %s 还没成立，旧反射器 %s 先保留着（进度看设备 /status）",
			strings.Join(wait.PendingAddrs, "/"), strings.Join(wait.RetiringAddrs, "/")), nil
	}
	var rej *routeRejected
	if errors.As(err, &rej) {
		if rej.status == http.StatusNotFound || rej.status == http.StatusMethodNotAllowed {
			// An sr-route from before C-c: the file on disk is still the honest
			// half, and one box not upgraded yet must not fail the whole delivery.
			a.Logf("overlay: rrs 已落盘，这台 sr-route 没有换 RR 的入口（%s 返回 %d），要它下次启动才生效", url, rej.status)
			return false, "rrs 已落盘，这台 sr-route 没有换 RR 的入口，要它下次启动才生效", nil
		}
		return false, "", fmt.Errorf("sr-route 拒绝 rrs（已落盘 %s，所以磁盘那份是对的、活表没跟上）: %s", a.RouteConf, rej.body)
	}
	a.Logf("overlay: 已落盘 %s、未热更（%s 连不上：%v）", a.RouteConf, url, err)
	return true, "", nil
}

// rrWait is the shape sr-route reports for a switch that has not finished. It is
// re-declared here rather than imported from internal/route because what binds
// these two is this HTTP body, not a Go type.
type rrWait struct {
	PendingAddrs  []string `json:"pending_addrs"`
	RetiringAddrs []string `json:"retiring_addrs"`
}

type rrsResponse struct {
	RRWait *rrWait `json:"rr_wait"`
}

// rrWaitOf reads the one field of a PUT /rrs response that changes what this
// apply has to say: sr-route answered, but answered "还差一点". nil means the
// switch is finished (or this build says nothing about it), which is a clean
// 已热更.
func rrWaitOf(body []byte) *rrWait {
	var doc rrsResponse
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil
	}
	if doc.RRWait == nil || (len(doc.RRWait.PendingAddrs) == 0 && len(doc.RRWait.RetiringAddrs) == 0) {
		return nil
	}
	return doc.RRWait
}

// routeRejected is sr-route saying "no" -- a non-2xx, which is a real complaint
// about the table, not a box whose sr-route has not started yet. The status code
// is kept because one of them is not a complaint about the content: 404/405 means
// this build has no seam for the key at all.
type routeRejected struct {
	status int
	body   string
}

func (e *routeRejected) Error() string { return "sr-route 拒绝这份表: " + e.body }

// putRouteTable returns the response body: PUT /rrs answers with the state of a
// switch that is still in progress, and "2xx" alone is not enough to report it.
func (a *Applier) putRouteTable(ctx context.Context, url string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(io.LimitReader(resp.Body, 512))
		return nil, &routeRejected{status: resp.StatusCode, body: strings.TrimSpace(buf.String())}
	}
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return out, nil
}
