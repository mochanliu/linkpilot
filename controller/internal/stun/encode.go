package stun

import (
	"encoding/binary"
	"fmt"
	"net"
)

// Response 编码一条 Binding Success Response。
//
// ip/port 是 reflector 从**数据报真实源**读到的地址，不是报文里带来的任何东西：
// STUN 的全部意义就是"第三方看到的你"，所以这里绝不回读请求里的地址字段。
// txid 原样回填，客户端靠它认回包。
func Response(txid []byte, ip string, port int) ([]byte, error) {
	if len(txid) != 12 {
		return nil, fmt.Errorf("txid must be 12 bytes, got %d", len(txid))
	}
	addr := net.ParseIP(ip)
	if addr == nil {
		return nil, fmt.Errorf("bad observed address %q", ip)
	}
	v4 := addr.To4()
	if v4 == nil {
		return nil, fmt.Errorf("only IPv4 observations are encoded, got %q", ip)
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("bad observed port %d", port)
	}
	magicCookie := binary.BigEndian.Uint32(magic[:])

	// 属性 = 4 字节头 + XOR-MAPPED-ADDRESS 的 8 字节值，所以报文总长 32。
	out := make([]byte, 20+4+8)
	out[0], out[1] = 0x01, 0x01 // class=01 (success), method=1 (binding)
	out[2], out[3] = 0x00, 0x08 // payload length（属性头的 4 字节不算在内）
	copy(out[4:8], magic[:])
	copy(out[8:20], txid)

	p := 20
	out[p], out[p+1] = 0x00, attrXorMappedAddress
	out[p+2], out[p+3] = 0x00, 0x08
	out[p+4] = 0x00 // 保留字节
	out[p+5] = 0x01 // family = IPv4
	binary.BigEndian.PutUint16(out[p+6:p+8], uint16(port)^uint16(magicCookie>>16))
	binary.BigEndian.PutUint32(out[p+8:p+12], binary.BigEndian.Uint32(v4)^magicCookie)
	return out, nil
}

// IsBindingRequest 判断这条数据报是不是"可以回答的" Binding Request。
//
// 只认三件事：报文头够长、type 是 Binding Request、magic cookie 对得上。
// 属性一个都不解析 —— 请求里带来的任何地址字段都不参与回答，解析它们只会把
// "客户端自称的源地址"变成 reflector 观测的一部分，那正是这个协议要避免的。
func IsBindingRequest(data []byte) bool {
	if len(data) < 20 {
		return false
	}
	if data[0] != 0x00 || data[1] != 0x01 {
		return false
	}
	for i := 0; i < 4; i++ {
		if data[4+i] != magic[i] {
			return false
		}
	}
	// 长度字段必须落在报文里，否则这是条截断/伪造的报文，回它只会放大垃圾。
	l := int(binary.BigEndian.Uint16(data[2:4]))
	return 20+l <= len(data)
}
