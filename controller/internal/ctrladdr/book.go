// Package ctrladdr 是"控制面在哪"的唯一答案。
//
// 集群之后控制器的地址不是常量：leader 会漂、节点会加会减、节点会换 IP。
// 而设备与 RR 只被给一个种子（激活时那条命令 / RR 配置里的 controller.addr），
// 所以这里把两件事分开：
//
//   - 种子 seed：配置里写的那一个地址，永不丢。就算某次应答的成员表里没有它
//     （换机、节点被下线），它也还在候选里——否则"表里没有"就等于"永远回不到
//     起点"，而且现场只看得到 connection refused。
//   - 候选 list：每次应答带回来的全表，当前在用的那个排第一。
//
// 落盘是可选的，但强烈建议开：设备重启后先试上次成功的那个，而不是回到一个
// 可能已经下线的种子。没有落盘时，一次重启就能把一台本来正常的设备卡死在死
// 地址上；这不是"装机时种子挂了怎么办"，而是"重启就掉线"。
//
// 两条规则（和集群切换语义绑定，不是性能优化）：
//  1. leader 变了 → 直接切过去，不等失败。写只在 leader 上成立，跟着它走少一次
//     307 往返；读则谁都能答。
//  2. 连不上 → 立刻试下一个，**不退避**。退避留给"整张表都试过一轮"，否则热切换
//     名义上自动、实际要等一个周期。
package ctrladdr

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// fileVersion 是落盘格式的版本。读到不认识的版本就当作没有落盘：候选表是
// 缓存，宁可从种子重来，也不要按错误的语义解释旧文件。
const fileVersion = 1

// Book 是一份线程安全的控制面候选表。零值不可用，用 New 或 Load。
type Book struct {
	mu    sync.Mutex
	seed  string          // 规范化后的种子，永不丢
	list  []string        // 候选，list[0] 是建议先试的
	cur   string          // 当前在用的
	tried map[string]bool // 这一轮已经试过的

	path      string // 落盘路径，空 = 不落盘
	lastSaved string // 上次写盘的内容，用来跳过无变化的写
	saveErr   error  // 上次写盘的错误（Save 会返回它，便于调用方记录一次）
}

// persisted 是落盘形状。seed 一起存，为的是识别"配置里的种子换了"。
type persisted struct {
	Version int      `json:"version"`
	Seed    string   `json:"seed"`
	Current string   `json:"current"`
	List    []string `json:"list"`
}

// New 用种子建一个不落盘的候选表。
func New(seed string) *Book {
	seed = Norm(seed)
	return &Book{
		seed:  seed,
		list:  []string{seed},
		cur:   seed,
		tried: map[string]bool{},
	}
}

// Load 读落盘状态；path 为空或文件不存在都退化成 New(seed)。
//
// 返回的 error 只表示"落盘读不了"，Book 本身总是可用的：候选表是缓存，
// 读不出来就从种子重来，不该让一个进程因为缓存坏了而拒绝启动。
func Load(seed, path string) (*Book, error) {
	b := New(seed)
	b.path = path
	if path == "" {
		return b, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return b, nil
		}
		return b, fmt.Errorf("读 %s: %w", path, err)
	}
	var p persisted
	if err := json.Unmarshal(raw, &p); err != nil {
		return b, fmt.Errorf("解析 %s: %w", path, err)
	}
	if p.Version != fileVersion {
		return b, fmt.Errorf("%s 的版本是 %d，不是 %d；从种子重来", path, p.Version, fileVersion)
	}
	if Norm(p.Seed) != b.seed {
		// 种子被改过 = 运维把设备/RR 指到了别处。旧候选属于旧集群，留着只会
		// 让下一次故障多试几个必然失败的地址。
		return b, nil
	}
	list := make([]string, 0, len(p.List)+2)
	seen := map[string]bool{}
	push := func(u string) {
		u = Norm(u)
		if u == "" || wildcard(u) || seen[u] {
			return
		}
		seen[u] = true
		list = append(list, u)
	}
	push(p.Current)
	push(b.seed)
	for _, u := range p.List {
		push(u)
	}
	if len(list) == 0 {
		return b, nil
	}
	b.list = list
	b.lastSaved = string(raw)
	if p.Current = Norm(p.Current); p.Current != "" && seen[p.Current] {
		b.cur = p.Current
	} else {
		b.cur = list[0]
	}
	return b, nil
}

// Norm 把 "host:port" / "https://host:port/" 统一成 "https://host:port"。
func Norm(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, "https://") && !strings.HasPrefix(s, "http://") {
		s = "https://" + s
	}
	return strings.TrimSuffix(s, "/")
}

// wildcard 判断这个 URL 的主机部分是不是"绑所有地址"的通配值。这种地址出现在
// 候选表里只会让客户端去连自己（单机 -listen 0.0.0.0 曾经就是这么下发的）。
func wildcard(u string) bool {
	uu, err := url.Parse(u)
	if err != nil {
		return true
	}
	switch uu.Hostname() {
	case "", "0.0.0.0", "::", "[::]":
		return true
	}
	return false
}

// Seed 是配置里给的那个地址。
func (b *Book) Seed() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seed
}

// Current 是现在该连的地址。
func (b *Book) Current() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cur
}

// List 是候选表的副本（日志与测试用）。
func (b *Book) List() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string{}, b.list...)
}

// Size 是候选数量（日志用）。
func (b *Book) Size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.list)
}

// Learn 吸收一次应答里的地址表，返回"应该立刻切过去"的地址（空 = 不动）。
//
// 种子永远进列表，就算 addrs 里没有它；通配地址永远不进列表。
//
// 控制器列表非空时，leader 必须是列表里的一员：不在列表里的 leader 说明对端给了
// 两份互相矛盾的信息（旧成员表的 leader + 新的地址表），照它切就是往一个连它自己
// 都没列出来的地址上撞——部署当场就是这么转圈的（RR 每轮切到旧 Mac 的地址，再被
// 下一轮拉回来）。列表为空时（老控制器）仍然接受 leader，保持向后兼容。
func (b *Book) Learn(addrs []string, leader string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	merged := make([]string, 0, len(addrs)+3)
	seen := map[string]bool{}
	push := func(u string) {
		u = Norm(u)
		if u == "" || wildcard(u) || seen[u] {
			return
		}
		seen[u] = true
		merged = append(merged, u)
	}
	push(b.cur)
	push(b.seed)
	for _, a := range addrs {
		push(a)
	}
	want := Norm(leader)
	if want != "" && (len(addrs) == 0 || inList(addrs, want)) {
		push(want)
	}
	if len(merged) == 0 {
		// 应答里一个可用地址都没有：保留原表，别把候选清空。
		return ""
	}
	b.list = merged
	if want == "" || want == b.cur || !seen[want] {
		return ""
	}
	b.cur = want
	b.tried = map[string]bool{}
	// 表头是"下一次先试的"，leader 就在那儿：重启落盘时也按这个顺序恢复。
	b.list = front(merged, want)
	return want
}

// inList 判断 u（已规范化）在不在 addrs 里。
func inList(addrs []string, u string) bool {
	for _, a := range addrs {
		if Norm(a) == u {
			return true
		}
	}
	return false
}

// front 把 u 挪到列表第一个（u 一定在列表里）。
func front(list []string, u string) []string {
	out := make([]string, 0, len(list))
	out = append(out, u)
	for _, v := range list {
		if v != u {
			out = append(out, v)
		}
	}
	return out
}

// Next 给出下一个要试的地址。返回 false = 这一轮全试过了（该退避了）。
//
// 调用 Next 的前提是"现在这个地址已经失败了"，所以它先把当前地址记进 tried：
// 否则一轮会绕回来再试一次那个刚失败的地址（表头是它的时候尤其明显），
// 白等一个超时。
func (b *Book) Next() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cur != "" {
		b.tried[b.cur] = true
	}
	for _, u := range b.list {
		if b.tried[u] {
			continue
		}
		b.tried[u] = true
		b.cur = u
		return u, true
	}
	b.tried = map[string]bool{}
	return "", false
}

// OK 在成功之后清空"试过"的集合：下一轮故障从零开始数。
func (b *Book) OK() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tried = map[string]bool{}
}

// Save 把当前候选与在用地址写盘（原子替换）。内容没变就不写——心跳每 5 秒
// 一次，无变化还写就是拿设备的 flash 换零收益。
//
// 写失败不改变内存状态：下一轮心跳还会 Save，磁盘问题不该让心跳路径失败。
func (b *Book) Save() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.path == "" {
		return nil
	}
	raw, err := json.Marshal(persisted{
		Version: fileVersion,
		Seed:    b.seed,
		Current: b.cur,
		List:    append([]string{}, b.list...),
	})
	if err != nil {
		return err
	}
	if string(raw) == b.lastSaved {
		return b.saveErr
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		b.saveErr = err
		return err
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		b.saveErr = err
		return err
	}
	if err := os.Rename(tmp, b.path); err != nil {
		b.saveErr = err
		return err
	}
	b.lastSaved = string(raw)
	b.saveErr = nil
	return nil
}
