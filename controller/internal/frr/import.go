package frr

import (
	"net/netip"
	"sort"
	"sync"
)

// Importer 维护"从客户侧学到、且带着约定 tag"的前缀集合。
//
// 为什么按 tag 过滤，而不是收下所有 BGP 路由：盒内 FRR 会把 sr-frr 导出到
// table 1250 的前缀再分发进 BGP，那些路由在 zebra 里同样是 BGP 类型，会和
// 客户宣告的前缀混在同一条再分发流里。客户侧邻居的入方向 route-map 统一打
// 一个 tag，只有带这个 tag 的路由是"客户告诉我们的"；我们自己导出又转回来
// 的路由没有 tag，天然被挡掉（这也是反向导入不形成回环的原因）。
//
// 集合语义是"每个前缀的最新选中路由"：一条 ADD 通知的 tag 不对，说明这个
// 前缀眼下不归我们导入，要从集合里删掉，而不是保留旧结论。
type Importer struct {
	mu     sync.Mutex
	tag    uint32
	routes map[netip.Prefix]struct{}
}

func NewImporter(tag uint32) *Importer {
	return &Importer{tag: tag, routes: map[netip.Prefix]struct{}{}}
}

// Handle 处理一条 zebra 再分发通知。
func (im *Importer) Handle(ev RouteEvent) {
	if !ev.Prefix.IsValid() || !ev.Prefix.Addr().Is4() {
		return
	}
	p := ev.Prefix.Masked()
	im.mu.Lock()
	defer im.mu.Unlock()
	if ev.Add && ev.Tag == im.tag {
		im.routes[p] = struct{}{}
		return
	}
	// 删除通知不论 tag：zebra 带的是被撤条目的 tag，正常与订阅一致；即便
	// 不一致，这个前缀也已经不属于我们了。
	delete(im.routes, p)
}

// Reset 清空集合。zebra 重启或换连接后调用：新连接上的回放会重建全部状态，
// 旧连接留下的条目不会再有删除通知。
func (im *Importer) Reset() {
	im.mu.Lock()
	im.routes = map[netip.Prefix]struct{}{}
	im.mu.Unlock()
}

// Prefixes 返回排好序的前缀快照。
func (im *Importer) Prefixes() []netip.Prefix {
	im.mu.Lock()
	out := make([]netip.Prefix, 0, len(im.routes))
	for p := range im.routes {
		out = append(out, p)
	}
	im.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// Len 返回集合大小（日志用）。
func (im *Importer) Len() int {
	im.mu.Lock()
	defer im.mu.Unlock()
	return len(im.routes)
}
