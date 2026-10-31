package route

// Loc-RIB：BGP 里"所有 peer 送来的路径汇成一张表、每个前缀从里面选一条最优"
// 的那一步。本文件是 GoBGP internal/pkg/table 的 destination.go + table.go 换
// 成我们的 path 形状：
//
//   - 我们的 path 不带 NEXT_HOP（与真 BGP 只有两条差异：mTLS 信道、RIB→VPP 的
//     落法），一条路径的身份是 (来源会话, RouteKey)，RouteKey =
//     (来源设备, 来源 VPN, 前缀)。同一份内容可以由两条 RR 会话各送一遍，那就是
//     同一个前缀的两份拷贝 —— 正是 GoBGP 里"同一个 destination 里来自两个 peer
//     的两条 path"。
//   - destination.paths 始终按最优先排序（GoBGP 的 knownPathList 同样如此），
//     所以"最优"= 从头往后第一条可解析的，不需要一次独立的全表扫描。
//   - 摘除永远由"每个会话自己那份副本"驱动（clientSession.adjIn），绝不反过来
//     扫合并后的表找是谁断了 —— GoBGP 的 dropAdjRIBIn 就是这个形状，它保证一条
//     会话消失只会带走它自己那份拷贝，另一条会话的同一份立刻顶上。
//
// 只有一套锁：调用方持有 Client.mu。GoBGP 分 2048 个 shard 是因为它按公网全表
// 规模设计，这里的表是一台 CPE 的 VPN 前缀，分片只会多出第二套状态要维护。

// path 是某一条 RR 会话送来的某一条路由的那一份拷贝。
type path struct {
	route  Route
	source string // clientSession.addr：跨重连稳定，SetRRs 复用会话也按它匹配

	// resolvable 是 GoBGP 的 Path.IsNexthopInvalid 的同位物（取反）。我们的
	// "下一跳"不在线上，是本机隧道表解析出来的，所以这个标志由 SetTunnels 翻，
	// 对应 GoBGP 的 Nexthop Tracking（updateNexthopState）。
	//
	// 不可解析的 path **留在表里**，只是不参与选优 —— 这就是 no_path 不再是
	// "派生出来的另一张表"、而变成 path 身上一个位的原因：隧道回来了，它自己
	// 重新当选，不需要 RR 重发任何东西。
	resolvable bool
}

// destination 是一个前缀（精确到 RouteKey）上所有来源的拷贝的集合。
//
// GoBGP 在这里还带一个 localIdMap *Bitmap，那是 ADD-PATH 用来编号路径的；我们的
// 协议没有 ADD-PATH（一个 key 一条会话最多一份），所以不搬这个字段。
type destination struct {
	key   RouteKey
	paths []*path
}

func (d *destination) indexOfSource(source string) int {
	for i, p := range d.paths {
		if p.source == source {
			return i
		}
	}
	return -1
}

// best 是队首那条，且队首不可解析时就没有最优路（GoBGP 的 GetBestPath 原样：
// `p == nil || p.IsNexthopInvalid ⇒ nil`）。"不可解析的沉到队尾"由
// compareByReachableNexthop 保证，所以不需要在这里往后扫。
//
// 值接收者：changes() 要拿一次写入的前后切片各算一遍最优，那里是临时构造的
// destination 字面量，不是表里的那个。
func (d destination) best() *path {
	if len(d.paths) == 0 || !d.paths[0].resolvable {
		return nil
	}
	return d.paths[0]
}

// insert 就是 GoBGP 的 insertSort：找到第一条"新路径排在它前面"的位置插入，
// 都比不出高下就排在末尾。
func (d *destination) insert(neu *path) {
	for i, old := range d.paths {
		if ranksAhead(neu, old) {
			d.paths = slicesInsert(d.paths, i, neu)
			return
		}
	}
	d.paths = append(d.paths, neu)
}

// bestPathSteps 照抄 GoBGP destination.go 在 insertSort 里逐个调用的比较器序列
// ——顺序与名字都照它（v4 已经把 compareByWeight / compareByIGPCost 两步删掉了，
// 我们也不补回来）。
//
// 每个比较器回答"新路径该不该排在这条旧路径前面"：
//
//	decided=true ⇒ 就此定论，better 是答案；
//	decided=false ⇒ 这一步分不出，问下一个。
//
// 全部问完仍分不出，新路径排在同值路径之前（与 GoBGP 一致）。走到那一步必然
// 不同源：同源同前缀在 process 里先被摘掉了。
//
// 每一步都留着（哪怕恒分不出）是有意的：这套表以后要加属性，加的位置在这里，
// 不在读路径上另发明一步。
var bestPathSteps = []struct {
	name string
	fn   func(neu, old *path) (better, decided bool)
}{
	// compareByLLGRStaleCommunity：本系统无 LLGR（RFC 4724bis 未实现）。
	{"llgr-stale", func(neu, old *path) (bool, bool) { return false, false }},

	// compareByReachableNexthop：这一步在我们这里是真的，因为下一跳本来就不在
	// 线上、只能由本机隧道表解析。可解析的一律排在不可解析的前面（GoBGP 这里
	// 返回 path1/path2 就是定论，不往下问）。
	{"reachable-nexthop", func(neu, old *path) (bool, bool) {
		if neu.resolvable != old.resolvable {
			return neu.resolvable, true
		}
		return false, false
	}},

	// compareByLocalPref：协议不携带 LOCAL_PREF。
	{"local-pref", func(neu, old *path) (bool, bool) { return false, false }},

	// compareByLocalOrigin：本机起源的路由在入口就被 ownRoute 摘掉，进不了这张
	// 表（见 applyAnnounce），所以这一步按构造恒分不出。
	{"local-origin", func(neu, old *path) (bool, bool) { return false, false }},

	// compareByASPath / compareByOrigin / compareByMED / compareByASNumber：
	// 无 AS_PATH、无 ORIGIN、无 MED，也没有 eBGP/iBGP 之分。
	{"as-path", func(neu, old *path) (bool, bool) { return false, false }},
	{"origin", func(neu, old *path) (bool, bool) { return false, false }},
	{"med", func(neu, old *path) (bool, bool) { return false, false }},
	{"as-number", func(neu, old *path) (bool, bool) { return false, false }},

	// compareByAge（RFC 5004）：GoBGP 自己就只在两条 eBGP 路径之间比 age；CPE 与
	// RR 之间全是同一租户内部会话，按它的条件这一步恒分不出。
	// 这一条尤其要留着注释：拿 age 决胜会让"某条 RR 重连"变成一次选路变化，
	// 而那正是我们最不想要的抖动。
	{"age", func(neu, old *path) (bool, bool) { return false, false }},

	// compareByRouterID：我们的 router-id 同位物是来源设备号。同一个 destination
	// 里它恒相等（RouteKey 含 DeviceID），所以按构造分不出 —— 留着是让"下一位由
	// 谁决"这件事有个名字，不是留个死代码。
	{"router-id", func(neu, old *path) (bool, bool) {
		if neu.route.DeviceID != old.route.DeviceID {
			return neu.route.DeviceID < old.route.DeviceID, true
		}
		return false, false
	}},

	// compareByNeighborAddress：最终定胜负的一步，比来源会话地址（GoBGP 比的是
	// peer address）。用地址而不是会话在数组里的下标：SetRRs 换顺序、某条会话
	// 重连、退休会话排在末尾这些都不能让已选出的最优路改变。
	{"neighbor-address", func(neu, old *path) (bool, bool) {
		return neu.source < old.source, true
	}},
}

func ranksAhead(neu, old *path) bool {
	for _, step := range bestPathSteps {
		if better, decided := step.fn(neu, old); decided {
			return better
		}
	}
	return true
}

// ribUpdate 是一次写入对某个 destination 造成的前后快照，照 GoBGP 的
// table.Update{KnownPathList, OldKnownPathList, Changed}：写和"我改了什么"在
// 同一个临界区里出来，被顶掉的最优路和刚当选的最优路来自同一个对象。
type ribUpdate struct {
	key     RouteKey
	old     []*path
	neu     []*path
	changed bool
}

// changes 是 GoBGP 的 GetChanges：只报"最优路变了"。
//
// 同一份 path 因为 resolvable 翻转而出局/入列，在这里也算一次变化 —— GoBGP 显式
// 看 IsNexthopInvalid，即使 changed 是 false 也照样发；这里同理，changed 记的是
// 内容变化，能不能装表看的是最优路。
func (u *ribUpdate) changes() (withdrawn, advertised *path) {
	was := destination{paths: u.old}.best()
	now := destination{paths: u.neu}.best()
	if samePath(was, now) {
		return nil, nil
	}
	return was, now
}

// samePath 是"这两份拷贝对装表来说没有区别"。process 替换同源路径时会换掉指针，
// 所以比内容不能比地址（GoBGP 用 Path.Equal 同理）。
func samePath(a, b *path) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.source == b.source && a.resolvable == b.resolvable && a.route.Equal(b.route)
}

// locRib 是这一台 CPE 的 Loc-RIB：每个 RouteKey 一个 destination。
type locRib struct {
	dsts map[RouteKey]*destination

	// byOrigin 是 GoBGP 的 UpdateNHTableMap 的同位物：来源设备 → 受影响的那些
	// path。隧道表变了要翻 resolvable，靠它定点翻，不去扫全表。
	byOrigin map[uint16]map[*path]bool
}

func newLocRib() *locRib {
	return &locRib{
		dsts:     map[RouteKey]*destination{},
		byOrigin: map[uint16]map[*path]bool{},
	}
}

// process 收下某一条会话送来的某一条路由：新增，或者替换同一来源的旧的那一份
// （GoBGP 的 implicitWithdraw —— 我们没有 ADD-PATH，同源同前缀只能有一份）。
// 返回的 ribUpdate 一定非 nil。
func (r *locRib) process(rt Route, source string, resolvable bool) *ribUpdate {
	k := rt.Key()
	d := r.dsts[k]
	if d == nil {
		d = &destination{key: k}
		r.dsts[k] = d
	}
	old := append([]*path(nil), d.paths...)

	// 同源先摘掉，同时分清"替换"还是"新增"（Changed 的定义照 GoBGP）。
	var replaced *path
	if i := d.indexOfSource(source); i >= 0 {
		replaced = d.paths[i]
		r.unindex(replaced)
		d.paths = append(d.paths[:i], d.paths[i+1:]...)
	}

	neu := &path{route: rt, source: source, resolvable: resolvable}
	d.insert(neu)
	r.index(neu)

	changed := replaced == nil || !replaced.route.Equal(rt)
	return &ribUpdate{key: k, old: old, neu: append([]*path(nil), d.paths...), changed: changed}
}

// withdraw 摘掉某个来源在某个前缀上那一份拷贝（GoBGP 的 explicitWithdraw）。
// 没命中返回 nil：撤回一条本来就没有的路由是 no-op，不该伪造一次表变化。
func (r *locRib) withdraw(k RouteKey, source string) *ribUpdate {
	d := r.dsts[k]
	if d == nil {
		return nil
	}
	i := d.indexOfSource(source)
	if i < 0 {
		return nil
	}
	old := append([]*path(nil), d.paths...)
	r.unindex(d.paths[i])
	d.paths = append(d.paths[:i], d.paths[i+1:]...)
	if len(d.paths) == 0 {
		delete(r.dsts, k)
		return &ribUpdate{key: k, old: old, neu: nil, changed: true}
	}
	return &ribUpdate{key: k, old: old, neu: append([]*path(nil), d.paths...), changed: true}
}

// bestOf 报某个前缀当前的最优路；没有可解析的拷贝时为 nil。
func (r *locRib) bestOf(k RouteKey) *path {
	if d := r.dsts[k]; d != nil {
		return d.best()
	}
	return nil
}

// eachBest 按 destination 的键序遍历最优路，读路径（RIB/AllRIB）从这里出。
func (r *locRib) eachBest(fn func(key RouteKey, p *path)) {
	keys := make([]RouteKey, 0, len(r.dsts))
	for k := range r.dsts {
		keys = append(keys, k)
	}
	for _, k := range SortedKeys(keys) {
		if p := r.dsts[k].best(); p != nil {
			fn(k, p)
		}
	}
}

// eachDest 按 destination 的键序遍历每一个前缀，不分可解析与否。待决清单从这里
// 出：一条路能不能装表要在读的时候问本机两张表（见 importablePathLocked），
// 所以它必须看见那些 best 为 nil 的前缀。
func (r *locRib) eachDest(fn func(d *destination)) {
	keys := make([]RouteKey, 0, len(r.dsts))
	for k := range r.dsts {
		keys = append(keys, k)
	}
	for _, k := range SortedKeys(keys) {
		fn(r.dsts[k])
	}
}

// originPaths 给出某个来源设备的所有 path（Nexthop Tracking 翻标志位用）。
// 返回的是副本切片，调用方可以边拿边 process。
func (r *locRib) originPaths(deviceID uint16) []*path {
	set := r.byOrigin[deviceID]
	if len(set) == 0 {
		return nil
	}
	out := make([]*path, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	return out
}

func (r *locRib) len() int { return len(r.dsts) }

func (r *locRib) index(p *path) {
	set := r.byOrigin[p.route.DeviceID]
	if set == nil {
		set = map[*path]bool{}
		r.byOrigin[p.route.DeviceID] = set
	}
	set[p] = true
}

func (r *locRib) unindex(p *path) {
	set := r.byOrigin[p.route.DeviceID]
	if set == nil {
		return
	}
	delete(set, p)
	if len(set) == 0 {
		delete(r.byOrigin, p.route.DeviceID)
	}
}

func slicesInsert(s []*path, i int, p *path) []*path {
	s = append(s, nil)
	copy(s[i+1:], s[i:])
	s[i] = p
	return s
}
