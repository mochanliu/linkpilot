package main

// diag_http_test.go —— 远程诊断链路的回归面：控制台发起 → 心跳派发 → 设备
// 回执 → 页面可见 + 台账留痕。四步里任何一步断了，控制台的现象都是"没有
// 结果"，所以这里把每一步的状态（pending → dispatched → done）分别钉死。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sdwan/controller/internal/wire"
)

func newDiagServer(t *testing.T) (*server, http.Handler) {
	t.Helper()
	s := newTestServerWithOps(t)
	now := time.Now().Unix()
	s.state.Accounts["vm-tenant"] = &Account{Username: "vm-tenant", Role: roleTenant,
		TenantID: "tenant-vm", Password: "pbkdf2$sha256$x", CreatedAt: now}
	s.state.Nodes["tenant-vm|1"] = &NodeRecord{TenantID: "tenant-vm", NodeID: 1,
		Serial: "CPE1", LastSeen: now}
	h := s.adminHandler()
	// 预放一个租户会话（不走 PBKDF2，真实登录路径在 password_test.go）。
	s.admin.mu.Lock()
	s.admin.sess["smoke-sid"] = consoleIdentity{Role: roleTenant, TenantID: "tenant-vm",
		Username: "vm-tenant", expires: time.Now().Add(time.Hour)}
	s.admin.mu.Unlock()
	return s, h
}

func diagReq(t *testing.T, h http.Handler, method, target, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "smoke-sid"})
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return doReq(t, h, req)
}

func TestDiagIssueDispatchResult(t *testing.T) {
	s, h := newDiagServer(t)
	s.admin.mu.Lock()
	s.admin.sess["smoke-sid"] = consoleIdentity{Role: roleTenant, TenantID: "tenant-vm",
		Username: "vm-tenant", expires: time.Now().Add(time.Hour)}
	s.admin.mu.Unlock()

	// 1. 控制台发起 ping。
	code, body := diagReq(t, h, "POST", "/api/diag", `{"node_id":1,"tool":"ping","target":"172.16.3.1","count":3,"iface":"tap12"}`)
	if code != 200 {
		t.Fatalf("issue: %d %s", code, body)
	}
	var issued diagRecord
	json.Unmarshal(body, &issued)
	if issued.Stat != "pending" || issued.Cmd.ID == "" {
		t.Fatalf("应答应为 pending 且带 id: %s", body)
	}

	// 2. 心跳派发：命令随响应下来，状态翻 dispatched。
	s.mu.Lock()
	cmd := s.diagPopLocked("tenant-vm", 1)
	s.mu.Unlock()
	if cmd == nil || cmd.ID != issued.Cmd.ID || cmd.Target != "172.16.3.1" || cmd.Count != 3 || cmd.Iface != "tap12" {
		t.Fatalf("心跳应派发该命令: %+v", cmd)
	}
	s.mu.Lock()
	if s.diagRecs["tenant-vm|1"][0].Stat != "dispatched" {
		t.Fatalf("派发后应标记 dispatched: %+v", s.diagRecs["tenant-vm|1"][0].Stat)
	}
	s.mu.Unlock()
	// 再派发一次必须拿不到（pop-once）。
	s.mu.Lock()
	if again := s.diagPopLocked("tenant-vm", 1); again != nil {
		t.Fatalf("pop-once 被破坏，第二次还拿到了 %+v", again)
	}
	s.mu.Unlock()

	// 3. 设备回执。
	res := wire.DiagResult{NodeID: 1, ID: cmd.ID, Tool: "ping", OK: true,
		Output: "3 packets transmitted, 3 received", DurMS: 40, Finished: time.Now().Unix()}
	rr := httptest.NewRecorder()
	s.handleDiagResult(rr, peerReq(http.MethodPost, "/v1/diag", mJSON(res), "tenant-vm", 1))
	if rr.Code != 200 {
		t.Fatalf("result: %d %s", rr.Code, rr.Body.String())
	}

	// 4. 控制台读：记录 done，输出在页面上。
	code, body = diagReq(t, h, "GET", "/api/diag?node=1", "")
	if code != 200 {
		t.Fatalf("list: %d %s", code, body)
	}
	var list struct {
		Records []diagRecord `json:"records"`
	}
	json.Unmarshal(body, &list)
	if len(list.Records) != 1 || list.Records[0].Stat != "done" ||
		list.Records[0].Res == nil || !strings.Contains(list.Records[0].Res.Output, "3 received") {
		t.Fatalf("页面应看到 done + 输出: %s", body)
	}

	// 5. 台账：发起 + 结果各一条，结果行带输出。
	ents := s.ops.query(0, time.Now().Add(time.Minute).Unix(), nil, "tenant-vm", 1)
	kinds := map[string]bool{}
	for _, e := range ents {
		kinds[e.Kind] = true
		if e.Kind == "diag.result" && e.Data != nil {
			var m map[string]any
			if err := json.Unmarshal(e.Data, &m); err != nil {
				t.Fatalf("台账结果行 data 解不开: %v", err)
			}
			if m["output"] == nil || !strings.Contains(m["output"].(string), "3 received") {
				t.Fatalf("台账结果行缺输出: %s", e.Data)
			}
		}
	}
	if !kinds["diag.issue"] || !kinds["diag.result"] {
		t.Fatalf("台账缺 diag.issue/diag.result: %v", kinds)
	}
}

func mJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestDiagIssueValidation(t *testing.T) {
	_, h := newDiagServer(t)
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"node_id":1,"tool":"curl","target":"http://x"}`, 400},
		{`{"node_id":1,"tool":"ping","target":"a b; rm"}`, 400},
		{`{"node_id":1,"tool":"ping","target":"172.16.3.1","count":99}`, 400},
		{`{"node_id":1,"tool":"ping","target":"172.16.3.1","iface":"tap; x"}`, 400},
		{`{"node_id":1,"tool":"vpp-show","vpp_show":"show run"}`, 400},
		{`{"node_id":1,"tool":"vpp-show","vpp_show":"show ip fib"}`, 200},
		{`{"node_id":1,"tool":"traceroute","target":"172.16.3.1"}`, 200},
	} {
		code, body := diagReq(t, h, "POST", "/api/diag", tc.body)
		if code != tc.want {
			t.Fatalf("%s: 应 %d 得 %d (%s)", tc.body, tc.want, code, body)
		}
	}
}

// TestDiagHeartbeatBusy：心跳自报在忙时，队列里的命令必须按住不派发；下一拍
// 空闲了才给。派发的形状（Diag 字段、pop-once）在这条测试里一起钉死。
func TestDiagHeartbeatBusy(t *testing.T) {
	s, _ := newDiagServer(t)
	s.mu.Lock()
	s.diagIssueLocked("tenant-vm", 1, &diagIssueRequest{NodeID: 1, Tool: "ping", Target: "172.16.3.1"}, "vm-tenant", 1)
	s.mu.Unlock()

	beat := func(busy bool) wire.HeartbeatResponse {
		req := wire.HeartbeatRequest{NodeID: 1, DiagBusy: busy}
		rr := httptest.NewRecorder()
		s.handleHeartbeat(rr, peerReq(http.MethodPost, "/v1/heartbeat", mJSON(req), "tenant-vm", 1))
		if rr.Code != 200 {
			t.Fatalf("heartbeat: %d %s", rr.Code, rr.Body.String())
		}
		var resp wire.HeartbeatResponse
		json.Unmarshal(rr.Body.Bytes(), &resp)
		return resp
	}
	if resp := beat(true); resp.Diag != nil {
		t.Fatalf("忙时应按住命令: %+v", resp.Diag)
	}
	resp := beat(false)
	if resp.Diag == nil || resp.Diag.Tool != "ping" {
		t.Fatalf("空闲拍应派发命令: %+v", resp.Diag)
	}
	if resp2 := beat(false); resp2.Diag != nil {
		t.Fatalf("pop-once 被破坏，下一拍又派发了: %+v", resp2.Diag)
	}
}

func TestDiagCapKeepsPending(t *testing.T) {
	// 未执行命令的队列在"发起"一侧限流：超过 diagPendingCap 应 409 拒绝，
	// 而不是悄悄挤掉（悄悄丢命令的页面上看不出来）。
	s, h := newDiagServer(t)
	for i := 0; i < diagPendingCap; i++ {
		code, body := diagReq(t, h, "POST", "/api/diag", `{"node_id":1,"tool":"ping","target":"172.16.3.1"}`)
		if code != 200 {
			t.Fatalf("第 %d 条发起应成功: %d %s", i+1, code, body)
		}
	}
	code, body := diagReq(t, h, "POST", "/api/diag", `{"node_id":1,"tool":"ping","target":"172.16.3.1"}`)
	if code != 429 {
		t.Fatalf("超过 pending 上限应 429: %d %s", code, body)
	}
	// 全部派发后（盒子取走）就能再发。
	s.mu.Lock()
	for s.diagPopLocked("tenant-vm", 1) != nil {
	}
	s.mu.Unlock()
	code, _ = diagReq(t, h, "POST", "/api/diag", `{"node_id":1,"tool":"ping","target":"172.16.3.1"}`)
	if code != 200 {
		t.Fatalf("派发完应能再发: %d", code)
	}

	// done 行封顶：截断只动 done，不动 pending/dispatched。
	s.mu.Lock()
	for i := 0; i < diagPerNodeCap+5; i++ {
		s.diagIssueLocked("tenant-vm", 1, &diagIssueRequest{NodeID: 1, Tool: "ping", Target: "172.16.3.1"}, "u", 1)
	}
	recs := s.diagRecs["tenant-vm|1"]
	for _, r := range recs {
		r.Stat = "done"
	}
	recs = diagCapLocked(recs)
	if len(recs) != diagPerNodeCap {
		t.Fatalf("done 截断后应恰好 %d 条: %d", diagPerNodeCap, len(recs))
	}
	if recs[len(recs)-1].Stat != "done" {
		t.Fatal("最后一条应是最新 done 行")
	}
	// 混入 pending 后再截：pending 必须留下。
	recs = append(recs, &diagRecord{Cmd: &wire.DiagCommand{ID: "diag_p", Tool: "ping"}, Stat: "pending"})
	recs = diagCapLocked(recs)
	if len(recs) != diagPerNodeCap || recs[len(recs)-1].Stat != "pending" {
		t.Fatalf("pending 不该被截掉: %d 条，尾行 %s", len(recs), recs[len(recs)-1].Stat)
	}
	s.mu.Unlock()
}

func TestDiagResultUnknownIDKept(t *testing.T) {
	s, _ := newDiagServer(t)
	res := wire.DiagResult{NodeID: 1, ID: "diag_ghost", Tool: "ping", OK: false, Err: "超时"}
	rr := httptest.NewRecorder()
	s.handleDiagResult(rr, peerReq(http.MethodPost, "/v1/diag", mJSON(res), "tenant-vm", 1))
	if rr.Code != 200 {
		t.Fatalf("幽灵回执应保留: %d %s", rr.Code, rr.Body.String())
	}
	recs := s.diagRecs["tenant-vm|1"]
	if len(recs) != 1 || recs[0].Cmd.ID != "diag_ghost" || recs[0].Stat != "done" {
		t.Fatalf("幽灵回执应落成自己的行: %+v", recs)
	}
}

func TestDiagOtherNodeForbidden(t *testing.T) {
	s, _ := newDiagServer(t)
	res := wire.DiagResult{NodeID: 2, ID: "diag_x", Tool: "ping", OK: true}
	rr := httptest.NewRecorder()
	s.handleDiagResult(rr, peerReq(http.MethodPost, "/v1/diag", mJSON(res), "tenant-vm", 1))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("node_id 与证书不符应 403: %d", rr.Code)
	}
}
