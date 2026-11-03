package stun

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Mapping 是一次探测的结果：某个反射器看见的"我"，加上这个问题在本地用的源。
//
// Local 是**这台机器自己的**源地址（内核给这个 socket 选的，或绑死的那个），
// Addr/Port 是反射器看到的。NAT 后面这两个必然不同 —— 产品里两半各有用途：
// Addr 是"对端该拨哪儿"，Local 是"我从哪个口出去的"（见 underlay 的出口口判定）。
// 少了 Local，观测地址就只是一条不知道该落在哪个物理口上的结论。
type Mapping struct {
	Reflector string `json:"reflector"` // 哪个反射器答的 ip:port
	Local     string `json:"local"`     // 本 socket 的本地源地址 ip:port
	Addr      string `json:"addr"`      // 观测到的外层 IPv4
	Port      int    `json:"port"`      // 观测到的外层端口
	TxID      string `json:"txid"`      // hex；harness 用它对齐反射器自己记的那一条
}

// Verdict 是一轮完整发现的结果。
type Verdict struct {
	// Mappings 按 reflectors 的顺序，只含真正答了的。
	Mappings []Mapping
	// Symmetric 是对称 NAT 判定；只有 SymmetricKnown 为真时它才是测量结果。
	Symmetric      bool
	SymmetricKnown bool
	// Failed 是没答的反射器（地址 + 原因）。全都没答时 Discover 直接返回错误。
	Failed []string
}

// Prober 在**一个固定的 socket** 上连续探测。socket 必须复用：对称 NAT 的定义就是
// "同一个本地 socket 打不同目的地会得到不同的映射"，每轮新建 socket 会让这个
// 判定只能猜。
type Prober struct {
	conn net.PacketConn
	// bindIP 是绑定时明确给出的源地址；nil = INADDR_ANY，源地址由路由决定，
	// 每次探测前单独查（见 sourceFor）。
	bindIP net.IP
	// port 是这个 socket 的本地端口：所有目的地共用它，所以 LocalPort 报的是
	// 一个真实的、对端可以引用的数字，不是每轮漂移的临时端口。
	port int
}

// NewProber 建出探测用的 socket。localBind 是 "ip:port" 或 ":port"；":0" 让内核
// 选源端口，固定端口则用于端口守恒环境的复测。
func NewProber(localBind string) (*Prober, error) {
	if strings.TrimSpace(localBind) == "" {
		localBind = ":0"
	}
	laddr, err := net.ResolveUDPAddr("udp", localBind)
	if err != nil {
		return nil, fmt.Errorf("stun: resolve bind %s: %w", localBind, err)
	}
	conn, err := net.ListenPacket("udp", localBind)
	if err != nil {
		return nil, fmt.Errorf("stun: bind %s: %w", localBind, err)
	}
	la, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("stun: bind %s: 本地地址不是 UDP", localBind)
	}
	p := &Prober{conn: conn, port: la.Port}
	if laddr.IP != nil && !laddr.IP.IsUnspecified() {
		p.bindIP = laddr.IP
	}
	return p, nil
}

func (p *Prober) Close() error {
	if p == nil || p.conn == nil {
		return nil
	}
	return p.conn.Close()
}

// LocalPort 是这个探测 socket 的本地 UDP 端口，所有目的地共用。
func (p *Prober) LocalPort() int {
	if p == nil {
		return 0
	}
	return p.port
}

// sourceFor 查出"发往这个目的地时内核会用哪个本地源地址"。
//
// 做法是临时 connect 一个 UDP socket：connect 只做一次路由查询，不发任何报文，
// 关掉也不留端口。为什么不能直接读探测 socket 的 LocalAddr：绑 ":0"/INADDR_ANY
// 时它返回的是未指定地址，而出口口判定要的恰恰是那个被路由选出来的地址。
//
// 目的地路由不通时这里就返回错误（ENETUNREACH），而这正是必须让它响的场合：
// 一台 WAN 地址在 VPP 里、内核只有管理口的盒子，探测包会从管理口出去，得到的
// 映射描述的是一条永远不会承载隧道的路径。宁可报"探不了"，不能报一个错地址。
func (p *Prober) sourceFor(raddr *net.UDPAddr) (net.IP, error) {
	la := &net.UDPAddr{IP: p.bindIP} // nil IP = 未指定，让路由决定
	conn, err := net.DialUDP("udp", la, raddr)
	if err != nil {
		return nil, fmt.Errorf("本机没有到 %s 的路由（探测源地址查不出）: %w", raddr, err)
	}
	defer conn.Close()
	src, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil, fmt.Errorf("stun: 源地址不是 UDP（destination %s）", raddr)
	}
	v4 := src.IP.To4()
	if v4 == nil {
		return nil, fmt.Errorf("stun: 发往 %s 会走 IPv6 源地址 %s，反射器只观测 IPv4", raddr, src.IP)
	}
	return v4, nil
}

// Probe 对单个反射器发 Binding Request，直到收到 txid 对得上的回包或次数用尽。
func (p *Prober) Probe(ctx context.Context, reflector string, tries int, gap time.Duration) (Mapping, error) {
	if p == nil || p.conn == nil {
		return Mapping{}, fmt.Errorf("stun: prober is closed")
	}
	if tries < 1 {
		tries = 1
	}
	if gap <= 0 {
		gap = 300 * time.Millisecond
	}
	raddr, err := net.ResolveUDPAddr("udp", reflector)
	if err != nil {
		return Mapping{}, fmt.Errorf("stun: resolve %s: %w", reflector, err)
	}
	buf := make([]byte, 2048)
	// 源地址在发第一个包之前就查：查不出来就不要发，因为"发了没人回"和
	// "包会从错误的口出去"必须在日志里是两件事。
	srcIP, err := p.sourceFor(raddr)
	if err != nil {
		return Mapping{}, err
	}
	local := net.JoinHostPort(srcIP.String(), strconv.Itoa(p.port))
	for i := 1; i <= tries; i++ {
		if err := ctx.Err(); err != nil {
			return Mapping{}, err
		}
		txid := make([]byte, 12)
		if _, err := rand.Read(txid); err != nil {
			return Mapping{}, fmt.Errorf("stun: txid: %w", err)
		}
		if _, err := p.conn.WriteTo(BindingRequest(txid), raddr); err != nil {
			return Mapping{}, fmt.Errorf("stun: send to %s: %w", reflector, err)
		}
		if err := p.conn.SetReadDeadline(time.Now().Add(gap)); err != nil {
			return Mapping{}, fmt.Errorf("stun: read deadline: %w", err)
		}
		for {
			n, from, err := p.conn.ReadFrom(buf)
			if err != nil {
				break // 这一轮没等到，下一轮重发
			}
			// 目的地必须是我们自己发过的那一个：一条能发 Binding Request 给任何人
			// 的 socket，也会被别人的应答敲门，把它当反射器答案会让映射变成噪声。
			if !sameAddr(from, raddr) {
				continue
			}
			ip, port, ok := MappedAddress(buf[:n], txid)
			if !ok {
				continue // 别人家的事务
			}
			return Mapping{
				Reflector: reflector, Local: local,
				Addr: ip, Port: port, TxID: hex.EncodeToString(txid),
			}, nil
		}
		if i < tries {
			select {
			case <-ctx.Done():
				return Mapping{}, ctx.Err()
			case <-time.After(gap):
			}
		}
	}
	return Mapping{}, fmt.Errorf("stun: %d 次探测都没在 %v 内收到 %s 的匹配回包", tries, gap, reflector)
}

// Discover 用同一个 socket 逐个探测这些反射器，并据此给出对称 NAT 判定。
//
// 判定规则就一条：同一本地 socket 打到不同反射器，观测到的 (ip:port) 只要有一对
// 不同，就是对称 NAT —— 它对端每个目的地会得到一个新映射，所以任何"别人按这个
// 映射来拨我"的隧道都不可能成立。少于两个反射器答了就没有判定可给，SymmetricKnown
// 留在 false，控制器不许把它当成"非对称"。
func (p *Prober) Discover(ctx context.Context, reflectors []string, tries int, gap time.Duration) (Verdict, error) {
	v := Verdict{Mappings: []Mapping{}, Failed: []string{}}
	seen := map[string]bool{}
	for _, ref := range reflectors {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		m, err := p.Probe(ctx, ref, tries, gap)
		if err != nil {
			v.Failed = append(v.Failed, fmt.Sprintf("%s: %v", ref, err))
			continue
		}
		v.Mappings = append(v.Mappings, m)
	}
	if len(v.Mappings) == 0 {
		return v, fmt.Errorf("stun: 没有任何反射器应答（%s）", strings.Join(v.Failed, "；"))
	}
	if len(v.Mappings) >= 2 {
		first := v.Mappings[0]
		v.SymmetricKnown = true
		for _, m := range v.Mappings[1:] {
			if m.Addr != first.Addr || m.Port != first.Port {
				v.Symmetric = true
				break
			}
		}
	}
	return v, nil
}

func sameAddr(a net.Addr, want *net.UDPAddr) bool {
	udp, ok := a.(*net.UDPAddr)
	return ok && udp != nil && udp.Port == want.Port && udp.IP.String() == want.IP.String()
}
