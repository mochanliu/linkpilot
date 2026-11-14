package main

// cluster_runtime.go —— 把共识接到 server 上：提交、应用、证书、重定向。
//
// 这一层要解决的问题只有一个：**把"改内存"和"已提交"变成同一件事**。
//
// 现在的写路径是"持 s.mu 改 state → save()"。改造之后 save() 变成 commit()：
// 持同一把锁算出硬状态的 diff，把这几行提议给 raft，等到多数派落盘才返回。
// 于是所有既有的写处理器（发布配置、改隧道、加账号……）**一行都不用改**，
// 自动获得 RPO=0 的语义。这也是这套方案能做下去的原因：不是重写 30k 行，
// 而是在唯一的持久化出口上换一个实现。

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%016x", os.Getpid())
	}
	return hex.EncodeToString(b)
}

// resetHardIndex 把"已提交"的基线设成当前状态。启动、快照落地之后都要调一次：
// 否则第一次 commit 会把整份状态当成"变化"，等于一上线就复制 187 KB。
func (s *server) resetHardIndex() {
	rows, err := hardRowsOf(s.state)
	if err != nil {
		return
	}
	idx := make(map[string]string, len(rows))
	for _, r := range rows {
		idx[r.id()] = hardHash(r)
	}
	s.hardIndex = idx
}

// commit 提交硬状态的变化。调用方必须持 s.mu。
//
// 返回错误 = 这一批改动没有落到多数派上。调用方（handler）必须让请求失败：
// 控制台显示"已发布"而磁盘/对端没有，是切换后跑旧配置的直接原因。
func (s *server) commit() error {
	if s.hardIndex == nil {
		// 第一次提交没有基线可比：单机模式直接把整份状态写一次（保持老行为），
		// 集群模式下状态本来就已经是共识的产物，不需要再写。
		s.resetHardIndex()
		if s.raft == nil && s.store != nil {
			return s.store.Save(s.state)
		}
		return nil
	}
	rows, err := hardRowsOf(s.state)
	if err != nil {
		s.noteCommitErr(err)
		return err
	}
	// 在副本上算 diff：提交失败时不推进基线，下次提交会把同样这几行再试一次。
	next := make(map[string]string, len(s.hardIndex)+8)
	for k, v := range s.hardIndex {
		next[k] = v
	}
	changed := rowDiff(next, rows)
	if len(changed) == 0 {
		s.hardIndex = next
		return nil
	}
	if s.raft != nil {
		if err := s.raft.Propose(changed); err != nil {
			s.noteCommitErr(err)
			return err
		}
	} else if s.store != nil {
		if err := s.store.Save(s.state); err != nil {
			s.noteCommitErr(err)
			return err
		}
	}
	s.hardIndex = next
	s.commitErr = ""
	return nil
}

func (s *server) noteCommitErr(err error) {
	if err == nil {
		return
	}
	s.commitErr = err.Error()
	s.commitErrSeq++
	log.Printf("commit failed: %v", err)
}

// saveSoft 是心跳路径的出口：软状态（在线时间、自报配置、隧道读数）不落盘、
// 不进共识。它存在的意义是让"这里要不要写"变成显式选择，而不是每个 handler
// 都顺手 save() 一次。
func (s *server) saveSoft() {}

// saveIfLeader 给"读数是软的、但落在 follower 上就没意义"的路径用（心跳）。
// follower 不写：它既不能提交，也不该在本地攒一份跟 leader 不同的状态 ——
// 设备下一拍就会被 leader 提示带过去。单机模式没有这些问题，照常写。
func (s *server) saveIfLeader() {
	if s.raft != nil && !s.raft.IsLeader() {
		return
	}
	s.save()
}

// ---------------------------------------------------------------- 共识启动

func (s *server) startRaft() error {
	disk, err := openRaftDisk(filepath.Join(s.dataDir, "cluster"))
	if err != nil {
		return err
	}
	r := newRaftNode(s.nodeID, &httpTransport{s: s}, disk)
	s.wireRaft(r)
	if err := r.recover(); err != nil {
		return fmt.Errorf("cluster: 读取共识状态失败: %w", err)
	}
	s.mu.Lock()
	if s.state.Cluster != nil {
		r.voters = votersOf(s.state.Cluster.sorted())
		r.learners = learnersOf(s.state.Cluster.sorted())
		r.cluster = s.state.Cluster.ID
	}
	s.publishPeerAddrsLocked()
	s.raft = r
	s.raftDisk = disk
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Unlock()
	go r.run(ctx)
	return nil
}

// startRaftAt 给加入方用：从 leader 的快照点起跑，而不是从 0 开始重放历史。
func (s *server) startRaftAt(snap *raftSnapshot) error {
	disk, err := openRaftDisk(filepath.Join(s.dataDir, "cluster"))
	if err != nil {
		return err
	}
	r := newRaftNode(s.nodeID, &httpTransport{s: s}, disk)
	s.wireRaft(r)
	s.mu.Lock()
	s.raft = r
	s.raftDisk = disk
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Unlock()
	if err := r.InstallSnapshot(snap, snap.State); err != nil {
		return err
	}
	go r.run(ctx)
	return nil
}

func (s *server) wireRaft(r *raftNode) {
	r.onRetire = s.retireSelf
	r.onOps = s.writeOpsLocal
	r.liveness = s.livenessSnapshot
	r.onLive = s.enqueueLiveness
	r.applyRows = func(rows []HardRow) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := applyRows(s.state, rows); err != nil {
			return err
		}
		s.noteApplied()
		return s.store.Save(s.state)
	}
	r.applyMembers = func(ms []ClusterMember) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.state.Cluster == nil {
			s.state.Cluster = &ClusterConfig{ID: r.cluster, Members: map[string]*ClusterMember{}}
		}
		s.state.Cluster.Generation++
		m := make(map[string]*ClusterMember, len(ms))
		for i := range ms {
			cp := ms[i]
			m[cp.ID] = &cp
		}
		s.state.Cluster.Members = m
		// 地址表变了：设备下一拍心跳就能拿到新列表，不需要谁去改配置；
		// 节点间复制也要立刻按新表发（别让新成员等下一次重启）。
		s.tlsAddrs = s.state.Cluster.activeEndpoints()
		s.publishPeerAddrsLocked()
		s.noteApplied()
		return s.store.Save(s.state)
	}
	r.applyState = func(b json.RawMessage) error {
		var st State
		if err := json.Unmarshal(b, &st); err != nil {
			return err
		}
		normalizeState(&st)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.state = &st
		s.resetHardIndex()
		if st.Cluster != nil {
			s.tlsAddrs = st.Cluster.activeEndpoints()
		}
		s.publishPeerAddrsLocked()
		s.noteApplied()
		return s.store.Save(&st)
	}
}

// ---------------------------------------------------------------- 被下线

// retireDelay 是"知道自己被移出成员表"到"进程退出"之间的时间。留一点余量让
// 日志和控制台那一帧先出去；设备心跳是 5s 一拍，10s 够它们全部换走。
var retireDelay = 10 * time.Second

// retireSelf 在本机被移出成员表的那一刻执行一次，做三件事：
//
//  1. 停共识 —— 它已经没有票，也没有人会再给它发日志；
//  2. 设备侧立刻 503：设备按地址表换到别的节点，下一拍就走（回 200 会让它
//     一直留在一台已经不该服务的机器上 —— lab 里正是这样，一台被下线的节点
//     照样收着四台 CPE 的心跳，谁都没发现）；
//  3. 过一会儿自己退出：现场看到的是"进程停了"，不是一台还在监听、看起来
//     一切正常的幽灵节点。-retire-exit=false 可以只做前两件事。
//
// 为什么由它自己做、而不是让操作员去 kill：控制台上的"下线"是一个动作，
// 不是"发一条通知，然后记得去某台机器上停进程"。
func (s *server) retireSelf(reason string) {
	if !s.retired.CompareAndSwap(false, true) {
		return
	}
	// reason 要在锁里写：控制台的视图读它（clusterViewLocked 持 s.mu）。
	s.mu.Lock()
	s.retiredNote = reason
	s.mu.Unlock()
	if s.raft != nil {
		// 先停共识：它会顺带把 role/leader 清掉，控制台那一帧立刻变得诚实。
		s.raft.Stop()
	}
	if s.admin != nil {
		s.admin.publish("cluster.retired", "", nil)
	}
	if !s.retireExit {
		log.Printf("cluster: 本机已被移出集群（%s）：已停止共识，设备侧改为 503；"+
			"进程保留（-retire-exit=false），不会再参与任何集群动作", reason)
		return
	}
	log.Printf("cluster: 本机已被移出集群（%s）：已停止共识，设备侧改为 503，%s 后进程退出",
		reason, retireDelay)
	go func() {
		select {
		case <-time.After(retireDelay):
		case <-s.ctx.Done():
			return
		}
		log.Printf("cluster: 已被移出集群，进程退出。要重新加入请清空数据目录后换新加入码")
		os.Exit(0)
	}()
}

// deviceGuard 让"本机已被下线"对设备侧立刻可见。用 503 而不是 200：
// 代理/地址簿的规则是"失败就试下一个地址"（cmd/sr-agent/addrs.go），
// 设备一拍就能换走。
func (s *server) deviceGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.retired.Load() {
			next.ServeHTTP(w, r)
			return
		}
		// 节点间 RPC 照旧放行：新的 leader 可能还在给"被替换的那台"补日志，
		// 而且控制台要能读到它自己的状态。
		if strings.HasPrefix(r.URL.Path, "/v1/cluster/") {
			next.ServeHTTP(w, r)
			return
		}
		if ep := s.memberLeaderEndpoint(); ep != "" {
			w.Header().Set("X-Cluster-Leader", ep)
		}
		http.Error(w, "this controller has been decommissioned; use another endpoint",
			http.StatusServiceUnavailable)
	})
}

func (s *server) memberLeaderEndpoint() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leaderEndpointLocked()
}

// noteApplied 是"对端刚应用了一批行"的通知点：控制台开着的页面会在下一次
// 轮询（或 SSE）里看到新数据。这里只做一次非阻塞广播。
func (s *server) noteApplied() {
	s.admin.publish("state.applied", "", nil)
}

// ---------------------------------------------------------------- 活跃度搭车

// livenessSnapshot 是 leader 侧的搭车载荷：设备最近一次心跳的时间表。
//
// **必须无锁**：sendAppend 在复制热路径上调它，而写路径 commit() 持着 s.mu
// 等多数派（最长 3s）—— 热路径在这里等锁，500ms 的复制 ctx 会整段烧掉，
// follower 收不到心跳就发起选举，集群进入风暴（2026-11-14 容器 smoke 实测，
// term 一路爬到 64）。快照由 ttlSweeper 每 2s 在持锁状态下重建，这里只读
// 一个 atomic pointer。
func (s *server) livenessSnapshot() map[string]int64 {
	if m := s.liveSnap.Load(); m != nil {
		return *m
	}
	return nil
}

// rebuildLivenessLocked 在持锁状态下重建快照。调用点：ttlSweeper（本来就
// 持着 s.mu，每 2s 一次，设备规模下是几十个键）。
func (s *server) rebuildLivenessLocked() {
	m := make(map[string]int64, len(s.state.Nodes))
	for _, rec := range s.state.Nodes {
		if rec.LastSeen > 0 {
			m[key(rec.TenantID, rec.NodeID)] = rec.LastSeen
		}
	}
	s.liveSnap.Store(&m)
}

// enqueueLiveness 是 follower 侧的落点：handleAppend 的**应答路径**只做一次
// 非阻塞投递。真正合并进本机软状态（要拿 s.mu）由 liveApplier 协程完成 ——
// append 应答晚到一拍，leader 的复制 ctx 就可能超时，所以队列满直接丢：
// 下一次 append 还会带新的表，丢一拍显示数据毫无代价。
func (s *server) enqueueLiveness(live map[string]int64) {
	if len(live) == 0 || s.liveCh == nil {
		return
	}
	select {
	case s.liveCh <- live:
	default:
	}
}

// liveApplier 把搭车来的活跃度表并进本机内存。只动 LastSeen（和过期的
// Stale），不落盘、不进共识 —— 这份表的真身一直在 leader 上，切换后 5 秒内
// 设备自己会把新的报上来。时间戳按大的算：append 可能并发乱序到达，谁新信谁。
func (s *server) liveApplier(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case live := <-s.liveCh:
			now := time.Now().Unix()
			s.mu.Lock()
			for k, ts := range live {
				rec := s.state.Nodes[k]
				if rec == nil || ts <= rec.LastSeen {
					continue
				}
				rec.LastSeen = ts
				// 换主前的旧账：本机当过 leader 时可能把这台标了失联。新的
				// 时间戳说明设备还活着，把标志翻回来（台账/告警的正式恢复
				// 仍由 leader 记录并复制过来）。
				if rec.Stale && now-ts <= int64(s.staleTTL()) {
					rec.Stale = false
				}
			}
			s.mu.Unlock()
		}
	}
}

// ---------------------------------------------------------------- 证书

// ensureServerCert 重新签发本节点的服务端证书，SAN 覆盖给定地址。
//
// 为什么必须重签：节点之间用共享 CA 互相信任，而每台机器的证书只对**它自己
// 的地址**有效。加入集群时如果沿用加入前的证书（那还是本机自签 CA 发的），
// 对端校验直接失败，症状是"成员在表里但永远连不上"。
func (s *server) ensureServerCert(sans []string) error {
	s.mu.Lock()
	ca, caKey := s.caCert, s.caKey
	s.mu.Unlock()
	if ca == nil || caKey == nil {
		return errors.New("CA 还没加载")
	}
	list := append([]string{"localhost", "127.0.0.1", "::1"}, sans...)
	if err := writeServerCert(s.dataDir, ca, caKey, list); err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(s.dataDir, "server.pem"),
		filepath.Join(s.dataDir, "server.key"))
	if err != nil {
		return err
	}
	s.cert.Store(&cert)
	return nil
}

// writeServerCert 覆盖写服务端证书（loadOrCreateServerCert 只在文件不存在时
// 生成，集群成员变了必须重签）。
func writeServerCert(dir string, ca *x509.Certificate, caKey ed25519.PrivateKey, sans []string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	der, err := signWithCA(ca, caKey, pub, sans)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "server.pem"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		return err
	}
	kb, _ := x509.MarshalPKCS8PrivateKey(priv)
	return os.WriteFile(filepath.Join(dir, "server.key"),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0600)
}

// signWithCA 用集群 CA 给一把公钥签控制器服务端证书，返回证书 DER。SAN 覆盖
// 给定地址（host:port 只取主机部分），外加回环 —— 与 loadOrCreateServerCert
// 的模板保持一致，两台机器签出来的证书互认。
func signWithCA(ca *x509.Certificate, caKey ed25519.PrivateKey, pub ed25519.PublicKey, sans []string) ([]byte, error) {
	tmpl := &x509.Certificate{
		SerialNumber: newSerial(),
		Subject:      pkix.Name{CommonName: "srou-controller"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(2, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, san := range sans {
		// host:port 形式：证书里只写主机部分。
		if h, _, err := net.SplitHostPort(san); err == nil {
			san = h
		}
		if ip := net.ParseIP(san); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			continue
		}
		tmpl.DNSNames = append(tmpl.DNSNames, san)
	}
	return x509.CreateCertificate(rand.Reader, tmpl, ca, pub, caKey)
}

// reloadCA 在加入集群之后重新读一次 CA：加入方原本用的是自己生成的 CA，
// 换掉之后设备证书才对得上。
//
// ca.key 可能还没有（新流程里它由 leader 在提升为 voter 之前经集群内部通道
// 补送，见 clusterSweep 的 pushCAKey）：那就只装证书。签发动作（设备入网、
// RR 证书、服务端证书重签）在这台机器上是不可用的，但它们要么走 leader，
// 要么等 key 到位 —— 控制台和设备侧照常工作。
func (s *server) reloadCA() error {
	ca, key, pool, err := loadClusterCA(s.dataDir)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.caCert, s.caKey, s.caPool = ca, key, pool
	s.mu.Unlock()
	// 节点间客户端缓存了旧根，换 CA 必须换客户端（见 clusterHTTP 的注释）。
	s.clusterCli.Store(newClusterClient(pool))
	return nil
}

// ---------------------------------------------------------------- 重定向

// redirectToLeader 把写请求转到 leader。返回 true 表示已经写回了重定向。
//
// 307 而不是 302：307 保留 body 和 method，Go 的 http.Client 会自动重放
// （bytes.Reader 会自动带 GetBody）。设备端不需要任何改动就能跟随。
func (s *server) redirectToLeader(w http.ResponseWriter, r *http.Request) bool {
	if s.raft == nil {
		return false
	}
	if s.raft.IsLeader() {
		return false
	}
	s.mu.Lock()
	leaderID := ""
	if s.state.Cluster != nil {
		leaderID = s.raft.Leader()
	}
	var target string
	var targetTLS bool
	if leaderID != "" && s.state.Cluster != nil {
		if m := s.state.Cluster.Members[leaderID]; m != nil {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				target = m.Console
				if target == "" {
					target = m.Endpoint
				}
				targetTLS = m.ConsoleTLS
			} else {
				target = m.Endpoint
				targetTLS = true
			}
		}
	}
	s.mu.Unlock()
	if target == "" {
		http.Error(w, "这台不是 leader，而且集群暂时没有 leader（可能已失去多数派）；请在恢复多数派后重试",
			http.StatusServiceUnavailable)
		return true
	}
	// scheme 必须跟着**目标**走，不能跟着这次请求走：同一台机器上 TLS 控制台
	// 和明文回环口是并存的，从明文口进来的写请求一样要 307 到 leader 的 https。
	scheme := "https://"
	if strings.HasPrefix(r.URL.Path, "/api/") && !targetTLS {
		scheme = "http://"
	}
	// 会话不跨节点：把"这个请求是谁"签一张 60 秒的断言带上，否则 leader 只会
	// 看到一个它没见过的 cookie，回 401（lab 实测，见 cluster_session.go）。
	if a := s.signSession(s.session(r)); a != "" {
		w.Header().Set(clusterSessionHeader, a)
	}
	u := scheme + target + r.URL.RequestURI()
	log.Printf("cluster: %s %s 不在 leader 上，307 → %s", r.Method, r.URL.Path, u)
	http.Redirect(w, r, u, http.StatusTemporaryRedirect)
	return true
}

// ---------------------------------------------------------------- 写转发

// leaderProxyMaxBody 是能转发的请求体上限。配置表整份上传是这里最大的合法
// 请求（CSV 走 base64 塞在 JSON 里），几 MB；留 16 MB 有余量，也挡住"谁把
// 一个 G 的 body 打到 follower 上"。
const leaderProxyMaxBody = 16 << 20

// leaderProxyClient 专门用来把控制台写请求转给 leader。
//
// 控制台证书是每台自签的（和集群 CA 不是一条链），所以这里不做链校验。这是
// 集群内部的回环流量，且断言本身有 HMAC：能读到的只有成员。**待办**：把控制台
// 证书也纳入集群 CA，或按成员钉指纹，就没有这条 InsecureSkipVerify 了。
var leaderProxyClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12},
	},
}

// proxyToLeader 把这一次写请求原样转给 leader，并把应答原样带回来。
//
// 返回 true 表示"已经答完了"。返回 false 只有两种情况：我就是 leader，或者
// 这个进程还没进集群 —— 那时照旧走本地处理器。
func (s *server) proxyToLeader(w http.ResponseWriter, r *http.Request) bool {
	if s.raft == nil || s.raft.IsLeader() {
		return false
	}
	// 两台对"谁是 leader"的判断不一致时，转来转去会打成死循环。见第二跳就拒。
	if r.Header.Get("X-Cluster-Hop") != "" {
		http.Error(w, "写请求在控制器之间来回转：两台对谁是 leader 的判断不一致，请到集群页确认成员状态", http.StatusServiceUnavailable)
		return true
	}
	s.mu.Lock()
	leaderID := ""
	if s.state.Cluster != nil {
		leaderID = s.raft.Leader()
	}
	var target string
	var targetTLS bool
	if leaderID != "" && s.state.Cluster != nil {
		if m := s.state.Cluster.Members[leaderID]; m != nil {
			target, targetTLS = m.Console, m.ConsoleTLS
			if target == "" {
				target, targetTLS = m.Endpoint, true
			}
		}
	}
	s.mu.Unlock()
	if target == "" {
		http.Error(w, "这台不是 leader，而且集群暂时没有 leader（可能已失去多数派）；请在恢复多数派后重试",
			http.StatusServiceUnavailable)
		return true
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, leaderProxyMaxBody+1))
	if err != nil {
		http.Error(w, "读取请求体失败："+err.Error(), http.StatusBadRequest)
		return true
	}
	if len(body) > leaderProxyMaxBody {
		http.Error(w, "请求体超过 16 MB，拒绝转发", http.StatusRequestEntityTooLarge)
		return true
	}
	scheme := "https://"
	if !targetTLS {
		scheme = "http://"
	}
	u := scheme + target + r.URL.RequestURI()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.Method, u, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "构造转发请求失败："+err.Error(), http.StatusInternalServerError)
		return true
	}
	req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	req.Header.Set("X-Cluster-Hop", "1")
	if a := s.signSession(s.session(r)); a != "" {
		req.Header.Set(clusterSessionHeader, a)
	}
	resp, err := leaderProxyClient.Do(req)
	if err != nil {
		http.Error(w, "leader（"+target+"）没有应答："+err.Error(), http.StatusBadGateway)
		return true
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		// Set-Cookie 不能透传：那是 leader 域名下的会话 id，本机不认，
		// 透传只会把浏览器手里这张好用的 cookie 覆盖掉。
		if strings.EqualFold(k, "Set-Cookie") || strings.EqualFold(k, "Connection") {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, leaderProxyMaxBody))
	// 打上"leader 已经记过台账"的标记（auditMiddleware 会检查它）：这次写
	// 真正执行的地方是 leader，它的 auditMiddleware 会落一条台账并复制到
	// 全体成员。本机再记一条，任何一台控制台上都会出现两行重复。
	r.Header.Set(xClusterForwarded, "1")
	return true
}

// ---------------------------------------------------------------- 提交守卫

// bufWriter 先把响应攒起来，等确认这次请求的写已经提交再发出去。
// 没有它就只能"先回 200、后失败"，而那正是 HA 里最贵的错误。
type bufWriter struct {
	header http.Header
	body   []byte
	code   int
}

func newBufWriter() *bufWriter { return &bufWriter{header: http.Header{}, code: 0} }

func (b *bufWriter) Header() http.Header { return b.header }
func (b *bufWriter) WriteHeader(c int) {
	if b.code == 0 {
		b.code = c
	}
}
func (b *bufWriter) Write(p []byte) (int, error) {
	if b.code == 0 {
		b.code = 200
	}
	b.body = append(b.body, p...)
	return len(p), nil
}

func (b *bufWriter) flush(w http.ResponseWriter) {
	for k, vs := range b.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	code := b.code
	if code == 0 {
		code = 200
	}
	w.WriteHeader(code)
	w.Write(b.body)
}

// commitGuard 把"这一次请求里提交失败"变成 500。
//
// 判断依据是 commitErr 的变化：请求开始前记下当时的错误，请求结束后如果
// 出现了新的错误、而处理器还打算回 2xx，就把这条改成 500 并说清楚。
func (s *server) commitGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead ||
			!strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		s.mu.Lock()
		before := s.commitErrSeq
		s.mu.Unlock()
		bw := newBufWriter()
		next.ServeHTTP(bw, r)
		s.mu.Lock()
		after := s.commitErrSeq
		msg := s.commitErr
		s.mu.Unlock()
		if after != before && bw.code < 400 {
			writeJSONStatus(bw, 500, map[string]string{
				"error": "改动没有提交成功：" + msg + "。请确认集群有多数派后重试。",
			})
		}
		bw.flush(w)
	})
}

// leaderGuard 把控制台的写请求交给 leader。
//
// 读留在本地：follower 的状态可能落后几百毫秒，但一次刷新就追上了；而写必须
// 只有一个入口，否则两台机器会各自写出一份"客户以为已经生效"的配置。
//
// 为什么是**服务端代理**而不是 307：控制台是个 SPA，页面用 fetch 交 JSON。
// 浏览器跟 307 的时候，跨源（同机不同端口）的 fetch 默认不带 cookie，而会话
// 又是每个节点各自内存里的 —— 结果就是"在 follower 上点发布 → 跳到 leader →
// 401"（2026-11-14 lab 实测）。服务端转发没有这一层：浏览器始终跟发起请求的
// 那台说话，身份由本机会话决定，再签给 leader 验。
func (s *server) leaderGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead &&
			strings.HasPrefix(r.URL.Path, "/api/") && !nodeLocalPath(r.URL.Path) {
			if s.proxyToLeader(w, r) {
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// nodeLocalPath 是"只和本进程有关"的接口：登录、登出、当前会话、改口令。
//
// 它们绝不能跟着 leader 走。会话是每个节点各自内存里的一张表，把 follower 上的
// 登录 307 到 leader，浏览器拿到的是一个跨站重定向：Set-Cookie 落在另一个源上，
// 页面自己那个源仍然没有会话 —— 现象是"在 follower 上永远登不进去"，而错误信息
// 只有一句 401。写请求才需要唯一的入口，登录不需要。
func nodeLocalPath(p string) bool {
	switch p {
	case "/api/login", "/api/logout", "/api/session", "/api/password":
		return true
	}
	return false
}

// loadLocalClusterIdentity 在启动时把"我是谁"捡回来。
//
// 单机模式下它只生成一个本地 id（控制台开启集群时用得上）；集群模式下它必须
// 和成员表里的一条对上，否则这台机器会以为自己是新节点，而旧的那一票还挂着。
func (s *server) loadLocalClusterIdentity(name string) {
	b, err := os.ReadFile(filepath.Join(s.dataDir, "cluster", "node-id"))
	if err == nil && strings.TrimSpace(string(b)) != "" {
		s.nodeID = strings.TrimSpace(string(b))
	}
	s.mu.Lock()
	cur := s.state.Cluster
	s.mu.Unlock()
	if s.nodeID != "" {
		return
	}
	if cur != nil {
		// 有成员表但没有 id 文件：只可能是单成员集群（init 失败过一次），
		// 认领它；多成员时不能猜，猜错就是两台机器共用一个身份。
		if len(cur.Members) == 1 {
			for id := range cur.Members {
				s.nodeID = id
				os.MkdirAll(filepath.Join(s.dataDir, "cluster"), 0700)
				os.WriteFile(filepath.Join(s.dataDir, "cluster", "node-id"), []byte(id), 0600)
			}
		}
		return
	}
	if s.nodeID == "" {
		s.nodeID = newMemberID()
	}
}
