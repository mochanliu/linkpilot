package main

import (
	"net"
	"strings"
)

// advertiseAddr 回答"设备与 RR 该用哪个地址找我"。
//
// -listen 只是绑定：0.0.0.0:8444 表示"所有网卡都收"，它不是任何人能拨的地址。
// 集群里这个值来自成员表的 Endpoint，单机没有成员表，只能从下面几处推：
//
//  1. -advertise：显式指定，最优先（放在 NAT 后面、端口映射过的现场必须用它）；
//  2. -tls-san 的第一项：证书里设备要校验的那个名字本来就得是这个地址，声明一次
//     两处生效；
//  3. -listen 的主机部分（当它不是通配值）；
//  4. 默认路由出口的本机地址（UDP dial 只做路由查找，不发包）；
//  5. 实在推不出来就用 -listen 原样——宁可给一个可疑值让日志能看出问题，也不要
//     静默留空：空地址会让设备地址表里少一项，症状是"心跳正常但永远切不过去"。
//
// localhost / 回环 / 通配一律跳过：它们对"另一台机器上的设备"没有意义。
func advertiseAddr(listen string, sans []string, explicit string) string {
	defPort := portOf(listen)
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		return hostPort(explicit, defPort)
	}
	for _, s := range sans {
		s = strings.TrimSpace(s)
		if s == "" || !usableHost(hostOf(s)) {
			continue
		}
		return hostPort(s, defPort)
	}
	if h := hostOf(listen); usableHost(h) {
		return net.JoinHostPort(h, defPort)
	}
	if ip := localOutboundIP(); ip != "" {
		return net.JoinHostPort(ip, defPort)
	}
	return hostPort(hostOf(listen), defPort)
}

// usableHost 排除"对端拨不到"的那些值。host.docker.internal 留在排除表里是因为
// 它只解析得到容器自己的宿主地址，写进证书 SAN 是历史包袱，不该当宣告地址用。
func usableHost(h string) bool {
	switch strings.ToLower(strings.Trim(h, "[]")) {
	case "", "0.0.0.0", "::", "localhost", "127.0.0.1", "::1", "host.docker.internal":
		return false
	}
	return true
}

// hostOf 取 "host:port" / "[v6]:port" / "host" 里的 host，去掉 IPv6 的方括号。
func hostOf(s string) string {
	s = strings.TrimSpace(s)
	if h, _, err := net.SplitHostPort(s); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(s, "[]")
}

// portOf 取监听端口，取不到就用控制面的默认端口。
func portOf(s string) string {
	if _, p, err := net.SplitHostPort(strings.TrimSpace(s)); err == nil && p != "" {
		return p
	}
	return "8444"
}

// hostPort 把不带端口的地址补上默认端口；带端口的原样保留（IPv6 重新加括号）。
func hostPort(s, defPort string) string {
	if h, p, err := net.SplitHostPort(strings.TrimSpace(s)); err == nil && p != "" {
		return net.JoinHostPort(strings.Trim(h, "[]"), p)
	}
	return net.JoinHostPort(strings.Trim(strings.TrimSpace(s), "[]"), defPort)
}

// localOutboundIP 是"默认路由出口的本机地址"。UDP dial 不握手、不发包，只做一次
// 路由查找；没有默认路由（隔离网段）时返回空。
func localOutboundIP() string {
	c, err := net.Dial("udp4", "192.0.2.1:9") // TEST-NET-1，永远不该有回包
	if err != nil {
		return ""
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok && a.IP != nil && !a.IP.IsUnspecified() {
		return a.IP.String()
	}
	return ""
}
