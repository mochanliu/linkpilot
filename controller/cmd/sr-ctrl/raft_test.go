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
}

func newMemNet() *memNet {
	return &memNet{peers: map[string]*memPeer{}, down: map[string]bool{}}
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
		waitFor(t, p.id+" 的 voter 集合同步", 3*time.Second, func() bool {
			vv, _ := p.r.voterSet()
			return len(vv) == 3 && contains(vv, "c4") && !contains(vv, "c3")
		})
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
