package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"testing"
)

// Reconstructs the cust12 test client's DISCOVER byte-for-byte and walks
// parseDHCP + handleDHCP to see where the reply is lost. Diagnostic only.
func TestDebugDiscover(t *testing.T) {
	xid := uint32(0xc9b01aec)
	macb := []byte{0x08, 0x00, 0x27, 0x46, 0x1d, 0xc3}
	pkt := []byte{}
	pkt = append(pkt, 1, 1, 6, 0)
	var xb [4]byte
	binary.BigEndian.PutUint32(xb[:], xid)
	pkt = append(pkt, xb[:]...)
	pkt = append(pkt, 0, 0, 0, 0)          // secs flags
	pkt = append(pkt, make([]byte, 16)...) // ciaddr yiaddr siaddr giaddr
	pkt = append(pkt, macb...)
	pkt = append(pkt, make([]byte, 16-len(macb))...)
	pkt = append(pkt, make([]byte, 64)...)  // sname
	pkt = append(pkt, make([]byte, 128)...) // file
	pkt = append(pkt, 0x63, 0x82, 0x53, 0x63)
	pkt = append(pkt, 53, 1, 1)
	pkt = append(pkt, 55, 5, 1, 3, 6, 15, 42)
	pkt = append(pkt, 255)
	fmt.Printf("total=%d magic@236=% x opt53@240=% x\n", len(pkt), pkt[236:240], pkt[240:243])

	p := parseDHCP(pkt)
	if p == nil {
		t.Fatal("parseDHCP returned nil")
	}
	fmt.Printf("parsed: op=%d xid=%x flags=%d msgType=%v\n", p.op, p.xid, p.flags, p.options[53])

	s := &server{
		leases: map[string]*lease{},
		log:    log.New(os.Stdout, "dbg: ", 0),
		stop:   map[string]chan struct{}{},
	}
	row := DHCPConfig{VPN: 0, VPP: "host-enp0s8", LCP: "lcp-x0", Listen: "172.31.12.1",
		Prefix: 24, Start: "172.31.12.100", End: "172.31.12.150", LeaseSec: 86400,
		Router: "172.31.12.1", DNS: "172.31.12.1"}
	resp := s.handleDHCP(row, p)
	fmt.Printf("handleDHCP resp=%v\n", resp)
	if resp != nil {
		out, dst := resp.encode(row)
		fmt.Printf("encode: len=%d dst=%s op=%d yiaddr=% x opt53=% x opt54=% x opt1=% x opt3=% x\n",
			len(out), dst, out[0], out[16:20], out[240:243], out[243:248], out[251:256], out[258:263])
	}
}
