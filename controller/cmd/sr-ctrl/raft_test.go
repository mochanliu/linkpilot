package main

// raft_test.go —— 共识内核的验收：选主、复制、多数派、重启、成员变更。
//
// 测试用内存传输（memNet）而不是真进程：它能在一条 goroutine 里精确地"把某台
// 机器拔掉"（down）或者在几百毫秒内完成一次换主，而不需要 3 个端口、3 个数据
// 目录和 30 秒的等待。生产用的 HTTP 传输只多一层序列化，安全性由同一份
// handleVote / handleAppend 决定 —— 那是被这里直接调用的代码。

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type memPeer struct {
	id   string
	mu   sync.Mutex
	st   *State
	r    *raftNode
	disk *raftDisk
}

type memNet struct {
	mu    sync.Mutex
	peers map[string]*memPeer
	down  map[string]bool
	// silent 是"单向丢包"：这台还发得出去，别人的 append 进不来。它模拟的是
	// lab 里那个真实场景 —— 进程刚起来、心跳还没到，它以为自己被孤立了。
	silent map[string]bool
}

func newMemNet() *memNet {
	return &memNet{peers: map[string]*memPeer{}, down: map[string]bool{}, silent: map[string]bool{}}
}

func (n *memNet) EndpointOf(id string) string { return id }

func (n *memNet) peer(id string) (*memPeer, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.down[id] {
		return nil, false
	}
	p, ok := n.peers[id]
	return p, ok
}

func (n *memNet) Call(ctx context.Context, target, path string, in, out any) error {
	p, ok := n.peer(target)
	if !ok {
		return fmt.Errorf("%s unreachable", target)
	}
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	switch path {
	case "/v1/cluster/vote":
		var req voteReq
		if err := json.Unmarshal(body, &req); err != nil {
			return err
		}
		b, _ := json.Marshal(p.r.handleVote(req))
		return json.Unmarshal(b, out)
	case "/v1/cluster/append":
		var req appendReq
		if err := json.Unmarshal(body, &req); err != nil {
			return err
		}
		if n.isSilent(target) {
			// 包到了门口被丢掉：调用方看到的是超时，而不是"你被开除了"。
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
				return fmt.Errorf("%s timed out", target)
			}
		}
		b, _ := json.Marshal(p.r.handleAppend(req))
		return json.Unmarshal(b, out)
	}
	return fmt.Errorf("unknown path %s", path)
}

func (n *memNet) Fetch(ctx context.Context, target string) (*raftSnapshot, error) {
	p, ok := n.peer(target)
	if !ok {
		return nil, fmt.Errorf("%s unreachable", target)
	}
	p.mu.Lock()
	b, _ := json.Marshal(p.st)
	members := []ClusterMember{}
	if p.st.Cluster != nil {
		members = p.st.Cluster.sorted()
	}
	p.mu.Unlock()
	snap := p.r.Snapshot(b, members)
	return &snap, nil
}

func (n *memNet) drop(id string, down bool) {
	n.mu.Lock()
	n.down[id] = down
	n.mu.Unlock()
}

func (n *memNet) setSilent(id string, on bool) {
	n.mu.Lock()
	n.silent[id] = on
	n.mu.Unlock()
}

func (n *memNet) isSilent(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.silent[id]
}

// joinTestCluster 起一个 n 节点的集群。所有节点从同一份成员表和同一个空状态
// 开始 —— 等价于"集群已经用一次配置项建好了，现在开始跑"。
func joinTestCluster(t *testing.T, n int) (*memNet, []*memPeer, func()) {
	t.Helper()
	dir := t.TempDir()
	net := newMemNet()
	var members []ClusterMember
	for i := 1; i <= n; i++ {
		members = append(members, ClusterMember{ID: fmt.Sprintf("c%d", i),
			Name: fmt.Sprintf("控制器 %d", i), Endpoint: fmt.Sprintf("10.0.0.%d:8443", i),
			Role: memberVoter, Status: memberActive, JoinedAt: int64(i)})
	}
	ctx, cancel := context.WithCancel(context.Background())
	var peers []*memPeer
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("c%d", i)
		st := &State{Nodes: map[string]*NodeRecord{}, NextNodeID: map[string]uint16{},
			Tenants: map[string]*Tenant{}, Claims: claimMap{}, Configs: map[string]*DeviceConfig{},
			Cluster: &ClusterConfig{ID: "cl-test", Generation: 1,
				Members: func() map[string]*ClusterMember {
					m := map[string]*ClusterMember{}
					for j := range members {
						cp := members[j]
						m[cp.ID] = &cp
					}
					return m
				}()}}
		disk, err := openRaftDisk(filepath.Join(dir, id))
		if err != nil {
			t.Fatalf("disk: %v", err)
		}
		p := &memPeer{id: id, st: st, disk: disk}
		r := newRaftNode(id, net, disk)
		r.cluster = "cl-test"
		r.voters = votersOf(members)
		r.applyRows = func(rows []HardRow) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			return applyRows(p.st, rows)
		}
		r.applyMembers = func(ms []ClusterMember) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			m := map[string]*ClusterMember{}
			for i := range ms {
				cp := ms[i]
				m[cp.ID] = &cp
			}
			p.st.Cluster.Generation++
			p.st.Cluster.Members = m
			return nil
		}
		r.applyState = func(b json.RawMessage) error {
			var st State
			if err := json.Unmarshal(b, &st); err != nil {
				return err
			}
			p.mu.Lock()
			p.st = &st
			p.mu.Unlock()
			return nil
		}
		p.r = r
		net.mu.Lock()
		net.peers[id] = p
		net.mu.Unlock()
		peers = append(peers, p)
		go r.run(ctx)
	}
	// 停的时候要等循环真的退出：只 cancel 的话它们还会读一遍时间常量，
	// 而 test cleanup 马上就要把常量改回去了（-race 会当场报错）。
	stopAll := func() {
		cancel()
		for _, p := range peers {
			p.r.Stop()
			p.r.Wait()
		}
	}
	return net, peers, stopAll
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等超时（%s）：%s", timeout, what)
}

func findLeader(peers []*memPeer) *memPeer {
	for _, p := range peers {
		if p.r.IsLeader() {
			return p
		}
	}
	return nil
}

// 缩短时间常量，让整个文件跑得快一点。生产值在 raft.go 里，这里只是测试夹具。
func fastRaft(t *testing.T) {
	t.Helper()
	oldH, oldMin, oldMax, oldCommit := raftHeartbeat, raftElectionMin, raftElectionMax, raftCommitTimeout
	raftHeartbeat = 20 * time.Millisecond
	raftElectionMin = 120 * time.Millisecond
	raftElectionMax = 240 * time.Millisecond
	raftCommitTimeout = 1500 * time.Millisecond
	t.Cleanup(func() {
		// 停下来的节点可能还有一条 in-flight 的广播（它们不在 runwg 里），
		// 那条广播会读 raftElection* —— 先放干净再改回常量，否则 -race 报的是
		// "夹具自己"的数据竞争，看上去像是内核有问题。
		time.Sleep(80 * time.Millisecond)
		raftHeartbeat, raftElectionMin, raftElectionMax, raftCommitTimeout = oldH, oldMin, oldMax, oldCommit
	})
}

func TestRaftElectsOneLeaderAndReplicates(t *testing.T) {
	fastRaft(t)
	_, peers, stop := joinTestCluster(t, 3)
	defer stop()

	waitFor(t, "选出 leader", 3*time.Second, func() bool { return findLeader(peers) != nil })
	// 一小段时间里 leader 必须只有一个：双主是这套东西最不能接受的失败。
	time.Sleep(300 * time.Millisecond)
	if n := countLeaders(peers); n != 1 {
		t.Fatalf("同时有 %d 个 leader", n)
	}
	lead := findLeader(peers)
	if err := lead.r.Propose([]HardRow{{Table: "Epoch", Data: json.RawMessage("42")}}); err != nil {
		t.Fatalf("提议失败: %v", err)
	}
	waitFor(t, "状态复制到全部节点", 3*time.Second, func() bool {
		for _, p := range peers {
			p.mu.Lock()
			ep := p.st.Epoch
			p.mu.Unlock()
			if ep != 42 {
				return false
			}
		}
		return true
	})
}

func countLeaders(peers []*memPeer) int {
	n := 0
	for _, p := range peers {
		if p.r.IsLeader() {
			n++
		}
	}
	return n
}

func TestRaftFailsOverAndKeepsCommitting(t *testing.T) {
	fastRaft(t)
	net, peers, stop := joinTestCluster(t, 3)
	defer stop()
	waitFor(t, "选出 leader", 3*time.Second, func() bool { return findLeader(peers) != nil })
	old := findLeader(peers)

	// 拔掉 leader：不发任何"优雅退出"，模拟 kill -9。
	net.drop(old.id, true)
	old.r.Stop()

	waitFor(t, "剩下的两台选出新 leader", 4*time.Second, func() bool {
		l := findLeader(peers)
		return l != nil && l.id != old.id
	})
	lead := findLeader(peers)
	if err := lead.r.Propose([]HardRow{{Table: "Epoch", Data: json.RawMessage("7")}}); err != nil {
		t.Fatalf("换主之后提议失败: %v", err)
	}
	// 活着的那两台必须有 7。
	for _, p := range peers {
		if p.id == old.id {
			continue
		}
		waitFor(t, p.id+" 收到新条目", 2*time.Second, func() bool {
			p.mu.Lock()
			defer p.mu.Unlock()
			return p.st.Epoch == 7
		})
	}
}

func TestRaftMinorityCannotCommit(t *testing.T) {
	fastRaft(t)
	net, peers, stop := joinTestCluster(t, 3)
	defer stop()
	waitFor(t, "选出 leader", 3*time.Second, func() bool { return findLeader(peers) != nil })
	lead := findLeader(peers)

	// 只留 leader 自己：多数派没了，写必须失败 —— 返回 200 就是"客户以为
	// 发布了，其实没落盘"。
	for _, p := range peers {
		if p.id != lead.id {
			net.drop(p.id, true)
			p.r.Stop()
		}
	}
	err := lead.r.Propose([]HardRow{{Table: "Epoch", Data: json.RawMessage("9")}})
	if err == nil {
		t.Fatal("没有多数派时提议竟然成功了")
	}
}

func TestRaftRestartRecoversCommittedState(t *testing.T) {
	fastRaft(t)
	_, peers, stop := joinTestCluster(t, 1)
	defer stop()
	waitFor(t, "单节点选出自己", 3*time.Second, func() bool { return findLeader(peers) != nil })
	p := peers[0]
	if err := p.r.Propose([]HardRow{{Table: "Epoch", Data: json.RawMessage("123")}}); err != nil {
		t.Fatalf("提议: %v", err)
	}
	waitFor(t, "条目已应用", 2*time.Second, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.st.Epoch == 123
	})
	commit, term := p.r.commit, p.r.term
	p.r.Stop()

	// 重启：同一个磁盘目录，重新 recover。
	r2 := newRaftNode(p.id, nil, p.disk)
	if err := r2.recover(); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if r2.commit < commit {
		t.Fatalf("提交点退回去了：%d < %d", r2.commit, commit)
	}
	if r2.term < term {
		t.Fatalf("term 退回去了：%d < %d", r2.term, term)
	}
	if r2.lastIndex() < commit {
		t.Fatalf("日志丢了：last=%d commit=%d", r2.lastIndex(), commit)
	}
}

func TestRaftMembershipAddPromoteRemove(t *testing.T) {
	fastRaft(t)
	net, peers, stop := joinTestCluster(t, 3)
	defer stop()
	waitFor(t, "选出 leader", 3*time.Second, func() bool { return findLeader(peers) != nil })
	lead := findLeader(peers)

	// 加第 4 台：先 learner（不参与多数派）。
	members := leadMembers(t, lead)
	members = append(members, ClusterMember{ID: "c4", Name: "控制器 4",
		Endpoint: "10.0.0.4:8443", Role: memberLearner, Status: memberActive, JoinedAt: 9})
	if err := lead.r.ProposeConfig(members); err != nil {
		t.Fatalf("加 learner: %v", err)
	}
	v, l := lead.r.voterSet()
	if len(v) != 3 || len(l) != 1 {
		t.Fatalf("加了 learner 之后 voters=%v learners=%v", v, l)
	}

	// 把第 4 台起起来（它的状态从 leader 的快照抄一份，等价于加入时的快照）。
	snap, err := net.Fetch(context.Background(), lead.id)
	if err != nil {
		t.Fatalf("fetch snapshot: %v", err)
	}
	p4 := &memPeer{id: "c4", st: &State{Nodes: map[string]*NodeRecord{}}}
	disk4, _ := openRaftDisk(filepath.Join(t.TempDir(), "c4"))
	r4 := newRaftNode("c4", net, disk4)
	r4.cluster = "cl-test"
	r4.applyRows = func(rows []HardRow) error {
		p4.mu.Lock()
		defer p4.mu.Unlock()
		return applyRows(p4.st, rows)
	}
	r4.applyMembers = func(ms []ClusterMember) error { return nil }
	r4.applyState = func(b json.RawMessage) error { return nil }
	p4.r = r4
	net.mu.Lock()
	net.peers["c4"] = p4
	net.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); r4.Stop(); r4.Wait() }()
	if err := r4.InstallSnapshot(snap, snap.State); err != nil {
		t.Fatalf("install: %v", err)
	}
	go r4.run(ctx)

	// 追上之后提升为 voter。
	waitFor(t, "learner 追上日志", 3*time.Second, func() bool {
		m, commit, _ := lead.r.matchOf("c4")
		return commit > 0 && m+1 >= commit
	})
	members = leadMembers(t, lead)
	for i := range members {
		if members[i].ID == "c4" {
			members[i].Role = memberVoter
		}
	}
	if err := lead.r.ProposeConfig(members); err != nil {
		t.Fatalf("提升 voter: %v", err)
	}
	v, l = lead.r.voterSet()
	if len(v) != 4 || len(l) != 0 {
		t.Fatalf("提升之后 voters=%v learners=%v", v, l)
	}

	// 下线一台。
	members = leadMembers(t, lead)
	var keep []ClusterMember
	for _, m := range members {
		if m.ID == "c3" {
			m.Status = memberDecommissioned
			m.Role = ""
		}
		keep = append(keep, m)
	}
	if err := lead.r.ProposeConfig(keep); err != nil {
		t.Fatalf("下线: %v", err)
	}
	v, _ = lead.r.voterSet()
	if len(v) != 3 || contains(v, "c3") {
		t.Fatalf("下线之后 voters=%v", v)
	}

	// 每一个 follower 也要知道同样的名单：只在 leader 上更新 voter 集合，
	// 那个刚被提升的节点就永远不参选 —— leader 一死，集群停在"没有 leader"。
	for _, p := range peers {
		if p.id == lead.id {
			continue
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			vv, _ := p.r.voterSet()
			if len(vv) == 3 && contains(vv, "c4") && !contains(vv, "c3") {
				break
			}
			if time.Now().After(deadline) {
				ll := p.r.learners
				st := p.r.Status()
				t.Fatalf("%s voters=%v learners=%v commit=%d applied=%d retired=%v role=%s",
					p.id, vv, ll, st.Commit, st.Applied, st.Retired, st.Role)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// 回归：把 leader 拔掉，剩下的必须能自己选出一个新的（含被提升过的那台）。
	// 注意候选集要带上 c4：它不在 joinTestCluster 返回的 peers 里。
	all := append(append([]*memPeer{}, peers...), p4)
	net.drop(lead.id, true)
	lead.r.Stop()
	waitFor(t, "换主（成员变更之后）", 5*time.Second, func() bool {
		l := findLeader(all)
		return l != nil && l.id != lead.id
	})
}

func leadMembers(t *testing.T, p *memPeer) []ClusterMember {
	t.Helper()
	// 生产里的成员变更会先等本机应用完再读表（applyMembershipChange 里的
	// waitCaughtUp），测试照做 —— 否则读到的是上一次变更的名单。
	if !p.r.waitCaughtUp(2 * time.Second) {
		t.Fatal("等本机应用完日志超时")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.st.Cluster == nil {
		t.Fatal("测试节点没有成员表")
	}
	return p.st.Cluster.sorted()
}

// ---------------------------------------------------------------- 预投票

// TestRaftRestartedNodeDoesNotStealLeadership 对应 lab 里的那个偏差：一台机器
// 重启后收不到任何心跳，选举超时一到就把 term 抬起来参选，把正在服务的主逼
// 下台（实测 term 13→14，约 2s 写空窗）。
//
// 预投票让它在"翻任期"之前就被拒掉：其余节点正在收心跳，会直接回答"不投"。
func TestRaftRestartedNodeDoesNotStealLeadership(t *testing.T) {
	fastRaft(t)
	net, peers, stop := joinTestCluster(t, 3)
	defer stop()
	waitFor(t, "选出 leader", 3*time.Second, func() bool { return findLeader(peers) != nil })
	lead := findLeader(peers)
	if err := lead.r.Propose([]HardRow{{Table: "Epoch", Data: json.RawMessage("1")}}); err != nil {
		t.Fatalf("提议失败: %v", err)
	}

	var f *memPeer
	for _, p := range peers {
		if p.id != lead.id {
			f = p
			break
		}
	}
	term0 := lead.r.Status().Term
	// 单向静默：它发得出去，心跳进不来 —— 正是"刚重启、还没接上"的那几秒。
	net.setSilent(f.id, true)
	defer net.setSilent(f.id, false)

	f.r.mu.Lock()
	f.r.electionAt = time.Now().Add(-time.Millisecond)
	f.r.mu.Unlock()

	time.Sleep(600 * time.Millisecond)
	if got := lead.r.Status().Term; got != term0 {
		t.Fatalf("重启的节点把 term 从 %d 抬到了 %d：预投票没拦住", term0, got)
	}
	if l := findLeader(peers); l != lead {
		t.Fatalf("leader 从 %s 换成了 %v：重启抢主", lead.id, l)
	}
}

// TestRaftPreVoteDoesNotBumpTerm 守住预投票的定义：它问完就走，不改任何状态。
// 改了就等于"预投票也翻任期"，正好毁掉它存在的意义。
func TestRaftPreVoteDoesNotBumpTerm(t *testing.T) {
	fastRaft(t)
	_, peers, stop := joinTestCluster(t, 3)
	defer stop()
	waitFor(t, "选出 leader", 3*time.Second, func() bool { return findLeader(peers) != nil })
	lead := findLeader(peers)
	var f *memPeer
	for _, p := range peers {
		if p.id != lead.id {
			f = p
			break
		}
	}
	before := f.r.Status().Term
	// 等它真的收到过一拍心跳再问：集群刚选出主的那一瞬间，follower 的
	// lastAppend 还是零值，"我刚跟 leader 说过话"这句话还不成立 —— 预投票
	// 会（正确地）答应，那不是我们要守的性质。
	waitFor(t, "follower 收到心跳", 2*time.Second, func() bool {
		f.r.mu.Lock()
		defer f.r.mu.Unlock()
		return time.Since(f.r.lastAppend) < 40*time.Millisecond
	})
	f.r.mu.Lock()
	li, lt := f.r.lastIndex(), f.r.lastTerm()
	f.r.mu.Unlock()
	resp := f.r.handleVote(voteReq{Term: before + 7, Candidate: "c99",
		LastIndex: li, LastTerm: lt, PreVote: true})
	if resp.Granted {
		t.Fatal("集群还活着，居然答应了预投票")
	}
	if after := f.r.Status().Term; after != before {
		t.Fatalf("预投票把 term 从 %d 改成了 %d", before, after)
	}
	if v := f.r.Leader(); v != lead.id {
		t.Fatalf("预投票把 leader 从 %s 改成了 %q", lead.id, v)
	}
}

// ---------------------------------------------------------------- 被下线

// TestRaftRemovedNodeLearnsItWasRemoved 验收"下线"是一个完整动作：
// 被摘掉的那台必须知道，而不是停在删除项之前的提交点上继续自称 voter。
func TestRaftRemovedNodeLearnsItWasRemoved(t *testing.T) {
	fastRaft(t)
	_, peers, stop := joinTestCluster(t, 3)
	defer stop()
	waitFor(t, "选出 leader", 3*time.Second, func() bool { return findLeader(peers) != nil })
	lead := findLeader(peers)
	var victim *memPeer
	for _, p := range peers {
		if p.id != lead.id {
			victim = p
			break
		}
	}

	retired := make(chan string, 4)
	victim.r.mu.Lock()
	victim.r.onRetire = func(reason string) {
		select {
		case retired <- reason:
		default:
		}
	}
	victim.r.mu.Unlock()

	members := leadMembers(t, lead)
	for i := range members {
		if members[i].ID == victim.id {
			members[i].Status = memberDecommissioned
			members[i].Role = ""
		}
	}
	if err := lead.r.ProposeConfig(members); err != nil {
		t.Fatalf("下线: %v", err)
	}

	select {
	case reason := <-retired:
		t.Logf("被下线的那台知道了：%s", reason)
	case <-time.After(3 * time.Second):
		t.Fatal("被下线的那台永远没学到自己被移出成员表（控制台会一直谎报 voter、设备继续往它心跳）")
	}

	// 它的成员表必须跟着变成 decommissioned：之前它停在删除项之前的提交点上。
	waitFor(t, "被下线的节点应用了移除记录", 2*time.Second, func() bool {
		victim.mu.Lock()
		defer victim.mu.Unlock()
		m := victim.st.Cluster.Members[victim.id]
		return m != nil && m.Status == memberDecommissioned
	})
	if !victim.r.Status().Retired {
		t.Fatal("raft 没把自己标成 retired")
	}

	// 它不该再参选：把选举闹钟拨到过去，term 也不能动。
	tv := victim.r.Status().Term
	victim.r.mu.Lock()
	victim.r.electionAt = time.Now().Add(-time.Second)
	victim.r.mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	if got := victim.r.Status().Term; got != tv {
		t.Fatalf("被下线的节点还在参选：term %d → %d", tv, got)
	}

	// leader 确认它收到之后就该放手，别再对着一个死人发日志。
	lead.r.mu.Lock()
	_, still := lead.r.retiring[victim.id]
	lead.r.mu.Unlock()
	if still {
		t.Fatal("对端已经确认，leader 还在给它补日志")
	}
}

// ---------------------------------------------------------------- 台账进共识

// TestRaftOpsReplicated 验收 P3：台账行经 raft 复制，三台机器的 onOps 都收到
// 同一批行、按同一顺序。台账不进 State、走独立载荷（raftEntry.Ops），
// apply 侧由 server 写进本机文件。
func TestRaftOpsReplicated(t *testing.T) {
	fastRaft(t)
	_, peers, stop := joinTestCluster(t, 3)
	defer stop()
	waitFor(t, "选出 leader", 3*time.Second, func() bool { return findLeader(peers) != nil })
	lead := findLeader(peers)

	got := map[string][]opsEntry{}
	var mu sync.Mutex
	for _, p := range peers {
		p.r.mu.Lock()
		p.r.onOps = func(ents []opsEntry) {
			mu.Lock()
			got[p.id] = append(got[p.id], ents...)
			mu.Unlock()
		}
		p.r.mu.Unlock()
	}
	batch := []opsEntry{
		{At: 1, Kind: "audit", Actor: "admin", Msg: "POST /api/config"},
		{At: 2, Kind: "node.online", Tenant: "t1", Node: 7, Msg: "设备上线"},
	}
	if err := lead.r.ProposeOps(batch); err != nil {
		t.Fatalf("提议台账失败: %v", err)
	}
	waitFor(t, "三台都应用了台账", 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range peers {
			if len(got[p.id]) < 2 {
				return false
			}
		}
		return true
	})
	mu.Lock()
	defer mu.Unlock()
	for _, p := range peers {
		if got[p.id][0].Kind != "audit" || got[p.id][1].Kind != "node.online" {
			t.Fatalf("%s 的台账顺序不对: %s, %s", p.id, got[p.id][0].Kind, got[p.id][1].Kind)
		}
	}
}

// ---------------------------------------------------------------- 活跃度搭车

// TestRaftLivenessPiggyback 验收"leader 把设备心跳时间带给 follower"：
// leader 的 sendAppend 带上 liveness 表，follower 收到后回调 onLive。
func TestRaftLivenessPiggyback(t *testing.T) {
	fastRaft(t)
	_, peers, stop := joinTestCluster(t, 3)
	defer stop()
	waitFor(t, "选出 leader", 3*time.Second, func() bool { return findLeader(peers) != nil })
	lead := findLeader(peers)

	lead.r.mu.Lock()
	lead.r.liveness = func() map[string]int64 {
		return map[string]int64{"t1|7": 1699999999}
	}
	lead.r.mu.Unlock()

	received := make(chan map[string]int64, 8)
	for _, p := range peers {
		if p.id == lead.id {
			continue
		}
		p.r.mu.Lock()
		p.r.onLive = func(live map[string]int64) {
			select {
			case received <- live:
			default:
			}
		}
		p.r.mu.Unlock()
	}
	lead.r.broadcastAppend()

	waitFor(t, "两台 follower 都收到活跃度表", 2*time.Second, func() bool {
		return len(received) >= 2
	})
	for i := 0; i < 2; i++ {
		live := <-received
		if live["t1|7"] != 1699999999 {
			t.Fatalf("活跃度表内容不对: %v", live)
		}
	}
}

// TestApplyLivenessMaxWins 守住并发语义：append 会乱序到达，旧时间戳不许把
// 新的顶回去。走真实的入队路径（enqueueLiveness → liveCh → liveApplier）。
func TestApplyLivenessMaxWins(t *testing.T) {
	s := &server{hbSec: 5, liveCh: make(chan map[string]int64, 16),
		state: &State{Nodes: map[string]*NodeRecord{
			"t1|7": {TenantID: "t1", NodeID: 7},
		}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.liveApplier(ctx)

	waitFor := func(t *testing.T, want int64) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			s.mu.Lock()
			got := s.state.Nodes["t1|7"].LastSeen
			s.mu.Unlock()
			if got == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("LastSeen 迟迟没变成 %d", want)
	}

	s.enqueueLiveness(map[string]int64{"t1|7": 100})
	waitFor(t, 100)
	s.enqueueLiveness(map[string]int64{"t1|7": 50}) // 乱序旧包，必须被丢弃
	time.Sleep(50 * time.Millisecond)
	s.mu.Lock()
	got := s.state.Nodes["t1|7"].LastSeen
	s.mu.Unlock()
	if got != 100 {
		t.Fatalf("旧时间戳把新的顶回去了: %d", got)
	}
	s.enqueueLiveness(map[string]int64{"t1|7": 200})
	waitFor(t, 200)
}
