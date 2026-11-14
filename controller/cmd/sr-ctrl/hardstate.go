package main

// hardstate.go —— 把 State 切成"硬的"和"软的"，并把硬的那半压成可复制的行。
//
// 为什么需要这一层（docs/controller-ha-plan.md §5）：
//
//	现在心跳每拍都 store.Save() 整份状态（线上 187 KB / 4 台设备 / 每 5 秒）。
//	多机 HA 之后，这条路径会变成"每个心跳都要多数派落盘"，写放大直接把共识打
//	死。而心跳里真正变的东西（LastSeen / 设备自报的配置 / 隧道读数 / 接口清单）
//	全都是"此刻的事实"，重启或切换后 5 秒内设备自己会再报一遍 —— 它们不该进
//	共识，也不该落盘。
//
// 所以这里做两件事：
//
//  1. 【分层】NodeRecord 的硬字段与软字段分开投影。硬字段（身份、端口、归属）
//     进复制与落盘；软字段（心跳读数）只活在内存里，重启后等设备自己上报。
//  2. 【按行】硬状态拆成 HardRow：一个 map 元素一行，一个标量字段一行。发布一次
//     配置只复制那几行，而不是 107 KB 的整份 configs。
//
// 行是"绝对值"而不是"增量"：重复应用同一行结果不变。这让重放、重试、快照恢复
// 都不需要额外的幂等设计 —— 网络重传和崩溃恢复都只是"再应用一次"。
//
// Cluster 字段被排除在行复制之外：它由集群配置项（config entry）唯一拥有。
// 两个写者写同一块状态，必然会在某个时刻互相覆盖。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"sdwan/controller/internal/wire"
)

// HardRow 是硬状态的最小复制单位。Table 是 State 的字段名，Key 是 map 键
// （标量字段为空），Data 是该行序列化后的值；Data 为 nil 表示"这一行被删了"。
//
// 用字段名而不是数据库表名，是因为 State 本身就是那张表：加了新字段不用改这里，
// 只要它不是软的。
type HardRow struct {
	Table string          `json:"t"`
	Key   string          `json:"k,omitempty"`
	Data  json.RawMessage `json:"d,omitempty"`
}

// id 是行在哈希索引里的键。用 NUL 分隔，避免 "Nodes|a|1" 与 "Nodes|a" 撞车。
func (r HardRow) id() string { return r.Table + "\x00" + r.Key }

// deleted 说明这一行是墓碑（发给对端 = 从它的 map 里删掉这个键）。
func (r HardRow) deleted() bool { return r.Data == nil }

// nodeHard 是 NodeRecord 里"必须一致、必须落盘"的那一半。
//
// 判断标准只有一条：重启后没人会再告诉控制器的事实。身份（serial/公钥/证书）、
// 归属（租户/node_id/站点）、端口（池里分配出去的那两个）属于这一半；
// LastSeen、设备自报的配置、接口清单、隧道读数、NAT 映射属于另一半 —— 它们
// 5 秒后就会被设备自己重新报上来，而且切换后显示"未知"比显示一个旧值更诚实。
type nodeHard struct {
	TenantID    string `json:"tenant_id"`
	NodeID      uint16 `json:"node_id"`
	Serial      string `json:"serial"`
	PublicKey   string `json:"public_key"`
	CertPEM     string `json:"cert_pem"`
	AgentVer    string `json:"agent_version,omitempty"`
	EnrolledAt  int64  `json:"enrolled_at_unix"`
	VpnPort     uint16 `json:"vpn_port,omitempty"`
	TransitPort uint16 `json:"transit_port,omitempty"`
	SiteID      string `json:"site_id,omitempty"`
	// Config 是设备自报"我跑的是哪一版"。它变化很慢（一次发布一次），但
	// 不落盘就会让控制台在重启后失去"这台设备到底跑的哪一版" —— 而这正是
	// 排障时第一个要看的东西。所以它进硬状态；隧道读数/接口清单不进（它们
	// 带计数器，每拍都变）。
	Config *wire.ConfigState `json:"config_state,omitempty"`
}

func (n *NodeRecord) hardView() nodeHard {
	return nodeHard{
		TenantID: n.TenantID, NodeID: n.NodeID, Serial: n.Serial,
		PublicKey: n.PublicKey, CertPEM: n.CertPEM, AgentVer: n.AgentVer,
		EnrolledAt: n.EnrolledAt, VpnPort: n.VpnPort, TransitPort: n.TransitPort,
		SiteID: n.SiteID, Config: n.Config,
	}
}

// applyHard 把复制来的硬字段盖到本地记录上，软字段（心跳读数）原样保留。
//
// 只覆盖硬字段而不是整条替换：设备可能正在心跳，替换会把这一拍刚报上来的
// 软状态清掉，控制台就会在切换瞬间出现"设备在线但所有读数空白"，看起来像故障。
func (n *NodeRecord) applyHard(v nodeHard) {
	n.TenantID, n.NodeID, n.Serial = v.TenantID, v.NodeID, v.Serial
	n.PublicKey, n.CertPEM, n.AgentVer = v.PublicKey, v.CertPEM, v.AgentVer
	n.EnrolledAt, n.SiteID = v.EnrolledAt, v.SiteID
	n.Config = v.Config
	// 端口只在"对端有"时覆盖：设备自报的端口比池里分配的更新，而自报端口是
	// 软状态，不能被一个旧快照退回去。
	if v.VpnPort != 0 {
		n.VpnPort = v.VpnPort
	}
	if v.TransitPort != 0 {
		n.TransitPort = v.TransitPort
	}
}

// hardRowsOf 把整份硬状态展开成行。O(n) 且每行 marshal 一次 —— 只应该在提交
// 路径上调用（人操作的速度），心跳路径不调用（见 server.saveSoft）。
func hardRowsOf(st *State) ([]HardRow, error) {
	var out []HardRow
	v := reflect.ValueOf(st).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // 未导出
			continue
		}
		// Cluster 由配置项拥有：把它也塞进行复制，会和成员变更互相覆盖。
		if f.Name == "Cluster" {
			continue
		}
		fv := v.Field(i)
		if fv.Kind() != reflect.Map {
			b, err := json.Marshal(fv.Interface())
			if err != nil {
				return nil, fmt.Errorf("marshal %s: %w", f.Name, err)
			}
			out = append(out, HardRow{Table: f.Name, Data: b})
			continue
		}
		if fv.IsNil() {
			continue
		}
		nodeMap := fv.Type().Elem() == reflect.TypeOf((*NodeRecord)(nil))
		for _, k := range fv.MapKeys() {
			elem := fv.MapIndex(k)
			var payload any
			if nodeMap {
				rec, _ := elem.Interface().(*NodeRecord)
				if rec == nil {
					continue
				}
				payload = rec.hardView()
			} else if elem.Kind() == reflect.Interface || elem.Kind() == reflect.Ptr {
				if elem.IsNil() {
					continue // nil 行等于没有这一行
				}
				payload = elem.Interface()
			} else {
				payload = elem.Interface()
			}
			b, err := json.Marshal(payload)
			if err != nil {
				return nil, fmt.Errorf("marshal %s/%v: %w", f.Name, k.Interface(), err)
			}
			out = append(out, HardRow{Table: f.Name, Key: fmt.Sprint(k.Interface()), Data: b})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id() < out[j].id() })
	return out, nil
}

// hardHash 是行内容的指纹。提交时只比较指纹，不比较内容：一次发布的 diff 通常
// 只有一两行，但比较是 O(全部行)。
func hardHash(r HardRow) string {
	sum := sha256.Sum256(r.Data)
	return hex.EncodeToString(sum[:])
}

// rowDiff 返回从 prev（rowID -> 指纹）到 cur 的变化行，并把 cur 的指纹写回 prev。
// 被删掉的行以墓碑形式返回，否则对端永远删不掉这个键。
func rowDiff(prev map[string]string, cur []HardRow) []HardRow {
	changed := make([]HardRow, 0, 4)
	seen := make(map[string]bool, len(cur))
	for _, r := range cur {
		id := r.id()
		seen[id] = true
		h := hardHash(r)
		if prev[id] == h {
			continue
		}
		prev[id] = h
		changed = append(changed, r)
	}
	for id := range prev {
		if seen[id] {
			continue
		}
		delete(prev, id)
		// 只有 map 行才有墓碑；标量字段永远存在（零值也会被序列化）。
		table, key, ok := splitRowID(id)
		if !ok || key == "" {
			continue
		}
		changed = append(changed, HardRow{Table: table, Key: key})
	}
	return changed
}

func splitRowID(id string) (table, key string, ok bool) {
	for i := 0; i < len(id); i++ {
		if id[i] == 0 {
			return id[:i], id[i+1:], true
		}
	}
	return id, "", true
}

// applyRows 把行盖到状态上。对端的 apply 只走这一条路径，所以这里对类型的要求
// 必须是显式的：不认识的字段名直接报错，而不是静默跳过 —— 静默跳过会让两台机器
// 的 state 悄悄分叉，而分叉是最难查的那类故障。
func applyRows(st *State, rows []HardRow) error {
	if st.Cluster == nil {
		// 配置项可能先于 Cluster 行到达：保住 Cluster 字段不被 applyRows 清掉。
	}
	v := reflect.ValueOf(st).Elem()
	t := v.Type()
	for _, r := range rows {
		sf, ok := t.FieldByName(r.Table)
		if !ok || sf.PkgPath != "" {
			return fmt.Errorf("apply: 未知状态字段 %q", r.Table)
		}
		f := v.FieldByName(r.Table)
		if f.Kind() == reflect.Map {
			if f.IsNil() {
				f.Set(reflect.MakeMap(f.Type()))
			}
			k := reflect.ValueOf(r.Key).Convert(f.Type().Key())
			if r.deleted() {
				f.SetMapIndex(k, reflect.Value{})
				continue
			}
			elem := reflect.New(f.Type().Elem()).Elem()
			// 已经有这一行就复用它：map 里存的是 *NodeRecord 指针，复用才能
			// 让 decodeRow 走到 applyHard（只盖硬字段、留住心跳读数）。
			// 换成 New 出来的空记录，等于每次同步都把软状态清一遍。
			if cur := f.MapIndex(k); cur.IsValid() {
				elem.Set(cur)
			}
			if err := decodeRow(r, elem); err != nil {
				return err
			}
			f.SetMapIndex(k, elem)
			continue
		}
		if r.deleted() {
			f.Set(reflect.Zero(f.Type()))
			continue
		}
		if isJSONNull(r.Data) {
			// "null" 是有效的绝对值（指针字段为 nil），但对结构体 unmarshal 是
			// no-op：不显式清零的话，"对端是 nil、本机不是"永远同步不过来。
			f.Set(reflect.Zero(f.Type()))
			continue
		}
		if err := decodeRow(r, f); err != nil {
			return err
		}
	}
	return nil
}

// isJSONNull 判断这一行的载荷是不是字面量 null。行是 json.Marshal 出来的，
// 没有多余空白，所以只需要比长度和四个字节。
func isJSONNull(b []byte) bool { return len(b) == 4 && string(b) == "null" }

// decodeRow 负责把行写进目标值；*NodeRecord 走 applyHard，保留软字段。
func decodeRow(r HardRow, dst reflect.Value) error {
	for dst.Kind() == reflect.Ptr {
		if dst.IsNil() {
			dst.Set(reflect.New(dst.Type().Elem()))
		}
		if rec, ok := dst.Interface().(*NodeRecord); ok {
			var h nodeHard
			if err := json.Unmarshal(r.Data, &h); err != nil {
				return fmt.Errorf("apply %s/%s: %w", r.Table, r.Key, err)
			}
			rec.applyHard(h)
			return nil
		}
		dst = dst.Elem()
	}
	if err := json.Unmarshal(r.Data, dst.Addr().Interface()); err != nil {
		return fmt.Errorf("apply %s/%s: %w", r.Table, r.Key, err)
	}
	return nil
}

// cloneState 是"上一次成功提交"的还原点。提交失败时用它把内存退回去，保证
// "内存里有"与"多数派已提交"是同一件事。
//
// 只在提交成功后调用（写盘/复制成功才拍快照），所以代价是"每次硬写一次 187 KB
// 的 JSON 往返"——人操作的速度下可以忽略，而心跳路径根本不进这里。
func cloneState(st *State) (*State, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	var out State
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// hardFingerprint 是整份硬状态的指纹：控制台用它回答"这两台差多少"，测试用它
// 一眼看出两台机器的状态是否一致。
func hardFingerprint(st *State) string {
	rows, err := hardRowsOf(st)
	if err != nil {
		return "err"
	}
	h := sha256.New()
	for _, r := range rows {
		fmt.Fprintf(h, "%s=%x;", r.id(), sha256.Sum256(r.Data))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
