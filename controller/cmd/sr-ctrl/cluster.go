package main

// cluster.go —— 控制器集群：成员模型、加入/替换/下线、以及节点之间的 RPC。
//
// 这一层的产品目标是（owner 2026-11-14）：集群的成员变更全部在控制台完成，
// 不需要后台部署。三种操作对应三个真实场景：
//
//	新增  客户只有一台控制器，后来要上 HA            → 加第 2、3 台
//	替换  某台坏了，换一台新的顶上                    → 新的替旧的，旧的自动下线
//	下线  发现 4 台用不着，退一台（或临时摘掉检修）    → 摘掉后不再参与多数派
//
// 三种操作在共识层是同一件事：改一次成员表（raft 配置项）。区别只在顺序：
//
//	新增 = 加 learner → 追上日志 → 提升 voter
//	替换 = 新增 + 把旧的标成 decommissioned
//	下线 = 把目标标成 decommissioned
//
// "先 learner 再 voter"不是讲究，是安全要求：一个空的节点直接进 voter 集合，
// 会让多数派门槛落在一个还没有日志的节点上 —— 那时候两台活着的机器就凑不出
// 多数派，等于自己把自己搞停了。
//
// 安全模型：
//   - 节点之间走 mTLS（共用一套 CA），外加 X-Cluster-Secret 头。
//   - 加入码（join token）是 HMAC 签名的短期凭证，由 leader 签发，绑定
//     cluster id + 成员 id + 地址 + 过期时间。它只能用来加入一次。
//   - 加入应答里会带 CA 私钥：集群必须共用同一套 CA，否则 A 发的设备证书 B
//     不认（docs/controller-ha-plan.md §6）。这一步的风险与"把 ca.key 拷到
//     三台机器"完全等价 —— 集群里本来就没有"只在一台上存在的根"。

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	memberVoter          = "voter"
	memberLearner        = "learner"
	memberActive         = "active"
	memberDraining       = "draining"
	memberDecommissioned = "decommissioned"

	joinTokenTTL = 30 * time.Minute

	// ctrlVersion 进成员表，控制台上能看出"这台还是旧版本"——滚动升级靠它决定
	// 下一台该换谁。
	ctrlVersion = "sr-ctrl/0.4 (cluster)"
)

// ClusterMember 是一个控制器节点。它进共识（State.Cluster），因为"谁有投票权"
// 必须全局一致 —— 两台机器对成员表有不同看法，就是两台机器各自算多数派。
type ClusterMember struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`          // mTLS host:port，设备也用它
	Console  string `json:"console,omitempty"` // 控制台 host:port，用于 follower 307
	// ConsoleTLS 是"这个控制台地址是 https 吗"。307 必须带对 scheme：同一台
	// 机器常常同时开 8443(https) 与 8088(明文回环)，从明文口进来就猜 http 会
	// 把请求送到一个只讲 TLS 的端口上（2026-11-14 lab 实测，见 ha-plan §9.3）。
	ConsoleTLS bool   `json:"console_tls,omitempty"`
	Role       string `json:"role"`   // voter | learner
	Status     string `json:"status"` // active | draining | decommissioned
	JoinedAt   int64  `json:"joined_at_unix"`
	Replaces   string `json:"replaces,omitempty"` // 它替换了谁（审计用）
	Version    string `json:"version,omitempty"`
	Note       string `json:"note,omitempty"` // 下线原因，给下一个看这个页面的人
}

// ClusterConfig 是集群的成员表。Generation 每次变更 +1，控制台拿它判断
// "我看到的这一版是不是已经过时了"。
type ClusterConfig struct {
	ID         string                    `json:"id"`
	Generation uint64                    `json:"generation"`
	Members    map[string]*ClusterMember `json:"members"`
}

func (c *ClusterConfig) sorted() []ClusterMember {
	if c == nil {
		return nil
	}
	out := make([]ClusterMember, 0, len(c.Members))
	for _, m := range c.Members {
		if m != nil {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].JoinedAt != out[j].JoinedAt {
			return out[i].JoinedAt < out[j].JoinedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// activeEndpoints 是设备应该尝试的控制器地址（只含 active 的成员）。
// 正在下线/已下线的成员必须从这里消失：设备列表里留着一个已经拆走的地址，
// 每次轮换都要白等一次超时。
func (c *ClusterConfig) activeEndpoints() []string {
	var out []string
	for _, m := range c.sorted() {
		if m.Status == memberActive && m.Endpoint != "" {
			out = append(out, m.Endpoint)
		}
	}
	return out
}

// ---------------------------------------------------------------- 加入码

// joinPayload 是加入码里带的东西。它同时是"要加谁"和"加进来叫什么"。
type joinPayload struct {
	Cluster  string `json:"cluster"`
	Member   string `json:"member"`
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Console  string `json:"console,omitempty"`
	Replaces string `json:"replaces,omitempty"`
	Expires  int64  `json:"exp"`
	// CAPEM 让"第一次见面"是可验证的：加入方还没拿到集群 CA，如果直接
	// InsecureSkipVerify，任何中间人都能冒充 leader 骗走一次加入。把 CA 写进
	// 加入码，加入方就只信这一个根 —— 与"先把 ca.pem 拷过去"等价，但不需要
	// 人手拷贝。
	CAPEM string `json:"ca,omitempty"`
}

func signJoinToken(secret string, p joinPayload) string {
	body := mustMarshal(p)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." +
		hex.EncodeToString(mac.Sum(nil))[:32]
}

func verifyJoinToken(secret, token string) (joinPayload, error) {
	var p joinPayload
	i := strings.LastIndex(token, ".")
	if i <= 0 {
		return p, errors.New("加入码格式不对")
	}
	body, err := base64.RawURLEncoding.DecodeString(token[:i])
	if err != nil {
		return p, errors.New("加入码格式不对")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))[:32]
	if !hmac.Equal([]byte(want), []byte(token[i+1:])) {
		return p, errors.New("加入码签名不对（不是本集群签发的，或者密钥已经换过）")
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return p, errors.New("加入码内容读不出来")
	}
	if time.Now().Unix() > p.Expires {
		return p, errors.New("加入码已过期，请重新生成")
	}
	return p, nil
}

// ---------------------------------------------------------------- 服务端状态

// clusterSecret 是节点之间的共享密钥，也用来签加入码。它第一次由 init 生成，
// 之后靠加入应答分发。
func (s *server) clusterSecret() string {
	if s.secret != "" {
		return s.secret
	}
	b, err := os.ReadFile(filepath.Join(s.dataDir, "cluster", "secret"))
	if err != nil {
		return ""
	}
	s.secret = strings.TrimSpace(string(b))
	return s.secret
}

func (s *server) writeClusterSecret(sec string) error {
	dir := filepath.Join(s.dataDir, "cluster")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	s.secret = sec
	return os.WriteFile(filepath.Join(dir, "secret"), []byte(sec), 0600)
}

// clusterNodeID 是本机的成员 id。它必须跨重启稳定：换一个 id 等于"新节点"，
// 旧的那票还挂在成员表里没人清。
func (s *server) clusterNodeID() string { return s.nodeID }

func (s *server) writeNodeID(id string) error {
	dir := filepath.Join(s.dataDir, "cluster")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	s.nodeID = id
	return os.WriteFile(filepath.Join(dir, "node-id"), []byte(id), 0600)
}

// membersWithSelf 返回当前成员表（含自己）。锁内取，锁外改。
func (s *server) membersSnapshot() []ClusterMember {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Cluster == nil {
		return nil
	}
	return s.state.Cluster.sorted()
}

// memberEndpoints 是给设备的地址列表。集群没开的时候就是自己的监听地址。
func (s *server) memberEndpoints() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.memberEndpointsLocked()
}

func (s *server) memberEndpointsLocked() []string {
	if s.state.Cluster == nil {
		return append([]string{}, s.tlsAddrs...)
	}
	return s.state.Cluster.activeEndpoints()
}

func (s *server) leaderEndpointLocked() string {
	if s.raft == nil {
		return ""
	}
	id := s.raft.Leader()
	if id == "" || s.state.Cluster == nil {
		return ""
	}
	if m := s.state.Cluster.Members[id]; m != nil {
		return m.Endpoint
	}
	return ""
}

// ---------------------------------------------------------------- 节点间传输

// httpTransport 走 mTLS。它不要求客户端证书（那套只给设备用），认证靠
// 共享密钥头 —— 能读到密钥的节点本来就是集群成员。
type httpTransport struct {
	s *server
}

// EndpointOf 查成员地址。读的是原子快照，不拿 s.mu（见 server.peerAddr 的注释）。
func (t *httpTransport) EndpointOf(id string) string {
	if p := t.s.peerAddr.Load(); p != nil {
		return p.m[id]
	}
	return ""
}

// peerAddrs 是成员地址表的只读快照。
type peerAddrs struct{ m map[string]string }

// publishPeerAddrsLocked 重建地址快照。持 s.mu 调用（成员表变了就重建）。
func (s *server) publishPeerAddrsLocked() {
	m := make(map[string]string, 4)
	if s.state.Cluster != nil {
		for _, cm := range s.state.Cluster.Members {
			if cm != nil && cm.Endpoint != "" {
				m[cm.ID] = cm.Endpoint
			}
		}
	}
	s.peerAddr.Store(&peerAddrs{m: m})
}

func (t *httpTransport) Call(ctx context.Context, target, path string, in, out any) error {
	if target == "" {
		return errors.New("未知成员地址")
	}
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	url := "https://" + target + path
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Cluster-Secret", t.s.clusterSecret())
	resp, err := t.s.clusterHTTP().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (t *httpTransport) Fetch(ctx context.Context, target string) (*raftSnapshot, error) {
	if target == "" {
		return nil, errors.New("未知成员地址")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://"+target+"/v1/cluster/snapshot", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Cluster-Secret", t.s.clusterSecret())
	resp, err := t.s.clusterHTTP().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("snapshot: HTTP %d", resp.StatusCode)
	}
	var snap raftSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// clusterHTTP 是节点间用的客户端：只信本集群的 CA，短超时。
//
// 客户端是缓存的，所以**换 CA 之后必须置空**（reloadCA 里做）：加入集群时本机
// 的信任根从"自己生成的 CA"换成了"集群 CA"，缓存住的客户端还拿着旧根，症状是
// 成员在表里但一条日志都复制不过去（x509: certificate signed by unknown authority）。
func (s *server) clusterHTTP() *http.Client {
	if c := s.clusterCli.Load(); c != nil {
		return c
	}
	s.mu.Lock()
	pool := s.caPool
	s.mu.Unlock()
	c := newClusterClient(pool)
	s.clusterCli.Store(c)
	return c
}

func newClusterClient(pool *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs: pool, MinVersion: tls.VersionTLS12,
		}},
	}
}

// clusterAuth 检查共享密钥。空密钥（没开集群）一律拒绝，避免误配把
// /v1/cluster/* 变成匿名可写。
func (s *server) clusterAuth(w http.ResponseWriter, r *http.Request) bool {
	sec := s.clusterSecret()
	if sec == "" {
		http.Error(w, "cluster not enabled", 503)
		return false
	}
	if !hmac.Equal([]byte(r.Header.Get("X-Cluster-Secret")), []byte(sec)) {
		http.Error(w, "bad cluster secret", 403)
		return false
	}
	return true
}

func (s *server) handleClusterVote(w http.ResponseWriter, r *http.Request) {
	if !s.clusterAuth(w, r) || s.raft == nil {
		return
	}
	var req voteReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	writeJSON(w, s.raft.handleVote(req))
}

func (s *server) handleClusterAppend(w http.ResponseWriter, r *http.Request) {
	if !s.clusterAuth(w, r) || s.raft == nil {
		return
	}
	var req appendReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	writeJSON(w, s.raft.handleAppend(req))
}

// handleClusterSnapshot 把当前已提交的状态整份寄给落后的节点。
//
// 必须持 s.mu 取：写路径是"持 s.mu → 提议"，所以持锁的这一刻不可能有半提交
// 的提议在途，state 正好停在 raft.commit 上。
func (s *server) handleClusterSnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.clusterAuth(w, r) || s.raft == nil {
		return
	}
	s.mu.Lock()
	b, err := json.Marshal(s.state)
	members := []ClusterMember{}
	if s.state.Cluster != nil {
		members = s.state.Cluster.sorted()
	}
	snap := s.raft.Snapshot(b, members)
	s.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, snap)
}

// ---------------------------------------------------------------- 加入

type joinRequest struct {
	Token   string `json:"token"`
	Version string `json:"version"`
	// ConsoleTLS 由加入方自报：只有它自己知道自己的控制台是 https 还是明文，
	// leader 拿它写进成员表，之后 follower 的 307 才知道该用哪个 scheme。
	ConsoleTLS bool `json:"console_tls,omitempty"`
}

type joinResponse struct {
	ClusterID string          `json:"cluster_id"`
	MemberID  string          `json:"member_id"`
	Secret    string          `json:"secret"`
	CAPEM     string          `json:"ca_pem"`
	CAKey     string          `json:"ca_key"`
	Members   []ClusterMember `json:"members"`
	Snapshot  raftSnapshot    `json:"snapshot"`
}

// handleClusterJoin 是"新控制器把自己接进来"的唯一入口。它只接受加入码，
// 不接受控制台会话：这一刻新节点还没有任何身份，加入码就是它的身份。
func (s *server) handleClusterJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	if s.raft == nil || !s.raft.IsLeader() {
		// 加入是写操作：只有 leader 能把它写进成员表。
		s.redirectToLeader(w, r)
		return
	}
	var req joinRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	p, err := verifyJoinToken(s.clusterSecret(), req.Token)
	if err != nil {
		http.Error(w, err.Error(), 403)
		return
	}
	s.mu.Lock()
	cur := s.state.Cluster
	if cur == nil || cur.ID != p.Cluster {
		s.mu.Unlock()
		http.Error(w, "加入码不属于这个集群", 403)
		return
	}
	existing := cur.Members[p.Member]
	s.mu.Unlock()

	if existing != nil && existing.Status == memberActive {
		http.Error(w, "这个加入码已经用过了（成员已存在）", 409)
		return
	}
	// 地址不能被两个成员同时占着：那会让 EndpointOf 有歧义，请求会打到一台
	// 机器上而以为在跟另一台说话。规则只有一份（endpointConflict，签发加入码时
	// 也走它），否则"签发时放行、加入时拒绝"这种自相矛盾迟早会出现 —— 替换场景
	// 就是：旧机器坏了、新机器直接接管它原来的 IP，签发要放行，加入也必须放行。
	if msg := endpointConflict(s.membersSnapshot(), p.Endpoint, p.Replaces); msg != "" {
		http.Error(w, msg, 409)
		return
	}

	nm := ClusterMember{ID: p.Member, Name: p.Name, Endpoint: p.Endpoint,
		Console: p.Console, ConsoleTLS: req.ConsoleTLS,
		Role: memberLearner, Status: memberActive,
		JoinedAt: time.Now().Unix(), Replaces: p.Replaces, Version: req.Version}
	if err := s.applyMembershipChange("add", nm.ID, p.Replaces, func(ms []ClusterMember) []ClusterMember {
		return upsertMember(ms, nm)
	}); err != nil {
		log.Printf("cluster: 加入 %s 失败: %v", nm.Name, err)
		http.Error(w, "成员变更没有提交："+err.Error(), 500)
		return
	}

	s.mu.Lock()
	caPEM, _ := os.ReadFile(filepath.Join(s.dataDir, "ca.pem"))
	caKey, _ := os.ReadFile(filepath.Join(s.dataDir, "ca.key"))
	b, _ := json.Marshal(s.state)
	members := []ClusterMember{}
	if s.state.Cluster != nil {
		members = s.state.Cluster.sorted()
	}
	cid := s.state.Cluster.ID
	snap := s.raft.Snapshot(b, members)
	s.mu.Unlock()

	log.Printf("cluster: 成员 %s（%s）已加入为 learner，等它追上日志后提升为 voter", nm.Name, nm.Endpoint)
	// 提升与替换的收尾交给 sweep：它同时覆盖"控制台加的"和"进程直接加入的"。
	s.kickClusterSweep()

	writeJSON(w, joinResponse{
		ClusterID: cid, MemberID: nm.ID, Secret: s.clusterSecret(),
		CAPEM: string(caPEM), CAKey: string(caKey), Members: members, Snapshot: snap,
	})
}

func upsertMember(ms []ClusterMember, m ClusterMember) []ClusterMember {
	out := make([]ClusterMember, 0, len(ms)+1)
	replaced := false
	for _, x := range ms {
		if x.ID == m.ID {
			out = append(out, m)
			replaced = true
			continue
		}
		out = append(out, x)
	}
	if !replaced {
		out = append(out, m)
	}
	return out
}

// applyMembershipChange 读成员表 → 改 → 提议。整个过程不持 s.mu 等待提交：
// 配置项应用到状态时要拿 s.mu，持锁等待就是死锁。
func (s *server) applyMembershipChange(kind, memberID, replaceID string, mut func([]ClusterMember) []ClusterMember) error {
	s.clusterOps.Lock()
	defer s.clusterOps.Unlock()
	if s.raft == nil {
		return errors.New("这个控制器还没加入集群")
	}
	if !s.raft.IsLeader() {
		return fmt.Errorf("这台不是 leader（当前 leader 是 %s）", s.raft.Leader())
	}
	// 先把上一次变更应用完再读成员表：否则"加一台、再加一台"的第二份提议里
	// 会没有第一台。
	s.raft.waitCaughtUp(2 * time.Second)
	before := s.membersSnapshot()
	next := mut(before)
	if err := s.raft.ProposeConfig(next); err != nil {
		s.raft.setOp(&memberOp{Kind: kind, MemberID: memberID, ReplaceID: replaceID,
			Phase: "failed", Err: err.Error(), StartedAt: time.Now().Unix()})
		return err
	}
	s.raft.setOp(&memberOp{Kind: kind, MemberID: memberID, ReplaceID: replaceID,
		Phase: "done", StartedAt: time.Now().Unix()})
	// 提交返回 ≠ 本机已经应用：配置项要经过 applyMembers 才进 state.Cluster。
	// 加入流程紧接着就要把这份成员表回给新节点（它要在里面找到自己），所以
	// 这里等一下 —— 否则新节点拿到的是"上一版名单"，加入直接失败。
	s.raft.waitCaughtUp(2 * time.Second)
	// 自己把自己移出 voter 集合之后必须让位：不然这个已经没有投票权的进程
	// 还会继续发心跳，剩下的节点永远选不出新主。
	if s.raft != nil && !s.raft.IsVoter() {
		log.Printf("cluster: 本机已不在 voter 集合里，退出 leader 角色")
		s.raft.stepDown()
	}
	return nil
}

// ---------------------------------------------------------------- 控制台视图

type clusterMemberView struct {
	ClusterMember
	Leader    bool   `json:"leader"`
	LiveRole  string `json:"live_role"` // leader | follower | learner | 失联
	Match     uint64 `json:"match"`
	Behind    uint64 `json:"behind"`
	Reachable bool   `json:"reachable"`
	LastHeard int64  `json:"last_heard_unix"`
	Self      bool   `json:"self"`
}

type clusterView struct {
	Mode      string              `json:"mode"` // standalone | cluster
	ClusterID string              `json:"cluster_id,omitempty"`
	Self      string              `json:"self"`
	Leader    string              `json:"leader"`
	Role      string              `json:"role"`
	Term      uint64              `json:"term"`
	Commit    uint64              `json:"commit"`
	Applied   uint64              `json:"applied"`
	Quorum    bool                `json:"quorum"`
	Degraded  string              `json:"degraded,omitempty"`
	Op        *memberOp           `json:"op,omitempty"`
	Members   []clusterMemberView `json:"members"`
	TLSAddrs  []string            `json:"tls_addrs"`
	// Reason 是"为什么现在不能用集群功能"的人话（没开集群 / 不是 leader ...）。
	Reason string `json:"reason,omitempty"`
	// Retired 表示本机已经被移出成员表：这台机器上的一切都还在，但它不再是
	// 集群的一部分，也不该再服务设备。
	Retired bool `json:"retired,omitempty"`
}

func (s *server) clusterViewLocked() clusterView {
	v := clusterView{Mode: "standalone", Self: s.nodeID, Role: "standalone",
		Members: []clusterMemberView{}, TLSAddrs: append([]string{}, s.tlsAddrs...)}
	if s.state.Cluster == nil {
		v.Reason = "还没开启集群：开启后可以在这里添加 / 替换 / 下线控制器"
		return v
	}
	v.Mode, v.ClusterID = "cluster", s.state.Cluster.ID
	v.TLSAddrs = s.state.Cluster.activeEndpoints()
	if s.raft == nil {
		v.Reason = "集群配置存在但本进程没有运行共识（检查日志）"
		return v
	}
	st := s.raft.Status()
	v.Role, v.Leader, v.Term = st.Role, st.Leader, st.Term
	v.Commit, v.Applied, v.Op = st.Commit, st.Applied, st.Op
	for _, m := range s.state.Cluster.sorted() {
		peer := st.Members[m.ID]
		mv := clusterMemberView{ClusterMember: m, Leader: st.Leader == m.ID,
			Match: peer.Match, Reachable: peer.Reachable, LastHeard: peer.LastHeard,
			Self: m.ID == s.nodeID}
		// matchIndex 只有 leader 在维护：follower 上它是空的，照抄就会让控制台
		// 在健康集群里显示"每台都落后 43 条"。只有 leader 有资格报落后多少。
		if st.Role != string(raftLeader) {
			mv.Match, mv.Behind = 0, 0
		} else {
			if mv.Self {
				mv.Match = st.Commit // leader 自己就是准绳，不看 matchIndex
			}
			if st.Commit > mv.Match {
				mv.Behind = st.Commit - mv.Match
			}
		}
		switch {
		case m.Status != memberActive:
			mv.LiveRole = m.Status
		case st.Leader == m.ID:
			mv.LiveRole = "leader"
		case m.Role == memberLearner:
			mv.LiveRole = "learner"
		case mv.Self:
			mv.LiveRole = "follower"
		case st.Role != string(raftLeader):
			// follower 只跟 leader 有来往，别的 peer 活没活它根本无从得知。
			// 这里报"失联"是瞎猜 —— 它会和 leader 视图打架，也会让运维误判。
			mv.LiveRole = "未知"
		case !peer.Reachable:
			mv.LiveRole = "失联"
		default:
			mv.LiveRole = "follower"
		}
		v.Members = append(v.Members, mv)
	}
	voters := 0
	alive := 0
	for _, mv := range v.Members {
		if mv.Role != memberVoter || mv.Status != memberActive {
			continue
		}
		voters++
		if mv.Self || mv.Reachable {
			alive++
		}
	}
	// 多数派只有 leader 算得准：lastHeard 是"收到这个 peer 的 append 应答"时
	// 打的点，只有 leader 会收到。站在 follower 上按同一套算法数，永远只数到
	// 自己那一台 —— 3 台集群里一切正常时控制台会喊"无多数派 · 只读"（lab 实测，
	// 2026-11-14 切主后每台 follower 都是这句）。
	//
	// follower 能知道的只有"有没有 leader"：leader 是多数派选出来的，它还在给
	// 我发心跳就说明多数派刚才还在。写反正会被转给 leader，由它自己那本账判。
	if v.Role == string(raftLeader) {
		v.Quorum = voters > 0 && alive*2 > voters
	} else {
		v.Quorum = v.Leader != ""
	}
	v.Degraded = s.commitErr
	if s.retired.Load() {
		// 这台的成员记录可能还写着 voter（成员表是 leader 写的，它自己只是
		// 照着旧表显示）—— 所以这一句必须压过一切别的说法。
		v.Reason = "本机已被移出集群（" + s.retiredNote + "）：已停止共识，" +
			"设备侧返回 503，进程稍后退出"
		v.Retired = true
	}
	if s.state.Cluster != nil && s.state.Cluster.ID == "" {
		v.Reason = "集群配置损坏（cluster id 为空）"
	}
	return v
}

func (s *server) handleAdminCluster(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireOperator(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		v := s.clusterViewLocked()
		s.mu.Unlock()
		writeJSON(w, v)
		return
	case http.MethodPost:
	default:
		http.Error(w, "GET or POST", 405)
		return
	}
	var body struct {
		Action   string `json:"action"` // init | token | remove | cancel
		Endpoint string `json:"endpoint"`
		Console  string `json:"console"`
		Name     string `json:"name"`
		Kind     string `json:"kind"` // add | replace
		Replace  string `json:"replace_id"`
		ID       string `json:"id"`
		Reason   string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	switch body.Action {
	case "init":
		if err := s.initCluster(body.Endpoint, body.Console, body.Name, id.Username); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s.mu.Lock()
		v := s.clusterViewLocked()
		s.mu.Unlock()
		writeJSON(w, v)

	case "token":
		tok, p, err := s.issueJoinToken(body.Kind, body.Name, body.Endpoint, body.Console, body.Replace)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]any{
			"token": tok, "expires_at": p.Expires, "member_id": p.Member,
			"endpoint": p.Endpoint, "name": p.Name, "replaces": p.Replaces,
			// 面板上直接给人抄的命令：这就是"不需要后台部署"的全部含义。
			"command": fmt.Sprintf("sr-ctrl -data /var/lib/sr-ctrl -listen 0.0.0.0:8443 "+
				"-join https://%s -join-token %s", s.leaderEndpointForCommand(), tok),
		})

	case "remove":
		if body.ID == "" {
			http.Error(w, "id required", 400)
			return
		}
		if err := s.decommissionMember(body.ID, body.Reason, true); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s.mu.Lock()
		v := s.clusterViewLocked()
		s.mu.Unlock()
		writeJSON(w, v)

	default:
		http.Error(w, "unknown action", 400)
	}
}

func (s *server) leaderEndpointForCommand() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ep := s.leaderEndpointLocked(); ep != "" {
		return ep
	}
	if len(s.tlsAddrs) > 0 {
		return s.tlsAddrs[0]
	}
	return "127.0.0.1:8443"
}

// issueJoinToken 生成一个加入码。kind=replace 时必须指定 replace_id：
// 用新机器替掉一台坏掉的，新机器只需要跑一条命令，剩下的（提升、下线旧的）
// 由 leader 的 sweep 做完。
func (s *server) issueJoinToken(kind, name, endpoint, console, replace string) (string, joinPayload, error) {
	var p joinPayload
	s.mu.Lock()
	cur := s.state.Cluster
	leaderEP := s.leaderEndpointLocked()
	s.mu.Unlock()
	if cur == nil {
		return "", p, errors.New("先开启集群，再添加控制器")
	}
	if s.raft == nil || !s.raft.IsLeader() {
		return "", p, errors.New("加入码只能在 leader 上签发（当前 leader：" + leaderEP + "）")
	}
	if endpoint == "" {
		return "", p, errors.New("请填写这台控制器对外的地址（host:port，设备与其它控制器都通过它访问）")
	}
	if console == "" {
		console = endpoint
	}
	if kind == "" {
		kind = "add"
	}
	if kind == "replace" {
		if replace == "" {
			return "", p, errors.New("替换需要指定要替换的控制器")
		}
		found := false
		for _, m := range s.membersSnapshot() {
			if m.ID == replace {
				found = true
			}
		}
		if !found {
			return "", p, errors.New("要替换的控制器不在成员表里")
		}
	}
	if name == "" {
		name = "控制器 " + fmt.Sprint(len(cur.Members)+1)
	}
	// 同一个地址只能有一个成员（见 endpointConflict 的注释）。这一步放在签发之前：
	// 让错误停在"点生成加入码"这一下，而不是等新机器跑起来、日志一路报奇怪的东西。
	if msg := endpointConflict(s.membersSnapshot(), endpoint, replace); msg != "" {
		return "", p, errors.New(msg)
	}
	p = joinPayload{
		Cluster: cur.ID, Member: newMemberID(), Name: name, Endpoint: endpoint,
		Console: console, Replaces: replace,
		Expires: time.Now().Add(joinTokenTTL).Unix(),
	}
	if caPEM, err := os.ReadFile(filepath.Join(s.dataDir, "ca.pem")); err == nil {
		p.CAPEM = string(caPEM)
	}
	sec := s.clusterSecret()
	if sec == "" {
		return "", p, errors.New("集群密钥缺失（cluster/secret），请检查数据目录")
	}
	return signJoinToken(sec, p), p, nil
}

// endpointConflict 回答"这个地址还能不能拿来加入"，返回空串表示没冲突。
//
// 为什么必须拦：两个成员指向同一个地址，多数派就算不清了。leader 会把 append
// 发给同一个进程两次，那个进程只认自己这一个成员 id，另一份永远追不上 ——
// 成员表看着像 4 台，其实只有 3 台机器，learner 卡在落后状态，sweep 也就永远
// 不会提升它，客户看到的是"加了一台，一直不变成投票成员"。
//
// 两种合理的复用要放行：
//   - 替换（replaceID 指的正是占着这个地址的旧成员）：旧机器坏了，新机器直接
//     接管它的 IP 是最常见的现场做法。
//   - 已下线的成员：行留着做审计，地址早就不占了。
func endpointConflict(members []ClusterMember, endpoint, replaceID string) string {
	ep := normEndpoint(endpoint)
	if ep == "" {
		return ""
	}
	for _, m := range members {
		if m.Status != memberActive || normEndpoint(m.Endpoint) != ep || m.ID == replaceID {
			continue
		}
		return fmt.Sprintf("地址 %s 已经是成员 %s（%s）的；换一台机器接管这个地址请用「替换」，"+
			"同一台机器重新加入请先清空它的数据目录", endpoint, m.Name, m.ID)
	}
	return ""
}

func normEndpoint(s string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "/")))
}

func newMemberID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("c-%x", time.Now().UnixNano()&0xffffffff)
	}
	return "c-" + hex.EncodeToString(b)
}

// initCluster 把一台单机控制器变成"一台机器的集群"。这是客户后来上 HA 的起点：
// 先在这里开启，再按加入码把第 2、3 台接进来。
func (s *server) initCluster(endpoint, console, name, actor string) error {
	if endpoint == "" {
		return errors.New("请填写这台控制器对外的地址（host:port）")
	}
	if _, _, err := net.SplitHostPort(endpoint); err != nil {
		return fmt.Errorf("地址要写成 host:port（例如 192.168.1.10:8443）：%w", err)
	}
	if console == "" {
		console = endpoint
	}
	if _, _, err := net.SplitHostPort(console); err != nil {
		return fmt.Errorf("控制台地址要写成 host:port：%w", err)
	}
	s.mu.Lock()
	if s.state.Cluster != nil {
		s.mu.Unlock()
		return errors.New("集群已经开启过了")
	}
	if s.raft != nil {
		s.mu.Unlock()
		return errors.New("共识已经在运行，不需要重复开启")
	}
	s.mu.Unlock()

	sec := randHex(24)
	if err := s.writeClusterSecret(sec); err != nil {
		return err
	}
	id := newMemberID()
	if err := s.writeNodeID(id); err != nil {
		return err
	}
	if name == "" {
		name = "控制器 1"
	}
	me := ClusterMember{ID: id, Name: name, Endpoint: endpoint, Console: console,
		ConsoleTLS: s.consoleTLS,
		Role:       memberVoter, Status: memberActive, JoinedAt: time.Now().Unix()}
	// 初始化不是共识写：集群还不存在，这一份成员表就是共识的起点。直接把
	// state.json 写盘，然后让 raft 从 [self] 这个配置开始选主（单 voter 的
	// 集群会立刻选出自己）。
	s.mu.Lock()
	s.state.Cluster = &ClusterConfig{ID: "cl-" + randHex(6), Generation: 1,
		Members: map[string]*ClusterMember{id: &me}}
	s.publishPeerAddrsLocked()
	err := s.store.Save(s.state)
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("保存集群配置失败: %w", err)
	}
	if err := s.startRaft(); err != nil {
		return err
	}
	if err := s.ensureServerCert([]string{endpoint}); err != nil {
		log.Printf("cluster: 重新签发服务端证书失败（设备可能连不上这台）: %v", err)
	}
	s.appendOps("cluster.init", "", 0, actor, "info",
		fmt.Sprintf("开启集群：%s（%s），等待添加更多控制器", name, endpoint))
	s.kickClusterSweep()
	return nil
}

// decommissionMember 是"下线"和"替换收尾"共用的写路径。
//
// 两条硬规则：
//  1. 不能把 voter 数量降到 0 —— 那是把集群从"能用"改成"只能读"。
//  2. 剩下的 voter 必须是奇数且 ≥3 才有 HA 意义；只剩 2 台时给出明确警告，
//     但不阻止（客户可能就是想从 4 台退到 3 台，或者临时拆一台去修）。
func (s *server) decommissionMember(memberID, note string, viaConsole bool) error {
	s.mu.Lock()
	cur := s.state.Cluster
	if cur == nil {
		s.mu.Unlock()
		return errors.New("集群还没开启")
	}
	target := cur.Members[memberID]
	if target == nil {
		s.mu.Unlock()
		return errors.New("成员不存在")
	}
	var voters int
	for _, m := range cur.Members {
		if m.Role == memberVoter && m.Status == memberActive {
			voters++
		}
	}
	s.mu.Unlock()

	if target.Status == memberDecommissioned {
		return errors.New("这台已经下线了")
	}
	if target.Role == memberVoter && target.Status == memberActive && voters <= 1 {
		return errors.New("这是最后一台有投票权的控制器，下线它等于关掉控制面")
	}
	if viaConsole && target.ID == s.nodeID {
		// 允许，但要说清楚会发生什么：这条配置提交之后本机立刻让位，
		// 浏览器会被踢到别的节点上。
		log.Printf("cluster: 本机（%s）正在被下线，提交后立即退出 leader 角色", target.ID)
	}
	if err := s.applyMembershipChange("remove", memberID, "", func(ms []ClusterMember) []ClusterMember {
		out := make([]ClusterMember, 0, len(ms))
		for _, m := range ms {
			if m.ID == memberID {
				m.Status = memberDecommissioned
				m.Role = ""
				m.Note = note
			}
			out = append(out, m)
		}
		return out
	}); err != nil {
		return err
	}
	s.appendOps("cluster.remove", "", 0, "", "warn",
		fmt.Sprintf("控制器下线：%s（%s）%s", target.Name, target.Endpoint,
			func() string {
				if note != "" {
					return "，原因：" + note
				}
				return ""
			}()))
	return nil
}

// ---------------------------------------------------------------- sweep

// clusterSweep 负责两件"等一等再发生"的事：
//   - learner 追上日志 → 提升为 voter
//   - 替换关系里的旧成员 → 新成员已经是 voter 之后下线
//
// 它必须容忍换主：新 leader 接手时看到的还是"有一个 learner"，于是接着做。
func (s *server) clusterSweep(ctx context.Context) {
	promoteStreak := map[string]int{}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.sweepKick:
		case <-t.C:
		}
		if s.raft == nil || !s.raft.IsLeader() {
			promoteStreak = map[string]int{}
			continue
		}
		members := s.membersSnapshot()
		for _, m := range members {
			if m.Status != memberActive || m.Role != memberLearner {
				continue
			}
			match, commit, _ := s.raft.matchOf(m.ID)
			if commit > 0 && match+1 >= commit {
				promoteStreak[m.ID]++
			} else {
				promoteStreak[m.ID] = 0
			}
			if promoteStreak[m.ID] < 3 {
				continue
			}
			delete(promoteStreak, m.ID)
			log.Printf("cluster: %s 已追上日志（%d/%d），提升为 voter", m.Name, match, commit)
			if err := s.applyMembershipChange("promote", m.ID, "", func(ms []ClusterMember) []ClusterMember {
				out := make([]ClusterMember, 0, len(ms))
				for _, x := range ms {
					if x.ID == m.ID {
						x.Role = memberVoter
					}
					out = append(out, x)
				}
				return out
			}); err != nil {
				log.Printf("cluster: 提升 %s 失败: %v", m.Name, err)
			} else {
				s.appendOps("cluster.join", "", 0, "", "info",
					fmt.Sprintf("控制器加入：%s（%s）", m.Name, m.Endpoint))
			}
		}
		// 替换收尾：新成员已经是 voter、旧的还挂着 → 下线旧的。
		for _, m := range members {
			if m.Replaces == "" || m.Status != memberActive || m.Role != memberVoter {
				continue
			}
			for _, old := range members {
				if old.ID == m.Replaces && old.Status == memberActive {
					log.Printf("cluster: %s 已就位，下线被替换的 %s", m.Name, old.Name)
					if err := s.decommissionMember(old.ID, "被 "+m.Name+" 替换", false); err != nil {
						log.Printf("cluster: 下线 %s 失败: %v", old.Name, err)
					}
				}
			}
		}
	}
}

func (s *server) kickClusterSweep() {
	select {
	case s.sweepKick <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------- 加入方引导

// joinCluster 在启动时执行：把本机接进一个已有集群。它做的事和"新设备入网"
// 很像 —— 拿一个一次性凭证换回身份，然后自己跑起来，不需要人去动老节点。
func (s *server) joinCluster(leaderURL, token string, version string) error {
	url := strings.TrimSuffix(leaderURL, "/")
	// 先只解析内容（不验签：密钥还没有），把里面的 CA 拿出来当这一次连接的
	// 信任根。验签在 leader 侧做，伪造的 token 换不到任何东西。
	var hint joinPayload
	if i := strings.LastIndex(token, "."); i > 0 {
		if body, err := base64.RawURLEncoding.DecodeString(token[:i]); err == nil {
			json.Unmarshal(body, &hint)
		}
	}
	var pool *x509.CertPool
	if hint.CAPEM != "" {
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(hint.CAPEM)) {
			return errors.New("加入码里的 CA 读不出来（加入码被截断了？）")
		}
	} else {
		return errors.New("加入码里没有 CA，无法安全地验证 leader；请用控制台重新生成")
	}
	body, _ := json.Marshal(joinRequest{Token: token, Version: version, ConsoleTLS: s.consoleTLS})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url+"/v1/cluster/join", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	cli := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	resp, err := cli.Do(req)
	if err != nil {
		return fmt.Errorf("连不上 %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		detail := make([]byte, 512)
		n, _ := resp.Body.Read(detail)
		return fmt.Errorf("加入被拒绝（HTTP %d）：%s", resp.StatusCode, strings.TrimSpace(string(detail[:n])))
	}
	var jr joinResponse
	if err := json.NewDecoder(resp.Body).Decode(&jr); err != nil {
		return err
	}
	// 1. 共用 CA（设备证书互认的前提）。
	if err := os.WriteFile(filepath.Join(s.dataDir, "ca.pem"), []byte(jr.CAPEM), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.dataDir, "ca.key"), []byte(jr.CAKey), 0600); err != nil {
		return err
	}
	// 2. 自己的身份与集群密钥。
	if err := s.writeNodeID(jr.MemberID); err != nil {
		return err
	}
	if err := s.writeClusterSecret(jr.Secret); err != nil {
		return err
	}
	// 3. 状态 = 快照。加入方不需要"重放全部历史"，它从 leader 的已提交点开始。
	var st State
	if err := json.Unmarshal(jr.Snapshot.State, &st); err != nil {
		return fmt.Errorf("快照解码失败: %w", err)
	}
	normalizeState(&st)
	if err := s.store.Save(&st); err != nil {
		return err
	}
	s.mu.Lock()
	s.state = &st
	// 快照就是已提交状态：基线必须按它算，否则加入之后的第一次写会把整份
	// 状态（187 KB）当成"变化"提议一遍。
	s.resetHardIndex()
	s.publishPeerAddrsLocked()
	s.mu.Unlock()
	// 换掉内存里的 CA：写盘不算数，ensureServerCert 签的是内存里那份。
	if err := s.reloadCA(); err != nil {
		return fmt.Errorf("加载集群 CA 失败: %w", err)
	}
	// 4. 自己签一张服务端证书：共用 CA 之后，SAN 必须覆盖自己的地址。
	var self ClusterMember
	for _, m := range jr.Members {
		if m.ID == jr.MemberID {
			self = m
		}
	}
	if self.Endpoint == "" {
		return errors.New("加入应答里没有本机成员记录")
	}
	if err := s.ensureServerCert([]string{self.Endpoint}); err != nil {
		return fmt.Errorf("签发服务端证书失败: %w", err)
	}
	// 5. 共识从快照点起跑。
	if err := s.startRaftAt(&jr.Snapshot); err != nil {
		return err
	}
	log.Printf("cluster: 已加入集群 %s，成员 %s（%s），起始 index=%d",
		jr.ClusterID, self.Name, self.Endpoint, jr.Snapshot.Index)
	return nil
}
