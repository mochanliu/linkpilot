// Remote diagnostics, the controller-side half of wire.DiagCommand/DiagResult.
//
// The channel reuses the config-push shape: the console enqueues a command, the
// device picks it up on its next heartbeat (the controller never dials the box
// -- it is behind NAT), and the result comes back as its own mTLS POST. Every
// state transition lands in the 运维台账, so "who ran what on which box and what
// came out" is one query in the ledger, not a memory of who remembers.
//
// The whole store is in-memory on purpose. A pending command that outlives a
// controller restart describes a console nobody is watching anymore, and a
// result that old is a screenshot, not a diagnostic. The ledger keeps the
// durable audit trail; this map only serves the live page.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sdwan/controller/internal/wire"
)

const (
	diagPerNodeCap = 50 // recent records kept per device (pending + done)
	diagPendingCap = 8  // max undelivered commands queued per device
	diagOutCap     = 64 << 10
	// ledger truncation: the full output lives in the live map; the ledger
	// needs enough to prove what happened, not the whole ping dump.
	diagLedgerOutCap = 4 << 10
)

// diagRecord is one command's lifecycle: queued → dispatched → done (or left
// dispatched forever if the box died before answering -- the console shows that
// state honestly instead of inventing a timeout verdict).
type diagRecord struct {
	Cmd  *wire.DiagCommand `json:"cmd"`
	Res  *wire.DiagResult  `json:"result,omitempty"`
	Stat string            `json:"state"` // pending | dispatched | done
}

// diagShowWhitelist is the controller-side copy of the agent's executor
// whitelist. Duplicated deliberately: the two binaries upgrade on different
// days, so the controller must issue only what today's fleet can answer, and
// the agent re-checks anyway (its refusal is what a future drift surfaces as).
var diagShowWhitelist = []string{
	"show interface",
	"show ip fib",
	"show ip6 fib",
	"show ip tunnel",
	"show bfd session",
	"show acl-plugin acl",
	"show abf",
}

// diagIssueRequest is the console's POST /api/diag body.
type diagIssueRequest struct {
	NodeID uint16 `json:"node_id"`
	Tool   string `json:"tool"`
	Target string `json:"target,omitempty"`
	Count  int    `json:"count,omitempty"`
	Iface  string `json:"iface,omitempty"`
	VppShow string `json:"vpp_show,omitempty"`
}

func diagValidTarget(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_' || r == ':':
		default:
			return false
		}
	}
	return true
}

func diagValidIface(s string) bool {
	if len(s) == 0 || len(s) > 15 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == ':':
		default:
			return false
		}
	}
	return true
}

// diagIssueValidate is the issue-time check. It mirrors what the agent's
// executor accepts: refusing here means the console shows the mistake before a
// device ever sees it.
func diagIssueValidate(req *diagIssueRequest) error {
	switch req.Tool {
	case "ping":
		if !diagValidTarget(req.Target) {
			return fmt.Errorf("ping 目标不合法（只接受主机名/IP 字符）")
		}
		if req.Count < 0 || req.Count > 10 {
			return fmt.Errorf("count 取值 1..10（0 = 默认 4 次）")
		}
		if req.Iface != "" && !diagValidIface(req.Iface) {
			return fmt.Errorf("源接口名不合法（Linux 接口名规则，最长 15 字符）")
		}
	case "traceroute":
		if !diagValidTarget(req.Target) {
			return fmt.Errorf("traceroute 目标不合法（只接受主机名/IP 字符）")
		}
		if req.Iface != "" && !diagValidIface(req.Iface) {
			return fmt.Errorf("源接口名不合法（Linux 接口名规则，最长 15 字符）")
		}
	case "vpp-show":
		if !diagShowAllowed(req.VppShow) {
			return fmt.Errorf("vpp-show 只接受：%s", strings.Join(diagShowWhitelist, " / "))
		}
	default:
		return fmt.Errorf("tool 必须是 ping / traceroute / vpp-show")
	}
	return nil
}

func diagShowAllowed(cmd string) bool {
	cmd = strings.TrimSpace(strings.Join(strings.Fields(cmd), " "))
	if cmd == "" {
		return false
	}
	for _, w := range diagShowWhitelist {
		if cmd == w {
			return true
		}
	}
	return false
}

// diagCapLocked trims a node's record list to the cap, oldest first. Pending
// and dispatched records are never dropped by trimming -- only done ones; the
// pending QUEUE is bounded separately at issue time (diagPendingCap), which is
// the honest answer to a spamming console: refuse, don't quietly drop.
func diagCapLocked(recs []*diagRecord) []*diagRecord {
	if len(recs) <= diagPerNodeCap {
		return recs
	}
	droppable := 0
	for _, r := range recs {
		if r.Stat == "done" {
			droppable++
		}
	}
	need := len(recs) - diagPerNodeCap
	if need > droppable {
		need = droppable // never drop a pending/dispatched row
	}
	out := recs
	for i := 0; i < len(out) && need > 0; i++ {
		if out[i].Stat == "done" {
			out = append(out[:i], out[i+1:]...)
			i--
			need--
		}
	}
	return out
}

// diagPendingCountLocked counts undelivered commands for one device.
func diagPendingCountLocked(recs []*diagRecord) int {
	n := 0
	for _, r := range recs {
		if r.Stat == "pending" {
			n++
		}
	}
	return n
}

// diagIssueLocked enqueues one command. Caller has validated tenant/node.
func (s *server) diagIssueLocked(tenant string, nodeID uint16, req *diagIssueRequest, by string, now int64) *diagRecord {
	if s.diagRecs == nil {
		s.diagRecs = map[string][]*diagRecord{}
	}
	k := key(tenant, nodeID)
	s.diagSeq++
	cmd := &wire.DiagCommand{
		ID:       fmt.Sprintf("diag_%d", s.diagSeq),
		Tool:     req.Tool,
		Target:   strings.TrimSpace(req.Target),
		Count:    req.Count,
		Iface:    strings.TrimSpace(req.Iface),
		VppShow:  strings.TrimSpace(req.VppShow),
		IssuedBy: by,
		IssuedAt: now,
	}
	rec := &diagRecord{Cmd: cmd, Stat: "pending"}
	s.diagRecs[k] = append(s.diagRecs[k], rec)
	s.diagRecs[k] = diagCapLocked(s.diagRecs[k])
	return rec
}

// diagPopLocked hands out the oldest pending command for one device, marking it
// dispatched. Called from the heartbeat path: the beat is the downlink.
func (s *server) diagPopLocked(tenant string, nodeID uint16) *wire.DiagCommand {
	k := key(tenant, nodeID)
	for _, r := range s.diagRecs[k] {
		if r.Stat == "pending" {
			r.Stat = "dispatched"
			return r.Cmd
		}
	}
	return nil
}

// diagResultLocked folds a device's answer into the record. A result for an
// unknown id is stored as its own done row anyway: the console must be able to
// see a result that raced its record (e.g. a controller restart between issue
// and answer kept the output from vanishing).
func (s *server) diagResultLocked(tenant string, nodeID uint16, res *wire.DiagResult) *diagRecord {
	if res.Output != "" && len(res.Output) > diagOutCap {
		res.Output = res.Output[:diagOutCap]
	}
	if s.diagRecs == nil {
		s.diagRecs = map[string][]*diagRecord{}
	}
	k := key(tenant, nodeID)
	for _, r := range s.diagRecs[k] {
		if r.Cmd != nil && r.Cmd.ID == res.ID {
			r.Res = res
			r.Stat = "done"
			return r
		}
	}
	// The command's own row is gone (restart) -- keep the answer with a stub.
	rec := &diagRecord{
		Cmd:  &wire.DiagCommand{ID: res.ID, Tool: res.Tool, IssuedAt: res.Finished},
		Res:  res,
		Stat: "done",
	}
	s.diagRecs[k] = append(s.diagRecs[k], rec)
	s.diagRecs[k] = diagCapLocked(s.diagRecs[k])
	return rec
}

// handleAdminDiag serves the console's view: GET lists a device's records,
// POST issues a command. Tenant-only like every other device-config surface --
// the operator role has no business running tools inside a customer's box.
func (s *server) handleAdminDiag(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	if id.Role != roleTenant {
		http.Error(w, "tenant role required", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleDiagList(w, r, id)
	case http.MethodPost:
		s.handleDiagIssue(w, r, id)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) handleDiagList(w http.ResponseWriter, r *http.Request, id consoleIdentity) {
	n, err := parseNodeParam(w, r)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Nodes[key(id.TenantID, n)]; !ok {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	recs := s.diagRecs[key(id.TenantID, n)]
	if recs == nil {
		recs = []*diagRecord{}
	}
	writeJSON(w, map[string]any{"node_id": n, "records": recs})
}

func (s *server) handleDiagIssue(w http.ResponseWriter, r *http.Request, id consoleIdentity) {
	var req diagIssueRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.NodeID == 0 {
		http.Error(w, "node_id must be 1..65535", http.StatusBadRequest)
		return
	}
	if err := diagIssueValidate(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now().Unix()
	s.mu.Lock()
	n := s.state.Nodes[key(id.TenantID, req.NodeID)]
	if n == nil || n.TenantID != id.TenantID {
		s.mu.Unlock()
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	if diagPendingCountLocked(s.diagRecs[key(id.TenantID, req.NodeID)]) >= diagPendingCap {
		s.mu.Unlock()
		http.Error(w, "这台设备已有过多未执行的诊断命令（等它们下发/出结果，或稍后再发）", http.StatusTooManyRequests)
		return
	}
	rec := s.diagIssueLocked(id.TenantID, req.NodeID, &req, id.Username, now)
	s.mu.Unlock()

	// 台账：谁在这台盒子上发起了什么诊断。结果回执时再补一条带输出的。
	s.appendOps("diag.issue", id.TenantID, req.NodeID, id.Username, "info",
		fmt.Sprintf("远程诊断：%s %s（%s）", req.Tool, diagDesc(req), n.Serial))
	s.admin.publish("node.diag", id.TenantID, n)
	writeJSON(w, rec)
}

// diagDesc renders the command's target for one-line ledger messages.
func diagDesc(req diagIssueRequest) string {
	if req.Tool == "vpp-show" {
		return req.VppShow
	}
	d := req.Target
	if req.Iface != "" {
		d += " @ " + req.Iface
	}
	return d
}

// handleDiagResult is the device-facing POST /v1/diag. Identity comes from the
// client certificate, like every /v1 endpoint: a box may only report its own
// diagnostics.
func (s *server) handleDiagResult(w http.ResponseWriter, r *http.Request) {
	tenant, nodeID, err := peerTenantFromCert(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var res wire.DiagResult
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, diagOutCap+1<<10)).Decode(&res); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if res.NodeID != nodeID {
		http.Error(w, "node id does not match certificate", http.StatusForbidden)
		return
	}
	if res.ID == "" {
		http.Error(w, "result id required", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	if _, ok := s.state.Nodes[key(tenant, nodeID)]; !ok {
		s.mu.Unlock()
		http.Error(w, "unknown node", http.StatusNotFound)
		return
	}
	rec := s.diagResultLocked(tenant, nodeID, &res)
	s.mu.Unlock()

	// 台账：诊断结果。输出截断进 data，台账页能直接读前几行；完整输出只活在
	// 内存的页面上（见文件头）。
	msg := fmt.Sprintf("诊断 %s 完成（%s）", rec.Cmd.ID, diagDesc(diagIssueRequest{
		Tool: rec.Cmd.Tool, Target: rec.Cmd.Target, Iface: rec.Cmd.Iface, VppShow: rec.Cmd.VppShow,
	}))
	if !res.OK {
		msg += "：失败"
	}
	out := res.Output
	if len(out) > diagLedgerOutCap {
		out = out[:diagLedgerOutCap] + "…（截断）"
	}
	s.appendOpsData("diag.result", tenant, nodeID, statusSev(res.OK), msg, map[string]any{
		"tool": res.Tool, "ok": res.OK, "err": res.Err,
		"dur_ms": res.DurMS, "output": out,
	})
	writeJSON(w, map[string]any{"ok": true})
}

func statusSev(ok bool) string {
	if ok {
		return "info"
	}
	return "warn"
}

// parseNodeParam reads ?node= for the diag list endpoint.
func parseNodeParam(w http.ResponseWriter, r *http.Request) (uint16, error) {
	q := strings.TrimSpace(r.URL.Query().Get("node"))
	if q == "" {
		http.Error(w, "node parameter required", http.StatusBadRequest)
		return 0, fmt.Errorf("missing node")
	}
	n, err := strconv.ParseUint(q, 10, 16)
	if err != nil || n == 0 {
		http.Error(w, "node must be 1..65535", http.StatusBadRequest)
		return 0, fmt.Errorf("bad node")
	}
	return uint16(n), nil
}
