// Package vppapi 是一个只依赖标准库的 VPP binary API 客户端。
//
// 为什么要自己写：控制器侧要求零第三方依赖（见 go.mod），govpp 会把模块的 go
// 指令抬到 1.24，而且这里要的是能塞进 agent 的最小通路 —— /run/vpp/api.sock。
// 格式不是猜的：每条都在 srwg-node1（VPP 26.06）上实测过，字节数与机理记在
// controller/test/scripts/vpp-api-probe.py 和 vpp-api-probe2.py 两个探针里，
// 改这个包之前先跑探针。
//
// 四条会咬人的实测事实：
//
//  1. 帧前缀 16 字节 = u64 msg_id(恒 0) + u32 length + u32 flags；length 只覆盖
//     API 消息本身，消息自己再以 u16 msg_id 开头。
//  2. 后续请求的 client_index 必须用注册应答里的 **index** 字段（实测
//     0x80000001/0x80000002），不是应答头里的 client_index（那个恒为 0，
//     用它只会一直超时）。
//  3. dump 没有结束标记：结果集为空时 VPP **一帧都不发**（本地观测表 0 条时
//     sr_wg_obs_dump 就是彻底静默，和"消息没送达"一模一样）。所以 dump 拿一条
//     显式应答的请求当栅栏（barrier），栅栏回来说明前面的 details 已经发完。
//  4. 请求头与应答头不对称：请求 = msg_id+client_index+context（context 在第 6
//     字节），应答 = msg_id+context（context 在第 2 字节）；注册请求
//     (vl_api_sockclnt_create_t) 又没有 client_index。偏移搞混时客户端只会安静地
//     超时，而 VPP 那边一切正常 —— 本包的测试第一版就是这么挂住的。
//
// 另外这里刻意"只认预期 msg_id 的应答"。本插件曾经因为没定义 REPLY_MSG_ID_BASE
// 而把应答写成插件内部下标（数据面执行了、应答认不出），客户端对这类 bug 的正确
// 行为是超时并计数，而不是把错帧当好帧收下。
package vppapi

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	frameHeaderLen   = 16
	sockclntCreate   = 15
	sockclntReplyID  = 16
	clientNameLen    = 64
	sockclntReplyLen = 20
	tableEntryLen    = 66

	// 请求头与应答头**不对称**：请求带 client_index，应答不带，
	// 于是 context 一个在第 6 字节、一个在第 2 字节。
	reqHeaderLen   = 10 // u16 msg_id + u32 client_index + u32 context
	replyHeaderLen = 6  // u16 msg_id + u32 context

	detailQueueLen = 256
	registerBudget = 16 * 1024 * 1024

	// vl_api_address_t = u8 af + union{ip4[4], ip6[16]}，实测 17 字节，不是 8。
	addressAPILen       = 17
	apiAddressFamilyIP4 = 0
	apiAddressFamilyIP6 = 1
)

// ErrClosed 表示连接已经断了（读循环退出）。
var ErrClosed = errors.New("vppapi: 连接已关闭")

// Client 是一条到 VPP api.sock 的连接，可并发使用：所有请求按 context 分发，
// 由单个读循环喂。异步事件按 msg_id 分发，同样由这一个读循环喂。
type Client struct {
	conn        net.Conn
	clientIndex uint32
	apiSock     string // api.sock 路径；IfStats 用它定位同目录的 stats.sock

	ids  map[string]uint16
	crcs map[string]string

	mu      sync.Mutex
	pending map[uint32]*waiter

	// asyncs 按 msg_id 收异步事件订阅者（见 WatchBFDEvents）。它和 pending 是两套
	// 索引：事件帧头里第 2 字节起是 VPP 盖的 client_index，不是 context，按 context
	// 永远找不到等待者，只会被计入 drops。
	asyncs map[uint16]func([]byte)

	nextCtx atomic.Uint32
	rx      chan struct{} // close 当读循环退出
	closed  chan struct{} // close 当 Close() 被调用
	drops   atomic.Uint64 // 收到但没人等的帧（异步通知、错 id 应答）
	err     atomic.Pointer[error]

	// stats 是惰性打开的 stats 段客户端（见 statseg.go）。一条 Client 对应一个
	// VPP 进程的一生（agent 在 VPP 重启后换新连接），所以这里缓存一次就够；
	// 读数瞬时失败（目录重建）不弃缓存，下个窗口重试。
	statsMu sync.Mutex
	stats   *StatSeg
}

type waiter struct {
	ch   chan []byte
	want map[uint16]bool
	// full 表示投递时队列已满、有帧被丢掉。只有 dump 会填满队列，
	// 单应答的 call 用不到它。
	full atomic.Bool
}

// APIError 是 VPP 在应答里回的 retval（i32，0 = 成功）。
type APIError struct {
	Op     string
	Retval int32
}

func (e APIError) Error() string {
	return fmt.Sprintf("vppapi: %s 失败，VPP retval=%d", e.Op, e.Retval)
}

// Dial 连上 api.sock、完成 sockclnt_create 注册、取回消息表并校验版本。
// appName 会出现在 VPP 的 show api clients 里，用它标识 agent 自己。
func Dial(ctx context.Context, socketPath, appName string) (*Client, error) {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("vppapi: 连接 %s: %w", socketPath, err)
	}

	c := &Client{
		conn:    conn,
		apiSock: socketPath,
		ids:     map[string]uint16{},
		crcs:    map[string]string{},
		pending: map[uint32]*waiter{},
		asyncs:  map[uint16]func([]byte){},
		rx:      make(chan struct{}),
		closed:  make(chan struct{}),
	}
	if err := c.register(ctx, appName); err != nil {
		conn.Close()
		return nil, err
	}
	if err := checkVersions(c.ids, c.crcs, requiredVersions); err != nil {
		conn.Close()
		return nil, err
	}
	go c.readLoop()
	return c, nil
}

// Close 关掉连接；在途请求与后续调用拿到 ErrClosed（不是挂到超时）。
func (c *Client) Close() error {
	if s := func() *StatSeg {
		c.statsMu.Lock()
		defer c.statsMu.Unlock()
		s := c.stats
		c.stats = nil
		return s
	}(); s != nil {
		s.Close()
	}
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	c.conn.SetDeadline(time.Now())
	return c.conn.Close()
}

// IfStats 读全部接口的 rx/tx 累计计数（statseg.go 的 stats 共享内存段）。
// 段客户端在第一次调用时惰性打开（api.sock 同目录的 stats.sock），随 Client
// 一起关闭。ctx 只管第一次打开的拨号预算，读本身是本地内存操作。
func (c *Client) IfStats(ctx context.Context) ([]IfCounter, error) {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	if c.stats == nil {
		s, err := OpenStatSeg(statsSocketPath(c.apiSock))
		if err != nil {
			return nil, err
		}
		c.stats = s
	}
	return c.stats.IfStats()
}

// statsSocketPath 由 api.sock 推 stats.sock 的路径（同一运行目录）。
func statsSocketPath(apiSock string) string {
	return filepath.Join(filepath.Dir(apiSock), "stats.sock")
}

// Drops 返回收到但没人等的帧数，排障用。
func (c *Client) Drops() uint64 { return c.drops.Load() }

// MsgID 暴露已解析的消息号，测试与排障用。
func (c *Client) MsgID(name string) (uint16, bool) {
	id, ok := c.ids[name]
	return id, ok
}

func (c *Client) register(ctx context.Context, appName string) error {
	if len(appName) >= clientNameLen {
		return fmt.Errorf("vppapi: 客户端名太长: %s", appName)
	}
	name := make([]byte, clientNameLen)
	copy(name, appName)

	payload := make([]byte, 0, 6+clientNameLen)
	var h [6]byte
	binary.BigEndian.PutUint16(h[0:2], sockclntCreate)
	binary.BigEndian.PutUint32(h[2:6], 0x52454701) // context，注册阶段自己认
	payload = append(payload, h[:]...)
	payload = append(payload, name...)

	if err := writeFrame(c.conn, payload); err != nil {
		return fmt.Errorf("vppapi: 发送 sockclnt_create: %w", err)
	}

	// 注册阶段读循环还没起来，同步读到一整帧为止。
	if err := c.setReadDeadline(ctx); err != nil {
		return err
	}
	defer c.conn.SetReadDeadline(time.Time{})

	buf := make([]byte, 0, 1<<16)
	tmp := make([]byte, 8192)
	for {
		n, err := c.conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) > registerBudget {
				return fmt.Errorf("vppapi: 注册应答超出 %d 字节仍未收齐，已放弃", registerBudget)
			}
			if len(buf) >= frameHeaderLen {
				length := int(binary.BigEndian.Uint32(buf[8:12]))
				if length > 0 && len(buf) >= frameHeaderLen+length {
					return c.parseRegister(buf[frameHeaderLen : frameHeaderLen+length])
				}
			}
		}
		if err != nil {
			return fmt.Errorf("vppapi: 读 sockclnt_create_reply: %w", err)
		}
	}
}

func (c *Client) setReadDeadline(ctx context.Context) error {
	if dl, ok := ctx.Deadline(); ok {
		return c.conn.SetReadDeadline(dl)
	}
	return c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
}

func (c *Client) parseRegister(body []byte) error {
	if len(body) < sockclntReplyLen {
		return fmt.Errorf("vppapi: sockclnt_create_reply 太短 (%d B)", len(body))
	}
	if msgID := binary.BigEndian.Uint16(body[0:2]); msgID != sockclntReplyID {
		return fmt.Errorf("vppapi: 期望注册应答(16)，收到 msg_id=%d", msgID)
	}
	if response := int32(binary.BigEndian.Uint32(body[10:14])); response != 0 {
		return fmt.Errorf("vppapi: 注册被拒绝 response=%d", response)
	}
	// body: u16 msg_id, u32 client_index, u32 context, i32 response, u32 index, u16 count
	c.clientIndex = binary.BigEndian.Uint32(body[14:18])
	count := int(binary.BigEndian.Uint16(body[18:20]))
	if len(body) != sockclntReplyLen+count*tableEntryLen {
		return fmt.Errorf("vppapi: 消息表长度不符: %d != %d", len(body), sockclntReplyLen+count*tableEntryLen)
	}
	for i := 0; i < count; i++ {
		at := sockclntReplyLen + i*tableEntryLen
		id := binary.BigEndian.Uint16(body[at : at+2])
		full := cstring(body[at+2 : at+tableEntryLen])
		base, crc := splitName(full)
		if base == "" {
			continue
		}
		c.ids[base] = id
		c.crcs[base] = crc
	}
	if len(c.ids) == 0 {
		return errors.New("vppapi: 消息表为空")
	}
	return nil
}

func (c *Client) readLoop() {
	defer close(c.rx)
	buf := make([]byte, 0, 1<<16)
	tmp := make([]byte, 8192)
	for {
		n, err := c.conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			consumed := 0
			for len(buf)-consumed >= frameHeaderLen {
				length := int(binary.BigEndian.Uint32(buf[consumed+8 : consumed+12]))
				if length == 0 || len(buf)-consumed-frameHeaderLen < length {
					break
				}
				c.dispatch(buf[consumed+frameHeaderLen : consumed+frameHeaderLen+length])
				consumed += frameHeaderLen + length
			}
			buf = append([]byte(nil), buf[consumed:]...)
		}
		if err != nil {
			e := err
			c.err.Store(&e)
			c.failPending()
			return
		}
	}
}

// dispatch 按 context 找等待者，并且只把 msg_id 在它预期集合里的帧交出去。
//
// 投递必须在 c.mu 里完成：failPending 会 close 这些 channel，锁外发送就可能在
// 对已关闭 channel 发送时 panic —— 连接断开的一瞬间恰好有应答到达即是触发点。
// 这里只做非阻塞投递，持锁时间是常数。
//
// 异步事件走另一条路：帧头第 2 字节是 VPP 盖的 client_index 而不是 context，
// 按 msg_id 找订阅者。回调**必须在放锁之后**调 —— 回调里的人下一步往往就是
// 发一条请求（重新 dump 会话），那条请求要拿 c.mu 去 arm，锁内回调就是死锁。
func (c *Client) dispatch(frame []byte) {
	if len(frame) < replyHeaderLen {
		c.drops.Add(1)
		return
	}
	// 应答头里没有 client_index，context 就在第 2 字节。
	msgID := binary.BigEndian.Uint16(frame[0:2])
	ctxVal := binary.BigEndian.Uint32(frame[2:6])

	c.mu.Lock()
	async := c.asyncs[msgID]
	if async == nil {
		w := c.pending[ctxVal]
		if w == nil || !w.want[msgID] {
			c.drops.Add(1)
		} else {
			select {
			case w.ch <- append([]byte(nil), frame...):
			default:
				// 队列满了：宁可比 expected 多，也绝不静默少给 —— 截断过的 dump
				// 看起来就是一份合法但不全的配置，那种错误没人会去复查。
				w.full.Store(true)
				c.drops.Add(1)
			}
		}
	}
	c.mu.Unlock()

	if async != nil {
		// 交给回调的是副本：frame 指向 readLoop 的缓冲区，下一轮就被覆写了。
		async(append([]byte(nil), frame...))
	}
}

func (c *Client) failPending() {
	c.mu.Lock()
	ps := c.pending
	c.pending = map[uint32]*waiter{}
	c.mu.Unlock()
	for _, w := range ps {
		close(w.ch)
	}
}

// send 组装一条请求：msg_id + client_index + context + 消息体，外面再套帧前缀。
func (c *Client) send(msgID uint16, ctxVal uint32, body []byte) error {
	payload := make([]byte, 0, reqHeaderLen+len(body))
	var h [reqHeaderLen]byte
	binary.BigEndian.PutUint16(h[0:2], msgID)
	binary.BigEndian.PutUint32(h[2:6], c.clientIndex)
	binary.BigEndian.PutUint32(h[6:10], ctxVal)
	payload = append(payload, h[:]...)
	payload = append(payload, body...)
	return writeFrame(c.conn, payload)
}

// call 发一条有显式应答的请求，回应答帧。
func (c *Client) call(ctx context.Context, name, replyName string, body []byte) ([]byte, error) {
	reqID, err := c.id(name)
	if err != nil {
		return nil, err
	}
	replyID, err := c.id(replyName)
	if err != nil {
		return nil, err
	}

	ctxVal := c.nextCtx.Add(1)
	w := &waiter{ch: make(chan []byte, 1), want: map[uint16]bool{replyID: true}}
	c.arm(ctxVal, w)
	defer c.disarm(ctxVal)

	if err := c.send(reqID, ctxVal, body); err != nil {
		return nil, c.sendErr(err)
	}
	select {
	case frame, ok := <-w.ch:
		if !ok {
			return nil, c.closedErr()
		}
		return frame, nil
	case <-c.rx:
		return nil, c.closedErr()
	case <-ctx.Done():
		return nil, fmt.Errorf("vppapi: %s 未完成(超时/取消)，可能是应答 msg_id 不对: %w", name, ctx.Err())
	}
}

// callStream 发一条"先流式回 details、最后回一条自带结束标记的 reply"的请求
// （vpp 里带 cursor 的 get，例如 mss_clamp_get），把 details 与 reply 一起交出去。
//
// 和 dump 的区别：dump 没有结束标记，所以必须拿屏障消息当"发完了"；这里的 reply
// 本身就是结束标记，多读一帧屏栅栏反而会和 cursor 分页打架。细节顺序是 VPP 保证
// 的（同一请求的处理在单线程上跑完：先发完本页 details，再发 reply）。
func (c *Client) callStream(ctx context.Context, name, detailName, replyName string, body []byte) ([][]byte, []byte, error) {
	reqID, err := c.id(name)
	if err != nil {
		return nil, nil, err
	}
	detailID, err := c.id(detailName)
	if err != nil {
		return nil, nil, err
	}
	replyID, err := c.id(replyName)
	if err != nil {
		return nil, nil, err
	}

	ctxVal := c.nextCtx.Add(1)
	w := &waiter{ch: make(chan []byte, detailQueueLen),
		want: map[uint16]bool{detailID: true, replyID: true}}
	c.arm(ctxVal, w)
	defer c.disarm(ctxVal)

	if err := c.send(reqID, ctxVal, body); err != nil {
		return nil, nil, c.sendErr(err)
	}
	var details [][]byte
	for {
		select {
		case frame, ok := <-w.ch:
			if !ok {
				return details, nil, c.closedErr()
			}
			if len(frame) >= replyHeaderLen && binary.BigEndian.Uint16(frame[0:2]) == replyID {
				return details, frame, nil
			}
			details = append(details, frame)
		case <-c.rx:
			return details, nil, c.closedErr()
		case <-ctx.Done():
			// 队列满时 reply 可能被丢掉 —— 那和"VPP 没回"要分开报，否则排障时会
			// 去怀疑对端而真正的原因是本地缓冲不够。
			if w.full.Load() {
				return details, nil, fmt.Errorf("vppapi: %s 的结果多于 %d 条，已被截断: %w",
					name, detailQueueLen, ctx.Err())
			}
			return details, nil, fmt.Errorf("vppapi: %s 的 details/reply 没读齐(超时/取消): %w", name, ctx.Err())
		}
	}
}

// dump 发一条 stream 请求，用 sr_wg_stats_get 当栅栏把 details 收齐。
//
// 为什么必须栅栏：VPP 的 dump 没有"发完了"这一帧，空结果干脆一帧不发。同一注册
// 上的消息按 FIFO 处理，所以后发一条显式应答并等它回来，就等价于"前面那条 dump
// 的所有 details 都已经进来了"。
func (c *Client) dump(ctx context.Context, name, detailName string, body []byte) ([][]byte, error) {
	reqID, err := c.id(name)
	if err != nil {
		return nil, err
	}
	detailID, err := c.id(detailName)
	if err != nil {
		return nil, err
	}
	barrierID, err := c.id(barrierMsg)
	if err != nil {
		return nil, err
	}
	barrierReplyID, err := c.id(barrierReply)
	if err != nil {
		return nil, err
	}

	dumpCtx := c.nextCtx.Add(1)
	barrierCtx := c.nextCtx.Add(1)
	dw := &waiter{ch: make(chan []byte, detailQueueLen), want: map[uint16]bool{detailID: true}}
	bw := &waiter{ch: make(chan []byte, 1), want: map[uint16]bool{barrierReplyID: true}}
	c.arm(dumpCtx, dw)
	c.arm(barrierCtx, bw)
	defer c.disarm(dumpCtx, barrierCtx)

	if err := c.send(reqID, dumpCtx, body); err != nil {
		return nil, c.sendErr(err)
	}
	if err := c.send(barrierID, barrierCtx, nil); err != nil {
		return nil, c.sendErr(err)
	}
	select {
	case _, ok := <-bw.ch:
		if !ok {
			return nil, c.closedErr()
		}
	case <-c.rx:
		return nil, c.closedErr()
	case <-ctx.Done():
		return nil, fmt.Errorf("vppapi: %s 栅栏未回(超时/取消): %w", name, ctx.Err())
	}

	var out [][]byte
	for {
		select {
		case frame, ok := <-dw.ch:
			if !ok {
				return out, c.closedErr()
			}
			out = append(out, frame)
		default:
			// 栅栏已经回来，通道里没有别帧了；此刻 full 就代表真丢过。
			if dw.full.Load() {
				return nil, fmt.Errorf("vppapi: %s 的结果多于 %d 条，已被截断（这个包不该继续用它）",
					name, detailQueueLen)
			}
			return out, nil
		}
	}
}

func (c *Client) arm(ctxVal uint32, w *waiter) {
	c.mu.Lock()
	c.pending[ctxVal] = w
	c.mu.Unlock()
}

func (c *Client) disarm(ctxVals ...uint32) {
	c.mu.Lock()
	for _, v := range ctxVals {
		delete(c.pending, v)
	}
	c.mu.Unlock()
}

func (c *Client) id(name string) (uint16, error) {
	if id, ok := c.ids[name]; ok {
		return id, nil
	}
	return 0, fmt.Errorf("vppapi: VPP 消息表里没有 %s（插件与本客户端版本不匹配）", name)
}

func (c *Client) closedErr() error {
	if p := c.err.Load(); p != nil {
		return fmt.Errorf("%w: %v", ErrClosed, *p)
	}
	return ErrClosed
}

// sendErr 把写失败归一成 ErrClosed：连接已经没了时，调用方该看到的是"连接没了"，
// 而不是 runtime 的 "use of closed network connection"（那种文本没法用 errors.Is 判）。
//
// "连接没了"有两条路，两条都必须归一：
//
//   - 我们自己调过 Close() —— 这条看得见 c.closed；
//   - **对端**（VPP 重启）关掉了 API 连接 —— c.closed 没关，写回来的是裸 errno
//     （实测 `write unix @->/run/vpp/api.sock: write: broken pipe`）。
//
// 第二条以前原样抛出去，于是 agent 的自愈判定（errors.Is(err, ErrClosed)）永远不成立：
// 它拿着一条死句柄每轮撞同一个 errno，退避从 5s 一路涨到 5min，盒子七轮都没重建隧道
// （2026-11-05 VM 台阶段 C 实测）。读循环这条路会把原因记进 c.err，一并算数。
func (c *Client) sendErr(err error) error {
	select {
	case <-c.closed:
		return fmt.Errorf("%w: %v", ErrClosed, err)
	default:
	}
	if c.err.Load() != nil || connGone(err) {
		return fmt.Errorf("%w: %v", ErrClosed, err)
	}
	return err
}

// connGone 判断一次 IO 错误是不是"这条连接已经不在了"。只列传输层的死法，
// 不把 retval 非 0、消息表缺项这类"连接好好的、请求本身有问题"算进来 ——
// 那些必须原样留着，否则调用方会把配置错误当成断连去重连。
func connGone(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	for _, no := range []syscall.Errno{
		syscall.EPIPE, syscall.ENOTCONN, syscall.ECONNRESET, syscall.ECONNABORTED, syscall.ESHUTDOWN,
	} {
		if errors.Is(err, no) {
			return true
		}
	}
	return false
}

// retval 取出应答里的 i32 错误码；非 0 转成 APIError。
func (c *Client) retval(op string, frame []byte) error {
	rv, err := Retval(frame)
	if err != nil {
		return err
	}
	if rv != 0 {
		return APIError{Op: op, Retval: rv}
	}
	return nil
}

// Retval 取出应答第 6 字节起的 i32（autoreply 应答都是 msg_id+context+retval）。
func Retval(frame []byte) (int32, error) {
	if len(frame) < 10 {
		return 0, errors.New("vppapi: 应答太短")
	}
	return int32(binary.BigEndian.Uint32(frame[6:10])), nil
}

func writeFrame(conn net.Conn, payload []byte) error {
	h := make([]byte, frameHeaderLen)
	// msg_id 字段在收/发两个方向都是 0，真正的消息号在 payload 里（实测）。
	binary.BigEndian.PutUint64(h[0:8], 0)
	binary.BigEndian.PutUint32(h[8:12], uint32(len(payload)))
	binary.BigEndian.PutUint32(h[12:16], 0)
	_, err := conn.Write(append(h, payload...))
	return err
}

func cstring(b []byte) string {
	for i, x := range b {
		if x == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// splitName 把 "sr_wg_key_set_e7289514" 拆成 base 与 CRC。CRC 是 VPP 对消息结构
// 算的版本号，结构一变就变；客户端据此拒绝继续发尺寸不对的包（VPP 对短包是静默
// 丢弃，只在日志里留一行 Truncated message）。
func splitName(full string) (base, crc string) {
	i := lastByte(full, '_')
	if i < 0 {
		return full, ""
	}
	return full[:i], full[i+1:]
}

func lastByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}
