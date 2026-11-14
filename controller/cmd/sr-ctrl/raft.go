package main

// raft.go —— 控制器集群的共识核心。标准库实现，不引第三方 raft 库。
//
// 为什么自己写（docs/controller-ha-plan.md §3）：这个模块的规矩是"零第三方
// 依赖"，而 raft 真正需要的语义只有五件事 —— 选主、日志复制、多数派提交、
// 成员变更、快照。引一个库要背它的 API、它的存储格式和它的升级路径，收益
// 不成比例。
//
// 语义边界（写清楚，免得后面有人误用）：
//
//   - 只有 leader 接受写。写 = 一批硬状态行（HardRow）或一次成员变更。
//   - 提交 = 多数派已把这条日志 fsync 落盘。leader 收到多数派 ack 才回 200，
//     所以"点了发布"与"已提交"是同一件事（RPO=0）。
//   - follower 只读。它的状态可能落后几百毫秒，控制台刷新一次就追上了。
//   - 成员变更一次一个：先加 learner（不参与多数派），追上日志再提升为 voter，
//     最后才移除旧的。这是 raft 论文 §6 的安全做法，也是"替换控制器"能不
//     中断写的原因。
//
// 落盘：
//
//	cluster/meta.json   term / votedFor / commit / applied / 日志基点
//	cluster/log.jsonl   追加写、fsync 之后才 ack；冲突或压缩时整体重写
//	state.json          已应用的硬状态（follower 在 apply 之后写）
//
// 日志基点（base/baseTerm）的含义是"这条日志之前的状态已经吃进 state.json 了"。
// 所以不需要单独的 snapshot 文件：state.json 就是快照，快照传输只是把它寄给
// 落后的节点。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// 这些是 var 而不是 const，只为一件事：测试要把选举窗口缩到几百毫秒，
// 否则一个"换主"用例要跑三秒。生产值就是这里的初值，不要在别处改。
var (
	raftHeartbeat     = 250 * time.Millisecond
	raftElectionMin   = 1200 * time.Millisecond
	raftElectionMax   = 2400 * time.Millisecond
	raftCommitTimeout = 3 * time.Second
	// raftCompactAt 是日志长度上限。超过就压到 state.json 的当前点；落后的
	// follower 会收到"去拉快照"，而不是被无限积压的日志拖住。
	raftCompactAt = 512
)

type raftRole string

const (
	raftFollower  raftRole = "follower"
	raftCandidate raftRole = "candidate"
	raftLeader    raftRole = "leader"
)

// raftEntry 是一条日志。四种形态共用一张表：
//   - Rows 非空：硬状态的一批行（最常见）
//   - Members 非 nil：成员配置（voters/learners 的完整快照）
//   - Ops 非空：一批台账行（P3：审计/事件进共识，任一节点查得到全部历史）
//   - Noop：leader 上任时追加的空条目，用来把上一个任期的条目推成已提交
type raftEntry struct {
	Index   uint64          `json:"i"`
	Term    uint64          `json:"t"`
	Rows    []HardRow       `json:"rows,omitempty"`
	Members []ClusterMember `json:"m,omitempty"`
	Ops     []opsEntry      `json:"ops,omitempty"`
	Noop    bool            `json:"noop,omitempty"`
}

func (e raftEntry) isConfig() bool { return e.Members != nil }

// raftMeta 是每个节点自己的选举记忆。它必须比任何 RPC 应答先落盘，否则重启后
// 可能在同一任期里投两票 —— 那正是双主的来源。
type raftMeta struct {
	Term      uint64 `json:"term"`
	VotedFor  string `json:"voted_for,omitempty"`
	Commit    uint64 `json:"commit"`
	Applied   uint64 `json:"applied"`
	Base      uint64 `json:"base"`
	BaseTerm  uint64 `json:"base_term"`
	ClusterID string `json:"cluster_id,omitempty"`
}

type voteReq struct {
	Term      uint64 `json:"term"`
	Candidate string `json:"candidate"`
	LastIndex uint64 `json:"last_index"`
	LastTerm  uint64 `json:"last_term"`
	// PreVote 是"如果我现在真的参选，你会投我吗"：不翻任期、不记投票。
	// 它唯一的用途是拦住"刚重启的节点把 term 抬高、逼健康的主下台"（§9.6）。
	PreVote bool `json:"pre_vote,omitempty"`
}

type voteResp struct {
	Term    uint64 `json:"term"`
	Granted bool   `json:"granted"`
}

type appendReq struct {
	Term         uint64      `json:"term"`
	Leader       string      `json:"leader"`
	PrevIndex    uint64      `json:"prev_index"`
	PrevTerm     uint64      `json:"prev_term"`
	Entries      []raftEntry `json:"entries,omitempty"`
	LeaderCommit uint64      `json:"leader_commit"`
	// Live 是搭车消息：设备最近一次心跳的时间（键 tenant|node），**软状态、
	// 不进日志**。只有 leader 见得到设备心跳，而 follower 的控制台也要显示
	// "这台设备几分钟前还在线" —— 让 leader 在每一次复制/心跳里把这份表
	// 顺带给出去（Vault HA 的 standby 转发请求给 active 是同一件事的另一种
	// 走法；我们在复制热路径上白送，不占额外的 RPC）。缺失的键不动本地值，
	// 重复值按时间戳大的算：append 可能并发乱序到达，时间戳是绝对值，谁新信谁。
	Live map[string]int64 `json:"live,omitempty"`
}

type appendResp struct {
	Term       uint64 `json:"term"`
	OK         bool   `json:"ok"`
	MatchIndex uint64 `json:"match_index"`
	NeedSnap   bool   `json:"need_snapshot,omitempty"`
}

// raftSnapshot 是"从某个已提交点开始的整份硬状态"。新节点加入、落后太多的
// follower 追赶、以及换主后本地重同步，都走这一条。
type raftSnapshot struct {
	Term     uint64          `json:"term"`
	Index    uint64          `json:"index"`
	BaseTerm uint64          `json:"base_term"`
	Members  []ClusterMember `json:"members"`
	State    json.RawMessage `json:"state"`
}

// raftTransport 把网络摘出去：生产是 HTTPS，测试是内存队列（可以精确地
// 丢包、分区），不需要起真进程。
type raftTransport interface {
	Call(ctx context.Context, target, path string, in, out any) error
	Fetch(ctx context.Context, target string) (*raftSnapshot, error)
	// EndpointOf 把成员 id 映射成地址。
	EndpointOf(id string) string
}

type raftNode struct {
	mu   sync.Mutex
	id   string
	tr   raftTransport
	disk *raftDisk

	// 回调都短：applyRows 会拿 server 的锁，绝不能在持 raft.mu 时调用。
	applyRows    func([]HardRow) error
	applyMembers func([]ClusterMember) error
	applyState   func(json.RawMessage) error
	// onOps 把一批复制的台账行落进本机台账（apply goroutine 上调用）。
	onOps func([]opsEntry)
	// liveness 只有 leader 用：发起复制前取一份设备活跃度表搭车带出去；
	// onLive 只有 follower 用：收到 append 后把这份表并进本机软状态。
	// 两个都可为 nil（单测、单机模式）。
	liveness func() map[string]int64
	onLive   func(map[string]int64)

	role     raftRole
	term     uint64
	votedFor string
	leader   string
	commit   uint64
	applied  uint64

	log      []raftEntry
	base     uint64
	baseTerm uint64
	cluster  string

	voters   []string // 已提交的 voter 集合
	learners []string

	nextIndex  map[string]uint64
	matchIndex map[string]uint64
	lastHeard  map[string]time.Time
	lastQuorum time.Time

	electionAt time.Time
	lastBeat   time.Time
	// lastAppend 是最近一次收到任一条 append（含心跳）的时刻。预投票靠它判断
	// "我这台还能听见集群"，所以它必须在任何 append 上刷新，而不仅仅是 leader 侧。
	lastAppend time.Time

	// retiring 是"已经不在成员表里、但还没确认收到那条移除记录"的对端。
	// 不保留它的话，配置项一提交 leader 就把对端从复制名单里摘掉，于是那台
	// 永远学不到"你被下线了"（lab 实测 2026-11-14：它的提交点停在删除项之前，
	// 控制台还自称 voter，设备继续往它心跳）。
	retiring map[string]retireTarget
	onRetire func(reason string) // 本机被移出集群时回调一次（在 apply goroutine 上）
	retired  bool

	pendingCfg  []string // 未提交配置项的新 voter 集合
	pendMembers []ClusterMember
	pendingIdx  uint64

	waiters  map[uint64]chan error
	stopc    chan struct{}
	stop     sync.Once
	runwg    sync.WaitGroup
	wake     chan struct{}
	needSnap bool
	// ctx/cancel 是节点自己的生命周期，Stop() 会取消它：在途的复制、选举、
	// 拉快照都跟着结束。没有它，一个已经停下来的节点还能继续发好几个 RPC，
	// 而这些 RPC 的 goroutine 谁也等不到（测试里表现为"夹具撤了还在读常量"）。
	ctx    context.Context
	cancel context.CancelFunc
	// rpcs 是在途的节点间请求。Stop 会先取消 ctx 再等它们退出：不留一个
	// "已经停下来的节点还在发 append"的窗口。
	rpcs    sync.WaitGroup
	closing bool

	// 成员变更驱动器的进度：一次只跑一个，控制台靠它显示"同步中"。
	op *memberOp

	lastSnapshotFetch time.Time
}

// retireTarget 是一次"请对端确认自己被移出"的复制任务。Index 是那条移除记录
// 的下标：对端 matchIndex 到了这里，才说明它真的看过了，可以彻底放手。
type retireTarget struct {
	Index uint64
	Since time.Time
}

// memberOp 是一次成员变更的进度。它是 leader 的运行时状态，不进共识：
// 换主之后新 leader 会从"还有 learner 没提升"这个事实重新推导。
type memberOp struct {
	Kind      string `json:"kind"` // add | replace | remove | promote
	MemberID  string `json:"member_id"`
	ReplaceID string `json:"replace_id,omitempty"`
	Phase     string `json:"phase"`
	Err       string `json:"err,omitempty"`
	StartedAt int64  `json:"started_at_unix"`
}

// raftStatus 是给控制台看的一帧。软信息（落后多少、最后联系）只在这里出现。
type raftStatus struct {
	Role    string                `json:"role"`
	Leader  string                `json:"leader"`
	Term    uint64                `json:"term"`
	Commit  uint64                `json:"commit"`
	Applied uint64                `json:"applied"`
	Members map[string]memberPeer `json:"members"`
	Op      *memberOp             `json:"op,omitempty"`
	// Retired 表示本机已经不在成员表里了（被下线或被替换）。
	Retired bool `json:"retired,omitempty"`
}

type memberPeer struct {
	Match     uint64 `json:"match"`
	Next      uint64 `json:"next"`
	LastHeard int64  `json:"last_heard_unix"`
	Reachable bool   `json:"reachable"`
	Voter     bool   `json:"voter"`
}

func newRaftNode(id string, tr raftTransport, disk *raftDisk) *raftNode {
	return &raftNode{
		id: id, tr: tr, disk: disk,
		role:       raftFollower,
		nextIndex:  map[string]uint64{},
		matchIndex: map[string]uint64{},
		lastHeard:  map[string]time.Time{},
		retiring:   map[string]retireTarget{},
		waiters:    map[uint64]chan error{},
		stopc:      make(chan struct{}),
		wake:       make(chan struct{}, 1),
	}
}

func (r *raftNode) lastIndex() uint64 {
	if len(r.log) == 0 {
		return r.base
	}
	return r.log[len(r.log)-1].Index
}

func (r *raftNode) lastTerm() uint64 {
	if len(r.log) == 0 {
		return r.baseTerm
	}
	return r.log[len(r.log)-1].Term
}

func (r *raftNode) termAt(i uint64) (uint64, bool) {
	if i == r.base {
		return r.baseTerm, true
	}
	if i < r.base || i > r.lastIndex() {
		return 0, false
	}
	return r.log[i-r.base-1].Term, true
}

func (r *raftNode) entryAt(i uint64) (raftEntry, bool) {
	if i <= r.base || i > r.lastIndex() {
		return raftEntry{}, false
	}
	return r.log[i-r.base-1], true
}

// ------------------------------------------------ 持久化

type raftDisk struct {
	dir      string
	mu       sync.Mutex
	metaPath string
	logPath  string
}

func openRaftDisk(dir string) (*raftDisk, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &raftDisk{dir: dir, metaPath: filepath.Join(dir, "meta.json"),
		logPath: filepath.Join(dir, "log.jsonl")}, nil
}

func (d *raftDisk) loadMeta() (raftMeta, error) {
	var m raftMeta
	b, err := os.ReadFile(d.metaPath)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

func (d *raftDisk) saveMeta(m raftMeta) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return writeFileSync(d.metaPath, mustMarshal(m))
}

func (d *raftDisk) loadLog() ([]raftEntry, error) {
	f, err := os.Open(d.logPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []raftEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e raftEntry
		if err := json.Unmarshal(line, &e); err != nil {
			// 半行（掉电写了半个 JSON）只可能出现在文件尾；丢掉它比拒绝启动好，
			// 因为那条日志从来没被 ack 过。
			log.Printf("raft: 丢弃日志尾部损坏的记录: %v", err)
			break
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// appendLog 追加并 fsync。必须在 ack 之前完成 —— "我收到了"与"我记住了"
// 必须是同一件事，否则 leader 数到的多数派是假的。
func (d *raftDisk) appendLog(e raftEntry) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	f, err := os.OpenFile(d.logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	b := append(mustMarshal(e), '\n')
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (d *raftDisk) rewriteLog(entries []raftEntry) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var b []byte
	for _, e := range entries {
		b = append(b, mustMarshal(e)...)
		b = append(b, '\n')
	}
	return writeFileSync(d.logPath, b)
}

func writeFileSync(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// recover 从磁盘恢复。state.json 里已经是"已应用的硬状态"，日志只需从 base
// 之后接上：不重放已应用的部分，也不假装自己有更早的历史。
func (r *raftNode) recover() error {
	m, err := r.disk.loadMeta()
	if err != nil {
		return err
	}
	entries, err := r.disk.loadLog()
	if err != nil {
		return err
	}
	r.term, r.votedFor = m.Term, m.VotedFor
	r.commit, r.applied = m.Commit, m.Applied
	r.base, r.baseTerm, r.cluster = m.Base, m.BaseTerm, m.ClusterID
	for _, e := range entries {
		if e.Index > r.base {
			r.log = append(r.log, e)
		}
	}
	r.commit = clamp(r.commit, r.base, r.lastIndex())
	r.applied = clamp(r.applied, r.base, r.commit)
	return nil
}

func clamp(v, lo, hi uint64) uint64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ------------------------------------------------ 主循环

func (r *raftNode) run(parent context.Context) {
	// 先派生自己的 ctx，再起 applyLoop：闭包捕获的是变量，先起 goroutine 后赋值
	// 会让它读到另一个 ctx（-race 会当场报出来）。
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	r.runwg.Add(2)
	go func() { defer r.runwg.Done(); r.applyLoop(ctx) }()
	defer r.runwg.Done()
	t := time.NewTicker(40 * time.Millisecond)
	defer t.Stop()
	r.mu.Lock()
	r.ctx, r.cancel = ctx, cancel
	r.resetElection()
	r.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopc:
			return
		case <-t.C:
			r.tick()
		}
	}
}

func (r *raftNode) Stop() {
	r.stop.Do(func() {
		// 停下来的节点不能再自称 leader：控制台的集群页和测试里的 findLeader
		// 都按这个字段判断，留着一个"主"会让它们以为集群还有主。
		r.mu.Lock()
		if r.role == raftLeader {
			r.role, r.leader = raftFollower, ""
		}
		r.closing = true
		cancel := r.cancel
		r.mu.Unlock()
		if cancel != nil {
			// 在途的复制/选举立刻结束：它们不该在节点已经停下之后还发请求。
			cancel()
		}
		close(r.stopc)
		// 等它们真的退出（ctx 已取消，最多几十毫秒）。不等的话，一个已经
		// 停下的节点还能在 Stop 返回之后去敲别人的 handleAppend。
		r.rpcs.Wait()
	})
}

// spawnRPC 起一个节点间请求。只在这里起，Stop 才等得到它们。
func (r *raftNode) spawnRPC(fn func()) {
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return
	}
	r.rpcs.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.rpcs.Done()
		fn()
	}()
}

// Wait 等 run / applyLoop 真正退出。进程退出和测试里都有用：只关 stopc 不知道
// 对方跑到哪一步了，跟着就改常量（测试）或删数据目录就会踩到还在跑的循环。
func (r *raftNode) Wait() { r.runwg.Wait() }

func (r *raftNode) resetElection() {
	d := raftElectionMin + time.Duration(rand.Int63n(int64(raftElectionMax-raftElectionMin)))
	r.electionAt = time.Now().Add(d)
}

// rpcCtx 给节点间 RPC 一个归属：Stop 之后在途请求立刻取消，而不是继续跟一个
// 已经停下来的节点说话（那些 goroutine 谁也等不到）。
func (r *raftNode) rpcCtx(d time.Duration) (context.Context, context.CancelFunc) {
	r.mu.Lock()
	base := r.ctx
	r.mu.Unlock()
	if base == nil {
		base = context.Background()
	}
	return context.WithTimeout(base, d)
}

func (r *raftNode) tick() {
	r.mu.Lock()
	role := r.role
	if r.retired {
		// 已被移出成员表：不再参选、不再复制。进程稍后会自己退出（见 server.retireSelf）。
		r.mu.Unlock()
		return
	}
	now := time.Now()
	needBeat := role == raftLeader && now.Sub(r.lastBeat) >= raftHeartbeat
	stale := role == raftLeader && now.Sub(r.lastQuorum) > 3*raftElectionMax
	// learner 不参选：它的日志本来就该落后，让它当 leader 等于让一个刚装好的
	// 空节点接管全局状态。
	election := role != raftLeader && now.After(r.electionAt) && contains(r.voters, r.id)
	leader := r.leader
	r.mu.Unlock()

	switch {
	case stale:
		// 拿不到多数派的 leader 必须先下台：它继续自认为主，控制台的写请求
		// 就会一直等到超时，而别的节点其实已经选出了新主。
		log.Printf("raft: %s 超过 %s 没有拿到多数派，主动下台", r.id, 3*raftElectionMax)
		r.stepDown()
	case election:
		r.startElection()
	case needBeat:
		r.broadcastAppend()
	}
	r.maybeFetchSnapshot(leader)
	r.maybeCompact()
}

func (r *raftNode) stepDown() {
	r.mu.Lock()
	r.role = raftFollower
	r.leader = ""
	r.pendingCfg, r.pendMembers, r.pendingIdx = nil, nil, 0
	r.resetElection()
	r.mu.Unlock()
}

// preVote 问一圈"如果我现在参选，你会投我吗"。它不改任何状态：不写 meta、
// 不记 votedFor、不动 term —— 这正是它和真选举的区别，也是它能拦住"重启抢主"
// 的原因：失败的一方连一次 term 都没花出去。
func (r *raftNode) preVote(term, lastIdx, lastTerm uint64, voters []string) bool {
	r.mu.Lock()
	role := r.role
	r.mu.Unlock()
	if role == raftLeader {
		// 已经自认为是主：不需要问，也不该问（问出来的只会是对自己的否定）。
		return true
	}
	if !contains(voters, r.id) {
		return false
	}
	majority := len(voters)/2 + 1
	if majority <= 1 {
		return true
	}
	ch := make(chan bool, len(voters))
	for _, p := range voters {
		if p == r.id {
			continue
		}
		peer := p
		r.spawnRPC(func() {
			ctx, cancel := r.rpcCtx(raftHeartbeat * 3)
			defer cancel()
			var out voteResp
			err := r.tr.Call(ctx, r.tr.EndpointOf(peer), "/v1/cluster/vote",
				voteReq{Term: term, Candidate: r.id, LastIndex: lastIdx, LastTerm: lastTerm,
					PreVote: true}, &out)
			ch <- err == nil && out.Granted
		})
	}
	votes := 1
	// 预算比正式选举短：预投票只是个探路，慢了不如让下一轮重新来。
	deadline := time.After(raftElectionMin)
	for pending := len(voters) - 1; pending > 0; pending-- {
		select {
		case ok := <-ch:
			if ok {
				votes++
			}
		case <-deadline:
			return false
		case <-r.stopc:
			return false
		}
		if votes >= majority {
			return true
		}
	}
	return false
}

func (r *raftNode) startElection() {
	r.mu.Lock()
	// 先把闹钟推后：预投票没通过也要等下一轮。不推的话 tick 每 40ms 就会再来
	// 一次，一台被隔离的机器会变成投票机关枪。
	r.resetElection()
	baseTerm, lastIdx, lastTerm := r.term, r.lastIndex(), r.lastTerm()
	voters := append([]string(nil), r.voters...)
	r.mu.Unlock()

	// 预投票（raft §9.6）：先问一圈"你会投我吗"，没人答应就原地不动。
	//
	// 这条路径存在的唯一原因是一个实测过的偏差：一台机器重启后不认识任何人
	// （lastHeard 是内存里的），选举超时一到就 term++ 参选，把正在服务的主
	// 逼下台（lab 实测 term 13→14，约 2s 写空窗）。现在它连 term 都翻不了：
	// 其余节点正在收心跳，会直接拒绝这份"你会投我吗"。
	if !r.preVote(baseTerm+1, lastIdx, lastTerm, voters) {
		return
	}

	r.mu.Lock()
	// 预投票这几百毫秒里情况可能变了：收到心跳、别人已经当选、或者自己成了主。
	// 这时候再翻任期就是白白制造一次换主。
	if r.term != baseTerm || r.role == raftLeader {
		r.mu.Unlock()
		return
	}
	r.role = raftCandidate
	r.term++
	r.votedFor = r.id
	r.leader = ""
	term := r.term
	r.resetElection()
	meta := r.metaLocked()
	r.mu.Unlock()
	if err := r.disk.saveMeta(meta); err != nil {
		log.Printf("raft: 保存选举状态失败: %v", err)
	}

	votes := 0
	if contains(voters, r.id) {
		votes = 1
	}
	majority := len(voters)/2 + 1
	log.Printf("raft: %s 发起选举 term=%d voters=%v", r.id, term, voters)
	if votes >= majority {
		r.becomeLeader(term)
		return
	}
	ch := make(chan bool, len(voters))
	for _, p := range voters {
		if p == r.id {
			continue
		}
		peer := p
		r.spawnRPC(func() {
			ctx, cancel := r.rpcCtx(raftHeartbeat * 3)
			defer cancel()
			var out voteResp
			err := r.tr.Call(ctx, r.tr.EndpointOf(peer), "/v1/cluster/vote",
				voteReq{Term: term, Candidate: r.id, LastIndex: lastIdx, LastTerm: lastTerm}, &out)
			ch <- err == nil && out.Granted
		})
	}
	deadline := time.After(raftElectionMax)
	for pending := len(voters) - 1; pending > 0; pending-- {
		select {
		case ok := <-ch:
			if ok {
				votes++
			}
		case <-deadline:
			pending = 0
		case <-r.stopc:
			return
		}
		if votes >= majority {
			r.mu.Lock()
			stillMine := r.term == term && r.role == raftCandidate
			r.mu.Unlock()
			if stillMine {
				r.becomeLeader(term)
			}
			return
		}
	}
}

func (r *raftNode) becomeLeader(term uint64) {
	r.mu.Lock()
	if r.term != term {
		r.mu.Unlock()
		return
	}
	r.role = raftLeader
	r.leader = r.id
	r.lastQuorum = time.Now()
	r.lastAppend = time.Now()
	for _, p := range append(append([]string{}, r.voters...), r.learners...) {
		r.nextIndex[p] = r.lastIndex() + 1
		if p == r.id {
			r.matchIndex[p] = r.lastIndex()
		} else {
			r.matchIndex[p] = 0
		}
	}
	r.mu.Unlock()
	// 上任先追一条空条目：只有本任期的条目能"顺带"把旧条目提交掉，这是
	// raft 的提交规则（§5.4.2）。没有它，上个任期最后一条永远提交不了。
	r.proposeEntry(raftEntry{Term: term, Noop: true})
	r.broadcastAppend()
	log.Printf("raft: %s 成为 leader term=%d", r.id, term)
}

// proposeEntry 是 leader 追加一条本地日志并立刻复制一遍（不等结果）。
func (r *raftNode) proposeEntry(e raftEntry) uint64 {
	r.mu.Lock()
	e.Index = r.lastIndex() + 1
	if e.Term == 0 {
		e.Term = r.term
	}
	if err := r.disk.appendLog(e); err != nil {
		log.Printf("raft: 追加日志失败（节点将退出共识）: %v", err)
		r.mu.Unlock()
		return 0
	}
	r.log = append(r.log, e)
	r.mu.Unlock()
	return e.Index
}

func (r *raftNode) metaLocked() raftMeta {
	return raftMeta{Term: r.term, VotedFor: r.votedFor, Commit: r.commit,
		Applied: r.applied, Base: r.base, BaseTerm: r.baseTerm, ClusterID: r.cluster}
}

// ------------------------------------------------ 复制

// broadcastAppend 给所有成员发 AppendEntries。心跳也走它：落后成员缺的条目
// 会顺路捎上，不需要单独的 catch-up 流程。
func (r *raftNode) broadcastAppend() {
	r.mu.Lock()
	if r.role != raftLeader {
		r.mu.Unlock()
		return
	}
	r.lastBeat = time.Now()
	// 自己也记一笔：预投票问的是"集群还活着吗"，一个正在发心跳的 leader
	// 显然活着。不刷新的话当了 1.2s 主之后它就会开始给别人投赞成票，
	// 而"别人"往往是刚重启、日志还落后的那台。
	r.lastAppend = r.lastBeat
	peers := append(append([]string{}, r.voters...), r.learners...)
	// 被移出成员表的对端继续收日志，直到它确认看过那条移除记录（默认 30s 后放弃，
	// 对端已经关机的情况不该让 leader 永远惦记着它）。
	for id, rt := range r.retiring {
		if time.Since(rt.Since) > 30*time.Second {
			delete(r.retiring, id)
			log.Printf("raft: 放弃等待 %s 确认被移出（%s 没有回应）", id, 30*time.Second)
			continue
		}
		if !contains(peers, id) {
			peers = append(peers, id)
		}
	}
	r.mu.Unlock()

	for _, p := range peers {
		if p == r.id {
			continue
		}
		peer := p
		r.spawnRPC(func() {
			ctx, cancel := r.rpcCtx(raftHeartbeat * 2)
			defer cancel()
			if _, err := r.sendAppend(ctx, peer); err != nil {
				log.Printf("raft: %s → %s 复制失败: %v", r.id, peer, err)
			}
		})
	}
	// 单节点集群没有对端可等：多数派就是自己，这一条必须就地提交，否则
	// Propose 会一直等到超时，lastQuorum 也不会续租（leader 会把自己选下台）。
	r.advanceCommit()
}

// sendAppend 是单播：从 nextIndex 开始，最多带 64 条。被拒绝就用对端报回的
// matchIndex 快速回退，而不是一格一格试。
func (r *raftNode) sendAppend(ctx context.Context, peer string) (uint64, error) {
	// 设备活跃度表在锁外取：它要拿 server 的锁，而提交路径是 s.mu → raft.mu，
	// 这里持着 raft.mu 再去拿 s.mu 就是把锁序反过来（会死锁）。
	var live map[string]int64
	if r.liveness != nil {
		live = r.liveness()
	}
	for attempt := 0; attempt < 8; attempt++ {
		r.mu.Lock()
		if r.role != raftLeader {
			r.mu.Unlock()
			return 0, errors.New("not leader")
		}
		next := r.nextIndex[peer]
		if next <= r.base {
			next = r.base + 1
		}
		prev := next - 1
		prevTerm, ok := r.termAt(prev)
		if !ok {
			r.mu.Unlock()
			return 0, errNeedSnapshot
		}
		var entries []raftEntry
		for i := next; i <= r.lastIndex() && len(entries) < 64; i++ {
			if e, ok := r.entryAt(i); ok {
				entries = append(entries, e)
			}
		}
		req := appendReq{Term: r.term, Leader: r.id, PrevIndex: prev,
			PrevTerm: prevTerm, Entries: entries, LeaderCommit: r.commit, Live: live}
		// 记下"这一包到底带了哪个提交点"：判断对端是否已经知道"你被移出"
		// 必须看它收到的那一包，而不是我此刻的 commit —— 别的复制协程可能
		// 刚把它推上去，于是我以为它知道了，其实它停在删除项之前。
		sentCommit := req.LeaderCommit
		term := r.term
		r.mu.Unlock()

		var out appendResp
		err := r.tr.Call(ctx, r.tr.EndpointOf(peer), "/v1/cluster/append", req, &out)
		if err != nil {
			return 0, err
		}
		r.mu.Lock()
		if out.Term > r.term {
			r.term = out.Term
			r.votedFor = ""
			r.role = raftFollower
			r.leader = ""
			r.resetElection()
			meta := r.metaLocked()
			r.mu.Unlock()
			r.disk.saveMeta(meta)
			return 0, fmt.Errorf("term advanced to %d", out.Term)
		}
		if r.term != term || r.role != raftLeader {
			r.mu.Unlock()
			return 0, errors.New("leadership changed")
		}
		if out.OK {
			match := prev + uint64(len(entries))
			if match > r.matchIndex[peer] {
				r.matchIndex[peer] = match
			}
			if out.MatchIndex > r.matchIndex[peer] {
				r.matchIndex[peer] = out.MatchIndex
			}
			r.nextIndex[peer] = r.matchIndex[peer] + 1
			r.lastHeard[peer] = time.Now()
			if rt, ok := r.retiring[peer]; ok && r.matchIndex[peer] >= rt.Index && sentCommit >= rt.Index {
				// 它不光收到了那条移除记录，还收到了"这条已提交"的提交点 ——
				// 缺一不可：只发记录不发提交点，它会停在这一条之前的 commit 上，
				// 永远不应用、也就永远不知道自己被下线（这正是 lab 里看到的现象）。
				delete(r.retiring, peer)
				log.Printf("raft: %s 已确认自己被移出成员表（日志 %d）", peer, rt.Index)
			}
			r.mu.Unlock()
			r.advanceCommit()
			return match, nil
		}
		// 拒绝：把 nextIndex 收到对端自报的最后一个下标之后（至少 base+1）。
		//
		// NeedSnap 是特例：对端的日志已经压到快照点（它比我"干净"），报回来的
		// 位置比我现在打算发的还靠前，必须相信它 —— 否则每轮都会算回同一个点，
		// 就是那句 "append rejected repeatedly"。
		back := out.MatchIndex + 1
		if back < r.base+1 {
			back = r.base + 1
		}
		if back > r.lastIndex()+1 {
			back = r.lastIndex() + 1
		}
		if back >= next && !out.NeedSnap {
			// 对端报回来的位置并不比我打算发的靠前：说明卡住的是同一个下标上
			// 的任期，退一格再试。
			back = next - 1
			if back < r.base+1 {
				back = r.base + 1
			}
		}
		r.nextIndex[peer] = back
		r.mu.Unlock()
	}
	return 0, errors.New("append rejected repeatedly")
}

var errNeedSnapshot = errors.New("raft: follower needs snapshot")

// seedPeerIndexLocked 给刚进成员表的节点一个起始复制位置。
//
// 不设的话它会在 map 里是 0，sendAppend 第一轮就发 prev=base，对端只能拒绝，
// 靠拒绝回退一点点试出来 —— 一台空节点要试几十轮才追上，控制台上就是"成员
// 在表里但一直同步中"。从日志末尾往回找是 raft 的标准做法。
func (r *raftNode) seedPeerIndexLocked(sets ...[]string) {
	for _, set := range sets {
		for _, p := range set {
			if p == r.id {
				continue
			}
			if _, ok := r.nextIndex[p]; !ok {
				r.nextIndex[p] = r.lastIndex() + 1
				r.matchIndex[p] = 0
			}
		}
	}
}

// waitCaughtUp 等本机的 applied 追平 commit。
//
// 成员变更是"读成员表 → 改 → 提议"的三步，而 applyMembers 要拿 server 的锁，
// 所以提交返回的那一刻，内存里的成员表可能还是旧的。照着旧表算下一份就会把刚
// 加入的节点漏掉（控制台上的症状：加了两台，第二台进来时第一台不见了）。
// 调用方必须**不持** server 锁。
func (r *raftNode) waitCaughtUp(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		r.mu.Lock()
		ok := r.applied >= r.commit
		r.mu.Unlock()
		if ok {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// advanceCommit 用 matchIndex 的中位数推进提交点。多数派 = 含自己在内。
func (r *raftNode) advanceCommit() {
	r.mu.Lock()
	if r.role != raftLeader {
		r.mu.Unlock()
		return
	}
	committedFor := func(set []string) uint64 {
		if len(set) == 0 {
			return 0
		}
		ms := make([]uint64, 0, len(set))
		for _, v := range set {
			if v == r.id {
				ms = append(ms, r.lastIndex())
				continue
			}
			ms = append(ms, r.matchIndex[v])
		}
		sort.Slice(ms, func(i, j int) bool { return ms[i] < ms[j] })
		return ms[len(ms)/2]
	}
	oldVoters := append([]string(nil), r.voters...)
	cand := committedFor(oldVoters)
	if r.pendingCfg != nil {
		// 配置项未提交时，新旧两个多数派都得同意（一次一变更的安全版）。
		if c := committedFor(r.pendingCfg); c < cand {
			cand = c
		}
	}
	old := r.commit
	if cand > r.commit && r.atTerm(cand) == r.term {
		r.commit = cand
	}
	if r.pendingCfg != nil && r.commit >= r.pendingIdx {
		r.voters = append([]string(nil), r.pendingCfg...)
		r.learners = learnersOf(r.pendMembers)
		r.seedPeerIndexLocked(r.voters, r.learners)
		r.pendingCfg, r.pendMembers, r.pendingIdx = nil, nil, 0
	}
	// 只有真的能凑到多数派才续租：只按"我是 leader"续租会让一个被隔离的
	// 老主永远不下台，双主就是这么来的。
	if alive := r.reachableLocked(oldVoters); alive*2 > len(oldVoters) && len(oldVoters) > 0 {
		r.lastQuorum = time.Now()
	}
	commit := r.commit
	advanced := commit > old
	if advanced {
		for idx, ch := range r.waiters {
			if idx <= commit {
				select {
				case ch <- nil:
				default:
				}
				delete(r.waiters, idx)
			}
		}
	}
	meta := r.metaLocked()
	r.mu.Unlock()
	if advanced {
		if err := r.disk.saveMeta(meta); err != nil {
			log.Printf("raft: 保存提交点失败: %v", err)
		}
		r.kickApply()
	}
}

// atTerm 返回下标 i 处条目的任期（leader 自己追加的一定在日志里）。
func (r *raftNode) atTerm(i uint64) uint64 {
	if t, ok := r.termAt(i); ok {
		return t
	}
	return 0
}

// reachableLocked 数一遍最近有过回应的节点（含自己）。
func (r *raftNode) reachableLocked(set []string) int {
	n := 0
	for _, v := range set {
		if v == r.id || time.Since(r.lastHeard[v]) < 3*raftHeartbeat {
			n++
		}
	}
	return n
}

// ------------------------------------------------ 提议

// Propose 追加一批行并等多数派确认。调用方必须已经持有 server 的锁 ——
// 这样"改内存"和"提交"之间不会被另一个写插进来。
func (r *raftNode) Propose(rows []HardRow) error {
	if len(rows) == 0 {
		return nil
	}
	r.mu.Lock()
	if r.role != raftLeader {
		who := r.leader
		r.mu.Unlock()
		if who == "" {
			return errors.New("集群没有 leader（可能已失去多数派）")
		}
		return fmt.Errorf("这台不是 leader（当前 leader 是 %s）", who)
	}
	idx := r.lastIndex() + 1
	e := raftEntry{Index: idx, Term: r.term, Rows: rows}
	if err := r.disk.appendLog(e); err != nil {
		r.mu.Unlock()
		return err
	}
	r.log = append(r.log, e)
	ch := make(chan error, 1)
	r.waiters[idx] = ch
	r.mu.Unlock()

	r.broadcastAppend()
	return r.waitCommit(idx, ch, nil)
}

// ProposeOps 追加一批台账行并等多数派确认。与 Propose 的差别只在载荷：
// 台账不进 State（它有自己的文件），所以不走 applyRows，走 onOps。
func (r *raftNode) ProposeOps(ents []opsEntry) error {
	if len(ents) == 0 {
		return nil
	}
	r.mu.Lock()
	if r.role != raftLeader {
		who := r.leader
		r.mu.Unlock()
		if who == "" {
			return errors.New("集群没有 leader（可能已失去多数派）")
		}
		return fmt.Errorf("这台不是 leader（当前 leader 是 %s）", who)
	}
	idx := r.lastIndex() + 1
	e := raftEntry{Index: idx, Term: r.term, Ops: ents}
	if err := r.disk.appendLog(e); err != nil {
		r.mu.Unlock()
		return err
	}
	r.log = append(r.log, e)
	ch := make(chan error, 1)
	r.waiters[idx] = ch
	r.mu.Unlock()

	r.broadcastAppend()
	return r.waitCommit(idx, ch, nil)
}

// noteRemovedLocked 记下"这份新成员表把谁摘掉了"，好让 leader 继续把那条移除
// 记录送到它手上。只在它确认收到之后才放手（见 sendAppend）。
//
// 为什么必须有这一步：raft 只要求 leader 复制给成员，成员表一变它就名正言顺地
// 停止了。于是被摘掉的那台永远停在删除项之前的提交点上，控制台还自称 voter，
// 设备还在往它心跳 —— 一台"看起来正常、其实已经被开除"的控制器。
func (r *raftNode) noteRemovedLocked(idx uint64, members []ClusterMember) {
	nv, nl := votersOf(members), learnersOf(members)
	for _, id := range append(append([]string{}, r.voters...), r.learners...) {
		if id == r.id || contains(nv, id) || contains(nl, id) {
			continue
		}
		if _, ok := r.retiring[id]; !ok {
			r.retiring[id] = retireTarget{Index: idx, Since: time.Now()}
		}
	}
}

// ProposeConfig 提议一次成员变更。members 必须是完整的成员表（含自己）。
func (r *raftNode) ProposeConfig(members []ClusterMember) error {
	r.mu.Lock()
	if r.role != raftLeader {
		r.mu.Unlock()
		return errors.New("成员变更必须在 leader 上发起")
	}
	idx := r.lastIndex() + 1
	e := raftEntry{Index: idx, Term: r.term, Members: members}
	if err := r.disk.appendLog(e); err != nil {
		r.mu.Unlock()
		return err
	}
	r.log = append(r.log, e)
	r.noteRemovedLocked(idx, members)
	r.pendingCfg = votersOf(members)
	r.pendMembers = members
	r.pendingIdx = idx
	ch := make(chan error, 1)
	r.waiters[idx] = ch
	r.mu.Unlock()

	r.broadcastAppend()
	if err := r.waitCommit(idx, ch, func() {
		r.mu.Lock()
		r.pendingCfg, r.pendMembers, r.pendingIdx = nil, nil, 0
		r.mu.Unlock()
	}); err != nil {
		return err
	}
	// 提交点只回到"我自己"：别人要等下一次 append 才会知道这一条已经提交，
	// 才会把它应用下去。这里等所有成员都拿到这一条，再补一次 append 把提交点
	// 带过去 —— 于是"控制台说已经生效"就等于"每个成员都知道它生效了"。
	r.waitPeersHave(idx, 2*time.Second)
	r.broadcastAppend()
	return nil
}

// waitPeersHave 等所有成员都收到下标 idx 之前的日志（最多等 d）。
// 被移出成员表的那台也算在里面：控制台说"已下线"的时候，最好它自己也已经知道了
// （否则它会继续接设备心跳，直到下一次复制才反应过来）—— 上限仍是 d，不等它。
func (r *raftNode) waitPeersHave(idx uint64, d time.Duration) {
	deadline := time.Now().Add(d)
	for {
		r.mu.Lock()
		ok := true
		peers := append(append([]string{}, r.voters...), r.learners...)
		for id := range r.retiring {
			if !contains(peers, id) {
				peers = append(peers, id)
			}
		}
		for _, p := range peers {
			if p == r.id {
				continue
			}
			if r.matchIndex[p] < idx {
				ok = false
				break
			}
		}
		r.mu.Unlock()
		if ok || !time.Now().Before(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *raftNode) waitCommit(idx uint64, ch chan error, onFail func()) error {
	cleanup := func() {
		r.mu.Lock()
		delete(r.waiters, idx)
		r.mu.Unlock()
		if onFail != nil {
			onFail()
		}
		// 这条日志可能永远提交不了，也可能以后提交。以后真的提交时，applyLoop
		// 会照常把它应用一遍 —— 行里带的是绝对值，重放一次不会写坏任何东西。
	}
	select {
	case err := <-ch:
		if err != nil {
			cleanup()
		}
		return err
	case <-time.After(raftCommitTimeout):
		cleanup()
		return fmt.Errorf("提交超时（%s 内未拿到多数派确认）", raftCommitTimeout)
	case <-r.stopc:
		cleanup()
		return errors.New("节点已停止")
	}
}

// ------------------------------------------------ 应用

func (r *raftNode) kickApply() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// applyLoop 是唯一的应用者。它不持 raft.mu 去调回调（回调要拿 server 的锁，
// 而 server 的写路径是"持 server 锁 → 提议"，反过来会死锁）。
func (r *raftNode) applyLoop(ctx context.Context) {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopc:
			return
		case <-t.C:
		case <-r.wake:
		}
		for {
			r.mu.Lock()
			from, to := r.applied+1, r.commit
			if from > to {
				r.mu.Unlock()
				break
			}
			var batch []raftEntry
			for i := from; i <= to; i++ {
				if e, ok := r.entryAt(i); ok {
					batch = append(batch, e)
					continue
				}
				// 日志被压掉了（只可能发生在我落后时）：跳过，等快照。
				batch = append(batch, raftEntry{Index: i, Noop: true})
			}
			r.mu.Unlock()

			// leader 也应用自己提的条目，不做任何"我刚写过所以跳过"的优化：
			// 跳过会让 state.json 永远缺着自己任内提交的那几行（重启后 applied
			// 已经等于 commit，日志不会再放一遍，那些配置就静默消失了）。
			// 行里带的是绝对值，重复应用是幂等的。
			for _, e := range batch {
				if err := r.applyEntry(e); err != nil {
					log.Printf("raft: 应用日志 %d 失败（状态可能已分叉）: %v", e.Index, err)
				}
			}
			r.finishApply(to)
		}
	}
}

func (r *raftNode) applyEntry(e raftEntry) error {
	switch {
	case e.isConfig():
		if err := r.applyMembers(e.Members); err != nil {
			return err
		}
		// 配置项也是日志的一部分：follower 应用它之后，自己的 voter 集合必须
		// 跟着变。只更新"控制台看到的成员表"而不更新这里，结果是那个刚被提升
		// 的节点永远不参选 —— leader 一死，集群就停在"没有 leader"，直到原来
		// 那台机器被人重新拉起来。
		r.mu.Lock()
		// 换主之后接手处理这次变更的新 leader 也要知道"有人该被通知"。
		if r.role == raftLeader {
			r.noteRemovedLocked(e.Index, e.Members)
		}
		r.voters = votersOf(e.Members)
		r.learners = learnersOf(e.Members)
		r.seedPeerIndexLocked(r.voters, r.learners)
		r.pendingCfg, r.pendMembers, r.pendingIdx = nil, nil, 0
		// 这份成员表里已经没有本机：我被下线（或被人替换）了。
		// 从这一刻起不再参选，也不再接活 —— 但先让进程把自己该做的收尾做完。
		selfOut := len(r.voters) > 0 && !contains(r.voters, r.id) && !contains(r.learners, r.id)
		becameRetired := selfOut && !r.retired
		if becameRetired {
			r.retired = true
			if r.role == raftLeader {
				r.role, r.leader = raftFollower, ""
			}
		}
		cb := r.onRetire
		r.mu.Unlock()
		if becameRetired && cb != nil {
			cb(fmt.Sprintf("成员表（日志 %d）里已经没有本机", e.Index))
		}
		return nil
	case e.Rows != nil:
		return r.applyRows(e.Rows)
	case len(e.Ops) > 0:
		// 台账行：不碰 State，只把它写进本机的台账文件。应用即落盘，重放也
		// 幂等吗？不 —— append-only 文件重复应用会出重复行。防线在调用侧：
		// applied 推进之前一条只应用一次（applyLoop 的 batch 是按 commit 截的），
		// 崩溃后 applied 回退最多重放半批 —— 台账页多出半批重复行可接受，
		// 比为它加"已应用台账指纹"这类状态便宜（那是 P0 立过的规矩：台账是
		// 人看的历史，不是机器对齐的硬状态）。
		if r.onOps != nil {
			r.onOps(e.Ops)
		}
		return nil
	default:
		return nil
	}
}

func (r *raftNode) finishApply(upto uint64) {
	r.mu.Lock()
	if upto > r.applied {
		r.applied = upto
	}
	meta := r.metaLocked()
	r.mu.Unlock()
	r.disk.saveMeta(meta)
}

// ------------------------------------------------ 压缩

func (r *raftNode) maybeCompact() {
	r.mu.Lock()
	if len(r.log) < raftCompactAt {
		r.mu.Unlock()
		return
	}
	// 只压到"所有成员都已经拿到"的位置，否则落后的 follower 会被迫拉快照。
	trim := r.commit
	for _, p := range append(append([]string{}, r.voters...), r.learners...) {
		if p == r.id {
			continue
		}
		if m := r.matchIndex[p]; m < trim {
			trim = m
		}
	}
	if trim <= r.base {
		r.mu.Unlock()
		return
	}
	term, ok := r.termAt(trim)
	if !ok {
		r.mu.Unlock()
		return
	}
	var keep []raftEntry
	for _, e := range r.log {
		if e.Index > trim {
			keep = append(keep, e)
		}
	}
	r.log = keep
	r.base, r.baseTerm = trim, term
	meta := r.metaLocked()
	r.mu.Unlock()
	if err := r.disk.rewriteLog(keep); err != nil {
		log.Printf("raft: 压缩日志失败: %v", err)
	}
	r.disk.saveMeta(meta)
}

// ------------------------------------------------ RPC 处理

func (r *raftNode) handleVote(req voteReq) voteResp {
	r.mu.Lock()
	defer r.mu.Unlock()
	resp := voteResp{Term: r.term}
	if req.PreVote {
		// 预投票绝不能改 term / votedFor：改了就等于"预投票也翻任期"，
		// 正好毁掉它存在的意义。
		if req.Term < r.term {
			return resp
		}
		// 刚刚还在跟 leader（或某个候选人）打交道，说明集群是活的：
		// 不给你票。这一条是"重启不再抢主"的关键 —— 重启的那台只是
		// 自己内存里没有 lastHeard，不是集群真的没主了。
		if time.Since(r.lastAppend) < raftElectionMin {
			return resp
		}
		resp.Granted = req.LastTerm > r.lastTerm() ||
			(req.LastTerm == r.lastTerm() && req.LastIndex >= r.lastIndex())
		return resp
	}
	if req.Term < r.term {
		return resp
	}
	if req.Term > r.term {
		r.term, r.votedFor, r.role, r.leader = req.Term, "", raftFollower, ""
		r.resetElection()
		r.disk.saveMeta(r.metaLocked())
	}
	// 候选人的日志至少和我一样新才投票：把票投给一个落后的节点，等于把
	// 已提交的东西丢掉。
	upToDate := req.LastTerm > r.lastTerm() ||
		(req.LastTerm == r.lastTerm() && req.LastIndex >= r.lastIndex())
	if (r.votedFor == "" || r.votedFor == req.Candidate) && upToDate {
		r.votedFor = req.Candidate
		r.resetElection()
		if err := r.disk.saveMeta(r.metaLocked()); err != nil {
			// 记不住这一票就别投：重启后重复投票是双主的入口。
			r.votedFor = ""
			return resp
		}
		resp.Granted = true
	}
	resp.Term = r.term
	return resp
}

func (r *raftNode) handleAppend(req appendReq) appendResp {
	r.mu.Lock()
	// Live 是软状态，落进本机内存就行；回调要拿 server 的锁，必须等这里的
	// raft.mu 释放之后再调（defer 的顺序保证它最后跑）。
	var liveCb func(map[string]int64)
	var live map[string]int64
	defer func() {
		r.mu.Unlock()
		if liveCb != nil {
			liveCb(live)
		}
	}()
	resp := appendResp{Term: r.term}
	if req.Term < r.term {
		return resp
	}
	if req.Term > r.term || r.role != raftFollower {
		r.term = req.Term
		r.votedFor = ""
		r.role = raftFollower
		r.disk.saveMeta(r.metaLocked())
	}
	r.leader = req.Leader
	r.lastAppend = time.Now()
	// follower 也要记一笔"我刚收到 leader 的包"。lastHeard 原本只在 leader 收到
	// append 应答时才写，于是 follower 上永远是空表，控制台就把活着的 leader
	// 标成"失联"（lab 实测 2026-11-14：三台健康，每台 follower 都把别人报失联）。
	if req.Leader != r.id {
		r.lastHeard[req.Leader] = time.Now()
	}
	if len(req.Live) > 0 && r.onLive != nil {
		liveCb, live = r.onLive, req.Live
	}
	r.resetElection()
	resp.Term = r.term
	// 新 leader 上任了：我之前记的"自己提的条目"不再作数，否则换主后会漏应用。
	if req.PrevIndex < r.base {
		resp.NeedSnap = true
		r.needSnap = true
		resp.MatchIndex = r.lastIndex()
		return resp
	}
	pt, ok := r.termAt(req.PrevIndex)
	if !ok || pt != req.PrevTerm {
		// 日志对不上：把真实的最长下标告诉 leader，让它从那里重试。
		resp.MatchIndex = r.lastIndex()
		return resp
	}
	for _, e := range req.Entries {
		if e.Index <= r.base {
			continue
		}
		if cur, ok := r.entryAt(e.Index); ok {
			if cur.Term == e.Term {
				continue
			}
			// 冲突：截到冲突点之前，把新条目换上（raft §5.3）。
			r.log = r.log[:e.Index-r.base-1]
			if err := r.disk.rewriteLog(r.log); err != nil {
				log.Printf("raft: 截断日志失败: %v", err)
			}
		}
		if err := r.disk.appendLog(e); err != nil {
			return resp
		}
		r.log = append(r.log, e)
		// 成员变更"进了日志就生效"，不等提交 —— 这是 raft 单步变更的标准做法，
		// 也是本实现里最要紧的一处时序：leader 刚把"提升某台为 voter"提交完就
		// 挂了，如果那台机器要等 commit 才认自己是 voter，它就不会参选，剩下的
		// 节点又凑不齐票 —— 集群停在"没有 leader"，看起来像整个控制面死了。
		if e.isConfig() {
			r.voters = votersOf(e.Members)
			r.learners = learnersOf(e.Members)
			r.seedPeerIndexLocked(r.voters, r.learners)
		}
	}
	resp.OK = true
	resp.MatchIndex = r.lastIndex()
	if req.LeaderCommit > r.commit {
		next := req.LeaderCommit
		if next > r.lastIndex() {
			next = r.lastIndex()
		}
		if next > r.commit {
			r.commit = next
			r.disk.saveMeta(r.metaLocked())
			r.kickApply()
		}
	}
	return resp
}

// Snapshot 生成一份"当前已提交状态"的副本。调用方在持 server 锁的情况下调用，
// 所以此刻没有在途提议，state 正好停在 commit 上。
func (r *raftNode) Snapshot(stateJSON json.RawMessage, members []ClusterMember) raftSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	bt, _ := r.termAt(r.commit)
	return raftSnapshot{Term: r.term, Index: r.commit, BaseTerm: bt,
		Members: members, State: stateJSON}
}

// InstallSnapshot 把快照装进来：状态整体替换，日志从头开始。
func (r *raftNode) InstallSnapshot(s *raftSnapshot, stateJSON json.RawMessage) error {
	if err := r.applyState(stateJSON); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.Term > r.term {
		r.term = s.Term
	}
	r.base, r.baseTerm = s.Index, s.BaseTerm
	r.log = nil
	r.commit, r.applied = s.Index, s.Index
	r.voters = votersOf(s.Members)
	r.learners = learnersOf(s.Members)
	r.needSnap = false
	if err := r.disk.rewriteLog(nil); err != nil {
		return err
	}
	return r.disk.saveMeta(r.metaLocked())
}

// maybeFetchSnapshot 在"leader 需要的日志我已经没有了"时去拉一份。
func (r *raftNode) maybeFetchSnapshot(leader string) {
	if leader == "" || leader == r.id {
		return
	}
	r.mu.Lock()
	need := r.needSnap
	recent := time.Since(r.lastSnapshotFetch) < 2*time.Second
	if need && !recent {
		r.lastSnapshotFetch = time.Now()
	}
	r.mu.Unlock()
	if !need || recent {
		return
	}
	ctx, cancel := r.rpcCtx(10 * time.Second)
	defer cancel()
	s, err := r.tr.Fetch(ctx, r.tr.EndpointOf(leader))
	if err != nil {
		log.Printf("raft: 拉取快照失败: %v", err)
		return
	}
	if err := r.InstallSnapshot(s, s.State); err != nil {
		log.Printf("raft: 安装快照失败: %v", err)
		return
	}
	log.Printf("raft: 已同步到 %s 的快照 index=%d", leader, s.Index)
}

// Status 给控制台一帧集群视图。
func (r *raftNode) Status() raftStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := raftStatus{Role: string(r.role), Leader: r.leader, Term: r.term,
		Commit: r.commit, Applied: r.applied, Members: map[string]memberPeer{},
		Retired: r.retired}
	fill := func(ids []string, voter bool) {
		for _, id := range ids {
			p := st.Members[id]
			p.Voter = voter
			if voter {
				p.Match, p.Next = r.matchIndex[id], r.nextIndex[id]
			}
			if t, ok := r.lastHeard[id]; ok {
				p.LastHeard = t.Unix()
			}
			p.Reachable = id == r.id || time.Since(r.lastHeard[id]) < 3*raftHeartbeat
			st.Members[id] = p
		}
	}
	fill(r.voters, true)
	fill(r.learners, false)
	if r.op != nil {
		cp := *r.op
		st.Op = &cp
	}
	return st
}

func (r *raftNode) IsLeader() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.role == raftLeader
}

func (r *raftNode) Leader() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.leader
}

func (r *raftNode) IsVoter() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return contains(r.voters, r.id)
}

func (r *raftNode) setOp(op *memberOp) {
	r.mu.Lock()
	r.op = op
	r.mu.Unlock()
}

func (r *raftNode) getOp() *memberOp {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.op == nil {
		return nil
	}
	cp := *r.op
	return &cp
}

// matchOf 暴露给驱动器判断"learner 追上来了没有"。
func (r *raftNode) matchOf(id string) (uint64, uint64, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.matchIndex[id], r.commit, r.lastIndex()
}

func (r *raftNode) voterSet() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.voters...), append([]string(nil), r.learners...)
}

// ------------------------------------------------ 小工具

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, a...), b...) {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func votersOf(ms []ClusterMember) []string {
	var out []string
	for _, m := range ms {
		if m.Role == memberVoter && m.Status == memberActive {
			out = append(out, m.ID)
		}
	}
	return out
}

func learnersOf(ms []ClusterMember) []string {
	var out []string
	for _, m := range ms {
		if m.Role == memberLearner && m.Status == memberActive {
			out = append(out, m.ID)
		}
	}
	return out
}
