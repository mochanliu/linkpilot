package main

// 隧道表格：一份 CSV 里同时表达新增、修改与删除。
//
// 为什么是 CSV：控制器不能引第三方库（controller/go.mod 只有标准库），xlsx
// 要么带一个读写器进来、要么自己写一套 zip+XML；CSV 是 Excel、WPS、Numbers
// 都能直接开、也都能直接存的最小公约数。文件带 UTF-8 BOM，中文表头在 Excel
// 里不会变成乱码；万一另存成了 GBK，这里明说并要求重存，而不是把乱码当成
// 设备名去猜。
//
// 一行的语义是「这条隧道应该长这样」，不是「执行这个动作」：下载 → 改 →
// 上传，没动过的行原样回来就是「不变」，不会白白改一次 UpdatedAt 让设备重
// 下发。三件事因此能放进同一个文件：
//
//	新增  隧道ID 留空
//	修改  隧道ID 填着，改掉想改的列（操作列可以留空）
//	删除  操作 写「删除」；整行删掉也行，但要在页面上勾「以文件为准」
//
// 整份文件是全有或全无：任何一行有错（设备不存在、颜色越界、模板找不到），
// 所有行都不写。半份表格落库之后，操作者无法只靠看文件知道哪些生效了。

import (
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"sdwan/controller/internal/underlay"
)

const (
	// underlayCSVMaxBytes 是一份表格的上限。1 MiB 够写几千行，也够挡住
	// 「不小心选了一个数据库导出文件」。
	underlayCSVMaxBytes = 1 << 20
	// underlayCSVMaxRows 是行数上限。超过它的表格多半不是人改的：一台设备
	// 一把密钥，几千条隧道已经越过每对 8 条的密钥槽上限。
	underlayCSVMaxRows = 2000
)

// underlayCSVColumn 是一列的定义。页面上的格式说明和下载文件里的表头都从
// 这一份生成——两处各写一遍，改列的时候一定会漏一处。
type underlayCSVColumn struct {
	Title string `json:"title"`
	Hint  string `json:"hint"`
}

func underlayCSVColumns() []underlayCSVColumn {
	return []underlayCSVColumn{
		{"操作", "留空 = 按隧道ID判断（有ID就是修改，没ID就是新增）；也可以直接写 新增 / 修改 / 删除"},
		{"隧道ID", "已有隧道的 ID（t3…）。新增留空；删除必填"},
		{"本端设备", "序列号，或 node-3，或 3"},
		{"本端接口", "留空 = 设备上报的出口端口"},
		{"本端地址", "ip 或 ip:port；留空 = 用反射器观测到的外层地址"},
		{"本端端口", "留空 = 自动分配；填了就按这个值用（不能和同设备其它隧道重复）"},
		{"对端设备", "同「本端设备」"},
		{"对端接口", "留空 = 设备上报的出口端口"},
		{"对端地址", "同「本端地址」"},
		{"对端端口", "留空 = 自动分配"},
		{"颜色", "0..7；留空 = 0（最优）"},
		{"权重", "1..255；留空 = 1（等价分担）。开了「动态」时它是调度上限"},
		{"动态", "开 / 关；留空 = 关"},
		{"FEC", "0..4；留空 = 0（关）。1 = 整包复制，n = 每 n 包 1 个冗余包"},
		{"中转路径", "留空 = 直连；模板:名称 = 引用路径模板；a>b>c = 内联中转链（每跳可写 a@地址）"},
	}
}

func underlayCSVTitles() []string {
	cols := underlayCSVColumns()
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Title
	}
	return out
}

// underlayCSVDoc 是页面上「表格格式」那一块的数据。给页面而不是让页面自己
// 写一遍：列宽、顺序和表头只有一个来源。
type underlayCSVDoc struct {
	Columns  []underlayCSVColumn `json:"columns"`
	MaxRows  int                 `json:"max_rows"`
	MaxBytes int                 `json:"max_bytes"`
	FileName string              `json:"file_name"`
}

func newUnderlayCSVDoc() underlayCSVDoc {
	return underlayCSVDoc{
		Columns:  underlayCSVColumns(),
		MaxRows:  underlayCSVMaxRows,
		MaxBytes: underlayCSVMaxBytes,
		FileName: "underlay-tunnels.csv",
	}
}

// ---------------------------------------------------------------- 导出

// underlayCSVBytes renders the tenant's tunnel table: the header plus one row
// per tunnel, in the order the page lists them. 操作 stays empty, so uploading
// the file untouched changes nothing.
//
// Must be called with s.mu held.
func (s *server) underlayCSVBytesLocked(tenant string, now int64) []byte {
	st := s.effectiveUnderlayLocked(tenant)
	views := s.nodeViewsLocked(tenant, now)
	serials := serialMap(views)

	var b bytes.Buffer
	// BOM：Excel 靠它认出 UTF-8。没有它，中文表头在中文 Windows 上会变成乱码，
	// 而乱码导回来就是一堆「设备名不存在」。
	b.WriteString("\ufeff")
	w := csv.NewWriter(&b)
	_ = w.Write(underlayCSVTitles())
	for _, t := range st.SortedTunnels() {
		_ = w.Write(underlayCSVRowFor(t, st, serials))
	}
	w.Flush()
	return b.Bytes()
}

func underlayCSVRowFor(t *underlay.Tunnel, st *underlay.State, serials map[uint16]string) []string {
	return []string{
		"",
		t.ID,
		csvSerial(serials, t.A.NodeID), t.A.Iface, t.A.Addr, csvPort(t.A.Port),
		csvSerial(serials, t.B.NodeID), t.B.Iface, t.B.Addr, csvPort(t.B.Port),
		strconv.Itoa(int(t.Color)),
		csvWeight(t.Weight),
		map[bool]string{true: "开", false: "关"}[t.Dynamic],
		strconv.Itoa(int(t.FecRatio)),
		storedCSVPath(t, st, serials),
	}
}

func csvSerial(serials map[uint16]string, node uint16) string {
	if s := strings.TrimSpace(serials[node]); s != "" {
		return s
	}
	return fmt.Sprintf("node-%d", node)
}

func csvPort(p uint16) string {
	if p == 0 {
		return ""
	}
	return strconv.Itoa(int(p))
}

// csvWeight 把「默认 1」写成空单元格：0 和 1 在数据面是同一件事，导出成
// "0" 只会让人以为这里有个可以改的数。
func csvWeight(w uint8) string {
	if w == 0 {
		return ""
	}
	return strconv.Itoa(int(w))
}

// storedCSVPath is the one spelling of "which relays does this tunnel take".
// A template is written by NAME because that is what the operator recognises;
// the id is the fallback for a reference whose template is already gone, so
// re-uploading the file reports the broken row instead of silently flattening
// the tunnel to direct.
func storedCSVPath(t *underlay.Tunnel, st *underlay.State, serials map[uint16]string) string {
	if t.Path == nil {
		return ""
	}
	if t.Path.TemplateID != "" {
		if p := st.Paths[t.Path.TemplateID]; p != nil {
			return "模板:" + p.Name
		}
		return "模板:" + t.Path.TemplateID
	}
	return inlineCSVPath(t.Path.Hops, serials)
}

func inlineCSVPath(hops []underlay.PathHop, serials map[uint16]string) string {
	parts := make([]string, 0, len(hops))
	for _, h := range hops {
		cell := csvSerial(serials, h.NodeID)
		if a := strings.TrimSpace(h.Addr); a != "" {
			cell += "@" + a
		}
		parts = append(parts, cell)
	}
	return strings.Join(parts, ">")
}

// ---------------------------------------------------------------- 解析

// csvEndpoint is one end of a row as the file wrote it. PortSet distinguishes
// "留空 = 自动分配" from "填了 0"，because 0 is not a port.
type csvEndpoint struct {
	NodeID  uint16
	Iface   string
	Addr    string
	Port    uint16
	PortSet bool
}

type csvTunnelSpec struct {
	Line int
	// Kind is add | update | delete. A row that matches its tunnel field for
	// field is dropped before this list is built (nothing to do).
	Kind    string
	ID      string
	A, B    csvEndpoint
	Color   uint8
	Weight  uint8
	Fec     uint8
	Dynamic bool
	Path    *underlay.TunnelPath
	// PathTxt is the canonical spelling of PathTxt (see storedCSVPath), used
	// for comparison and for the preview line.
	PathTxt string
}

// csvKey is the comparable shape of a row and of a stored tunnel. Everything
// is resolved first (device names to ids, templates to names) so "the operator
// retyped the same thing in another spelling" is not a change.
type csvKey struct {
	ANode, BNode       uint16
	AIface, AAddr      string
	APort              uint16
	BIface, BAddr      string
	BPort              uint16
	Color, Weight, Fec uint8
	Dynamic            bool
	Path               string
}

func storedCSVKey(t *underlay.Tunnel, st *underlay.State, serials map[uint16]string) csvKey {
	return csvKey{
		ANode: t.A.NodeID, BNode: t.B.NodeID,
		AIface: t.A.Iface, AAddr: t.A.Addr, APort: t.A.Port,
		BIface: t.B.Iface, BAddr: t.B.Addr, BPort: t.B.Port,
		Color: t.Color, Weight: t.Weight, Fec: t.FecRatio, Dynamic: t.Dynamic,
		Path: storedCSVPath(t, st, serials),
	}
}

// keyFor folds a row against the tunnel it updates. Empty cells mean "leave
// this alone" for the three fields where a blank has no better meaning
// (interface, address, port); the rest of the columns are "what the file
// says", because a spreadsheet cannot express "unchanged" any other way.
func (r csvTunnelSpec) keyFor(stored *underlay.Tunnel) csvKey {
	k := csvKey{
		ANode: r.A.NodeID, BNode: r.B.NodeID,
		AIface: r.A.Iface, AAddr: r.A.Addr, APort: r.A.Port,
		BIface: r.B.Iface, BAddr: r.B.Addr, BPort: r.B.Port,
		Color: r.Color, Weight: r.Weight, Fec: r.Fec, Dynamic: r.Dynamic,
		Path: r.PathTxt,
	}
	if stored == nil {
		return k
	}
	if !r.A.PortSet {
		k.APort = stored.A.Port
	}
	if !r.B.PortSet {
		k.BPort = stored.B.Port
	}
	if k.AIface == "" {
		k.AIface = stored.A.Iface
	}
	if k.BIface == "" {
		k.BIface = stored.B.Iface
	}
	if k.AAddr == "" {
		k.AAddr = stored.A.Addr
	}
	if k.BAddr == "" {
		k.BAddr = stored.B.Addr
	}
	return k
}

// --------------------------------------------------------- 设备与模板索引

type csvDeviceIndex struct {
	byKey map[string]uint16
}

// buildCSVDeviceIndex lets a row name a device the way the page shows it
// (serial), the way the API names it (node-3) or as a bare number. A key that
// two devices claim is dropped instead of resolved to the first one: picking
// one of two devices named the same would build a tunnel to a box nobody
// meant.
func buildCSVDeviceIndex(views map[uint16]underlay.NodeView, serials map[uint16]string) csvDeviceIndex {
	idx := csvDeviceIndex{byKey: map[string]uint16{}}
	dup := map[string]bool{}
	ids := make([]uint16, 0, len(views))
	for id := range views {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		keys := []string{
			strconv.Itoa(int(id)),
			fmt.Sprintf("node-%d", id),
			fmt.Sprintf("设备 %d", id),
			fmt.Sprintf("设备%d", id),
		}
		for _, raw := range []string{views[id].Name, serials[id]} {
			if n := strings.TrimSpace(raw); n != "" {
				keys = append(keys, n, strings.ToLower(n))
			}
		}
		for _, k := range keys {
			if prev, ok := idx.byKey[k]; ok && prev != id {
				dup[k] = true
			}
			idx.byKey[k] = id
		}
	}
	for k := range dup {
		delete(idx.byKey, k)
	}
	return idx
}

func (ix csvDeviceIndex) lookup(name string) (uint16, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, false
	}
	if id, ok := ix.byKey[name]; ok {
		return id, true
	}
	id, ok := ix.byKey[strings.ToLower(name)]
	return id, ok
}

// --------------------------------------------------------------- 计划

type underlayCSVRow struct {
	Line int    `json:"line"`
	Op   string `json:"op"` // add | update | delete | keep | error
	ID   string `json:"id,omitempty"`
	Text string `json:"text,omitempty"`
	Msg  string `json:"msg,omitempty"`
}

type underlayCSVPlan struct {
	Rows   []underlayCSVRow `json:"rows"`
	Add    int              `json:"add"`
	Update int              `json:"update"`
	Delete int              `json:"delete"`
	Keep   int              `json:"keep"`
	Errors int              `json:"errors"`
}

func (p *underlayCSVPlan) count(r underlayCSVRow) {
	switch r.Op {
	case "add":
		p.Add++
	case "update":
		p.Update++
	case "delete":
		p.Delete++
	case "keep":
		p.Keep++
	case "error":
		p.Errors++
	}
	p.Rows = append(p.Rows, r)
}

// planUnderlayCSV turns the uploaded bytes into the list of writes plus the
// per-row report the page shows before anything is applied. It never touches
// the state: the same function produces the preview and the commit.
func planUnderlayCSV(st *underlay.State, views map[uint16]underlay.NodeView,
	raw []byte, prune bool) ([]csvTunnelSpec, underlayCSVPlan, error) {

	recs, err := readUnderlayCSV(raw)
	if err != nil {
		return nil, underlayCSVPlan{}, err
	}
	serials := serialMap(views)
	ix := buildCSVDeviceIndex(views, serials)
	byID := map[string]*underlay.Tunnel{}
	for id, t := range st.Tunnels {
		byID[id] = t
	}

	var (
		plan  = underlayCSVPlan{Rows: []underlayCSVRow{}}
		specs []csvTunnelSpec
		seen  = map[string]int{}
	)
	for i, rec := range recs {
		line := i + 2 // 表头是第一行
		spec, row, err := parseCSVRecord(rec, line, st, views, serials, ix, byID, seen)
		if err != nil {
			row.Op, row.Msg = "error", err.Error()
			plan.count(row)
			continue
		}
		row.Line = line
		if spec.ID != "" {
			// 记在「出现过」里，而不是只记会被写入的行：同一份文件里同一个ID
			// 出现两次，哪怕其中一次是「不变」，也是文件错了；而以文件为准
			// 删除时，出现过的（包括没改的）都不算「不在文件里」。
			seen[spec.ID] = line
		}
		row.Op = spec.Kind
		if spec.Kind == "" {
			row.Op, row.Msg = "keep", "和现有隧道一致"
		} else {
			specs = append(specs, spec)
		}
		row.ID, row.Text = spec.ID, csvRowText(spec, serials)
		plan.count(row)
	}

	// 以文件为准：文件里没提到的隧道删掉。显式写了「删除」的行已经在 specs
	// 里，这里只补那些整行被删掉的；没改过但留在文件里的行（seen）当然不算
	// 没提到。
	if prune {
		for _, t := range st.SortedTunnels() {
			if _, mentioned := seen[t.ID]; mentioned {
				continue
			}
			specs = append(specs, csvTunnelSpec{Line: 0, Kind: "delete", ID: t.ID,
				A: csvEndpoint{NodeID: t.A.NodeID}, B: csvEndpoint{NodeID: t.B.NodeID}})
			plan.count(underlayCSVRow{
				Line: 0, Op: "delete", ID: t.ID,
				Text: csvSerial(serials, t.A.NodeID) + " ↔ " + csvSerial(serials, t.B.NodeID),
				Msg:  "不在文件里，按「以文件为准」删除",
			})
		}
	}
	return specs, plan, nil
}

func csvRowText(spec csvTunnelSpec, serials map[uint16]string) string {
	a := csvSerial(serials, spec.A.NodeID)
	b := csvSerial(serials, spec.B.NodeID)
	path := "直连"
	if spec.PathTxt != "" {
		path = spec.PathTxt
	}
	return a + " ↔ " + b + " · " + path
}

// parseCSVRecord resolves one line. Returned error text is what the operator
// reads next to the line number, so it names the column and the value.
func parseCSVRecord(rec []string, line int, st *underlay.State, views map[uint16]underlay.NodeView,
	serials map[uint16]string, ix csvDeviceIndex, byID map[string]*underlay.Tunnel,
	seen map[string]int) (csvTunnelSpec, underlayCSVRow, error) {

	cell := func(i int) string {
		if i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	row := underlayCSVRow{Line: line}
	op, ok := normalizeCSVOp(cell(0))
	if !ok {
		return csvTunnelSpec{}, row, fmt.Errorf("操作列「%s」不认识：只认 新增 / 修改 / 删除，留空表示自动判断", cell(0))
	}
	id := cell(1)
	if id != "" {
		if prev, dup := seen[id]; dup && op != "delete" {
			return csvTunnelSpec{}, row, fmt.Errorf("隧道 %s 在第 %d 行已经出现过", id, prev)
		}
	}
	spec := csvTunnelSpec{Line: line, ID: id}

	if op == "delete" {
		if id == "" {
			return csvTunnelSpec{}, row, fmt.Errorf("删除行必须填隧道ID")
		}
		t := byID[id]
		if t == nil {
			return csvTunnelSpec{}, row, fmt.Errorf("隧道 %s 不存在（可能已经被删掉了）：文件是不是旧的？", id)
		}
		spec.Kind, spec.A.NodeID, spec.B.NodeID = "delete", t.A.NodeID, t.B.NodeID
		spec.PathTxt = storedCSVPath(t, st, serials)
		return spec, row, nil
	}
	if op == "add" && id != "" {
		return csvTunnelSpec{}, row, fmt.Errorf("「新增」行不要填隧道ID：ID 由控制器分配")
	}
	if op == "update" && id == "" {
		return csvTunnelSpec{}, row, fmt.Errorf("「修改」行必须填已有的隧道ID")
	}

	var stored *underlay.Tunnel
	if id != "" {
		stored = byID[id]
		if stored == nil {
			return csvTunnelSpec{}, row, fmt.Errorf("隧道 %s 不存在（可能已经被删掉了）：新增一行请把隧道ID留空", id)
		}
	}

	var err error
	if spec.A, err = parseCSVEndpoint(cell(2), cell(3), cell(4), cell(5), "本端", ix, views); err != nil {
		return csvTunnelSpec{}, row, err
	}
	if spec.B, err = parseCSVEndpoint(cell(6), cell(7), cell(8), cell(9), "对端", ix, views); err != nil {
		return csvTunnelSpec{}, row, err
	}
	if spec.A.NodeID == spec.B.NodeID {
		return csvTunnelSpec{}, row, fmt.Errorf("两端都是设备 %s：隧道不能连自己", cell(2))
	}
	if spec.Color, err = csvUint(cell(10), "颜色", int(underlay.MaxTunnelColor)); err != nil {
		return csvTunnelSpec{}, row, err
	}
	if spec.Weight, err = csvUint(cell(11), "权重", 255); err != nil {
		return csvTunnelSpec{}, row, err
	}
	if spec.Dynamic, err = csvBool(cell(12), "动态"); err != nil {
		return csvTunnelSpec{}, row, err
	}
	if spec.Fec, err = csvUint(cell(13), "FEC", 4); err != nil {
		return csvTunnelSpec{}, row, err
	}
	if spec.Path, spec.PathTxt, err = parseCSVPath(cell(14), st, serials, ix); err != nil {
		return csvTunnelSpec{}, row, err
	}

	if stored == nil {
		spec.Kind = "add"
		return spec, row, nil
	}
	spec.Kind = "update"
	if spec.keyFor(stored) == storedCSVKey(stored, st, serials) {
		// 一个字段都没变：不写。空 Kind 让调用方把它记成「不变」。
		spec.Kind = ""
	}
	return spec, row, nil
}

func parseCSVEndpoint(dev, iface, addr, port, what string,
	ix csvDeviceIndex, views map[uint16]underlay.NodeView) (csvEndpoint, error) {

	var ep csvEndpoint
	var err error
	if ep.NodeID, err = ix.must(dev, what+"设备"); err != nil {
		return ep, err
	}
	ep.Iface = iface
	// 空接口 = 设备上报的出口；设备没上报过就留空交给更新逻辑保留原值。
	if ep.Iface == "" {
		ep.Iface = strings.TrimSpace(views[ep.NodeID].DefaultIface)
	}
	ep.Addr = addr
	if p := strings.TrimSpace(port); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 65535 {
			return ep, fmt.Errorf("%s端口「%s」不是 0..65535 的端口号", what, p)
		}
		ep.Port, ep.PortSet = uint16(n), n > 0
	}
	return ep, nil
}

func (ix csvDeviceIndex) must(cell, what string) (uint16, error) {
	if strings.TrimSpace(cell) == "" {
		return 0, fmt.Errorf("%s没有填", what)
	}
	id, ok := ix.lookup(cell)
	if !ok {
		return 0, fmt.Errorf("%s「%s」不在设备表里：可以填序列号、node-3 或设备号", what, strings.TrimSpace(cell))
	}
	return id, nil
}

// parseCSVPath: 空 = 直连；模板:名称 = 共享模板；a>b>c = 内联链。
//
// 单独的「a」既可能是一台中转设备，也可能是一个模板名。这里按设备解释：
// 这一列叫「中转路径」，写一台设备就是经过它中转，这是更常见的写法；要用
// 模板就写「模板:名称」，页面上那个前缀不会漏。
func parseCSVPath(cell string, st *underlay.State, serials map[uint16]string, ix csvDeviceIndex) (*underlay.TunnelPath, string, error) {
	cell = strings.TrimSpace(cell)
	if cell == "" {
		return nil, "", nil
	}
	if name, ok := trimCSVPrefix(cell, "模板"); ok {
		if name == "" {
			return nil, "", fmt.Errorf("「模板:」后面要写模板名")
		}
		tpl, err := findCSVTemplate(st, name)
		if err != nil {
			return nil, "", err
		}
		return &underlay.TunnelPath{TemplateID: tpl.ID}, "模板:" + tpl.Name, nil
	}

	chain := strings.NewReplacer("→", ">", "->", ">", "，", ">", ",", ">").Replace(cell)
	parts := strings.Split(chain, ">")
	if len(parts) > underlay.MaxRelayHops {
		return nil, "", fmt.Errorf("中转路径写了 %d 跳，最多 %d 跳", len(parts), underlay.MaxRelayHops)
	}
	hops := make([]underlay.PathHop, 0, len(parts))
	for i, raw := range parts {
		name, addr := splitCSVHop(raw)
		if name == "" {
			return nil, "", fmt.Errorf("中转路径第 %d 跳是空的", i+1)
		}
		id, ok := ix.lookup(name)
		if !ok {
			if strings.Contains(name, "模板") {
				return nil, "", fmt.Errorf("中转路径第 %d 跳「%s」不是设备；引用模板要写成「模板:名称」", i+1, name)
			}
			return nil, "", fmt.Errorf("中转路径第 %d 跳「%s」不在设备表里：可以填序列号、node-3 或设备号", i+1, name)
		}
		hops = append(hops, underlay.PathHop{NodeID: id, Addr: addr})
	}
	return &underlay.TunnelPath{Hops: hops}, inlineCSVPath(hops, serials), nil
}

func splitCSVHop(cell string) (string, string) {
	cell = strings.TrimSpace(cell)
	if i := strings.Index(cell, "@"); i > 0 {
		return strings.TrimSpace(cell[:i]), strings.TrimSpace(cell[i+1:])
	}
	// 页面上中转链写成「设备（地址）」；允许直接把它粘进表格。
	for _, pair := range [][2]string{{"（", "）"}, {"(", ")"}} {
		if i := strings.LastIndex(cell, pair[0]); i > 0 && strings.HasSuffix(cell, pair[1]) {
			return strings.TrimSpace(cell[:i]),
				strings.TrimSpace(strings.TrimSuffix(cell[i+len(pair[0]):], pair[1]))
		}
	}
	return cell, ""
}

func trimCSVPrefix(cell, prefix string) (string, bool) {
	for _, sep := range []string{":", "："} {
		if strings.HasPrefix(cell, prefix+sep) {
			return strings.TrimSpace(cell[len(prefix)+len(sep):]), true
		}
	}
	return "", false
}

func findCSVTemplate(st *underlay.State, key string) (*underlay.PathTemplate, error) {
	if p := st.Paths[key]; p != nil {
		return p, nil
	}
	var hits []*underlay.PathTemplate
	for _, p := range st.Paths {
		if p != nil && strings.EqualFold(strings.TrimSpace(p.Name), key) {
			hits = append(hits, p)
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].ID < hits[j].ID })
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("没有叫「%s」的路径模板：表格只引用模板、不创建模板，先在页面上的「路径模板」里建好", key)
	case 1:
		return hits[0], nil
	default:
		ids := make([]string, 0, len(hits))
		for _, p := range hits {
			ids = append(ids, p.ID)
		}
		return nil, fmt.Errorf("有 %d 个模板都叫「%s」（%s）：表格里改用「模板:ID」区分",
			len(hits), key, strings.Join(ids, "、"))
	}
}

func normalizeCSVOp(cell string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(cell)) {
	case "", "自动", "auto":
		return "", true
	case "新增", "新建", "添加", "add", "create", "new":
		return "add", true
	case "修改", "更新", "改", "update", "modify", "edit":
		return "update", true
	case "删除", "删", "delete", "del", "remove":
		return "delete", true
	}
	return "", false
}

func csvUint(cell, what string, max int) (uint8, error) {
	s := strings.TrimSpace(cell)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s「%s」不是数字", what, s)
	}
	if n < 0 || n > max {
		return 0, fmt.Errorf("%s %d 越界（0..%d）", what, n, max)
	}
	return uint8(n), nil
}

func csvBool(cell, what string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(cell)) {
	case "", "关", "否", "off", "no", "false", "0":
		return false, nil
	case "开", "是", "on", "yes", "true", "1":
		return true, nil
	}
	return false, fmt.Errorf("%s「%s」只认 开 / 关", what, cell)
}

// readUnderlayCSV decodes the file and validates the header. Rows that are
// empty or start with '#' are skipped, so a file can carry its own notes above
// the header while people keep editing it in Excel.
func readUnderlayCSV(raw []byte) ([][]string, error) {
	if len(raw) > underlayCSVMaxBytes {
		return nil, fmt.Errorf("表格太大（%d KiB，上限 %d KiB）", len(raw)>>10, underlayCSVMaxBytes>>10)
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(raw) {
		// 不在服务端猜编码：GBK 的字节流里没有可靠的标记，猜错一次就是把
		// 另一个人的设备名写进配置。
		return nil, fmt.Errorf("文件不是 UTF-8：Excel 另存时请选「CSV UTF-8（逗号分隔）」，不要用「CSV（逗号分隔）」")
	}
	rd := csv.NewReader(bytes.NewReader(raw))
	rd.FieldsPerRecord = -1
	rd.TrimLeadingSpace = true
	rd.LazyQuotes = true
	recs, err := rd.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("表格解析失败：%v", err)
	}
	recs = dropCSVComments(recs)
	if len(recs) == 0 {
		return nil, fmt.Errorf("文件是空的：第一行要是表头")
	}
	want := underlayCSVTitles()
	head := recs[0]
	matched := len(head) == len(want)
	for i := 0; matched && i < len(want); i++ {
		matched = strings.TrimSpace(head[i]) == want[i]
	}
	if !matched {
		return nil, fmt.Errorf("第一行要是表头，且列的顺序不能变：%s", strings.Join(want, ","))
	}
	body := recs[1:]
	if len(body) > underlayCSVMaxRows {
		return nil, fmt.Errorf("表格有 %d 行，上限 %d 行", len(body), underlayCSVMaxRows)
	}
	return body, nil
}

func dropCSVComments(recs [][]string) [][]string {
	out := recs[:0]
	for _, rec := range recs {
		empty := true
		for _, c := range rec {
			if strings.TrimSpace(c) != "" {
				empty = false
				break
			}
		}
		if empty {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(rec[0]), "#") {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// ---------------------------------------------------------------- 落库

// applyUnderlayCSV writes the planned operations onto st: deletes first (they
// free ports and overlay /32s the same file may be about to reuse), then
// updates, then adds.
func applyUnderlayCSV(st *underlay.State, specs []csvTunnelSpec, views map[uint16]underlay.NodeView, now int64) error {
	var deletes, updates, adds []csvTunnelSpec
	for _, sp := range specs {
		switch sp.Kind {
		case "delete":
			deletes = append(deletes, sp)
		case "update":
			updates = append(updates, sp)
		case "add":
			adds = append(adds, sp)
		}
	}
	where := func(sp csvTunnelSpec) string {
		if sp.Line <= 0 {
			return "隧道 " + sp.ID
		}
		return fmt.Sprintf("第 %d 行", sp.Line)
	}
	for _, sp := range deletes {
		if err := st.Delete(sp.ID); err != nil {
			return fmt.Errorf("%s：%v", where(sp), err)
		}
	}
	for _, sp := range updates {
		upd := underlay.UpdateRequest{
			IfaceA: sp.A.Iface, IfaceB: sp.B.Iface,
			PortA: sp.A.Port, PortB: sp.B.Port,
			Color: &sp.Color, Weight: &sp.Weight, Dynamic: &sp.Dynamic, FecRatio: &sp.Fec,
			Now: now,
		}
		addrA, addrB := sp.A.Addr, sp.B.Addr
		upd.AddrA, upd.AddrB = &addrA, &addrB
		if sp.Path == nil {
			upd.ClearPath = true
		} else {
			upd.Path = sp.Path
		}
		if _, err := st.Update(sp.ID, upd, views); err != nil {
			return fmt.Errorf("%s（隧道 %s）：%v", where(sp), sp.ID, err)
		}
	}
	for _, sp := range adds {
		if _, err := st.Create(underlay.CreateRequest{
			A:        underlay.EndpointSpec{NodeID: sp.A.NodeID, Iface: sp.A.Iface, Addr: sp.A.Addr, Port: sp.A.Port},
			B:        underlay.EndpointSpec{NodeID: sp.B.NodeID, Iface: sp.B.Iface, Addr: sp.B.Addr, Port: sp.B.Port},
			Path:     sp.Path,
			Color:    sp.Color,
			Weight:   sp.Weight,
			Dynamic:  sp.Dynamic,
			FecRatio: sp.Fec,
			Now:      now,
		}, views); err != nil {
			return fmt.Errorf("%s：%v", where(sp), err)
		}
	}
	return nil
}

// ---------------------------------------------------------------- HTTP

func (s *server) handleAdminUnderlayTunnelCSV(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	s.mu.Lock()
	body := s.underlayCSVBytesLocked(id.TenantID, time.Now().Unix())
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="underlay-tunnels.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}

// underlayCSVImportRequest is one uploaded table. The file travels base64
// encoded inside JSON for two reasons: every console POST must carry a JSON
// body (requireSession refuses anything else, which is what keeps a cross-site
// form from posting here), and base64 is the only way to hand the bytes over
// intact so readUnderlayCSV can say "this is not UTF-8" about the real file
// instead of about a browser's guess at it.
type underlayCSVImportRequest struct {
	CSV   string `json:"csv"`
	Dry   bool   `json:"dry"`
	Prune bool   `json:"prune"`
}

// handleAdminUnderlayTunnelImport previews or applies an uploaded table.
// dry=true (the default) only reports; dry=false commits. Both run the same
// plan on the same state, so what the preview lists is what the commit writes.
func (s *server) handleAdminUnderlayTunnelImport(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	// The encoded form is ~4/3 of the file; the file limit itself is checked
	// after decoding so the number in the error message is bytes, not base64.
	var req underlayCSVImportRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, int64(underlayCSVMaxBytes*2)))
	if err := dec.Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("表格内容读不出来（文件上限 %d KiB）", underlayCSVMaxBytes>>10), http.StatusBadRequest)
		return
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(req.CSV))
	if err != nil {
		http.Error(w, "表格内容不是合法的 base64", http.StatusBadRequest)
		return
	}
	now := time.Now().Unix()

	s.mu.Lock()
	defer s.mu.Unlock()
	uc := s.underlayConfigLocked(id.TenantID, true)
	st := uc.State
	views := s.nodeViewsLocked(id.TenantID, now)

	specs, plan, err := planUnderlayCSV(st, views, raw, req.Prune)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if plan.Errors > 0 {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"ok": false, "dry": req.Dry, "plan": plan,
			"error": fmt.Sprintf("表格里有 %d 行不能执行；这一份没有写入任何改动", plan.Errors),
		})
		return
	}
	if req.Dry {
		writeJSON(w, map[string]any{"ok": true, "dry": true, "plan": plan})
		return
	}
	if len(specs) == 0 {
		// 一份只有表头的文件配上「以文件为准」= 删掉所有隧道；那是合法但危险的
		// 意图，由 prune 分支产生 specs，不会走到这里。空计划只可能是什么都
		// 没写，明说而不是回一句「成功」。
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"ok": false, "dry": false, "plan": plan,
			"error": "表格里没有可执行的改动（所有行都和现有隧道一致）",
		})
		return
	}

	work := st.Clone()
	if err := applyUnderlayCSV(work, specs, views, now); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"ok": false, "dry": false, "plan": plan, "error": err.Error()})
		return
	}
	*st = *work
	s.save()

	msg := fmt.Sprintf("按表格改隧道：新增 %d、修改 %d、删除 %d、不变 %d", plan.Add, plan.Update, plan.Delete, plan.Keep)
	s.appendOps("underlay.change", id.TenantID, 0, id.Username, "info", msg)
	log.Printf("underlay: tenant=%s imported tunnel table by %s (add %d, update %d, delete %d, keep %d)",
		id.TenantID, id.Username, plan.Add, plan.Update, plan.Delete, plan.Keep)

	writeJSON(w, map[string]any{
		"ok":       true,
		"dry":      false,
		"plan":     plan,
		"underlay": s.underlayViewLocked(id.TenantID, time.Now().Unix()),
	})
}
