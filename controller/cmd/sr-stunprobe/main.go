// sr-stunprobe -- 手工验证 STUN 链路的最小探针。
//
// 它把 Binding Request 从**自己新建的内核 UDP socket** 发出去，读回报文并解码
// XOR-MAPPED-ADDRESS。因此它证明的是"reflector 观测到什么、客户端能解码什么"，
// **不代表数据面那个 socket 的映射**：探针从哪个 socket 出、回包从哪里读，是
// vBOND 接入 VPP 时待定的接缝。
//
//	./sr-stunprobe -reflector 10.10.0.5:3478
//	./sr-stunprobe -reflector 10.10.0.5:3479 -local :51820   # 固定源端口再探一次
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"time"

	"sdwan/controller/internal/stun"
)

func main() {
	var (
		reflector = flag.String("reflector", "", "reflector 地址 ip:port（必填）")
		local     = flag.String("local", ":0", "本地绑定地址；:0 = 让内核选源端口")
		tries     = flag.Int("tries", 5, "探测次数")
		gap       = flag.Duration("gap", 300*time.Millisecond, "两次探测之间的间隔")
	)
	flag.Parse()

	if *reflector == "" {
		log.Fatal("-reflector ip:port 必填")
	}
	conn, err := net.ListenPacket("udp", *local)
	if err != nil {
		log.Fatalf("bind %s: %v", *local, err)
	}
	defer conn.Close()
	raddr, err := net.ResolveUDPAddr("udp", *reflector)
	if err != nil {
		log.Fatalf("resolve %s: %v", *reflector, err)
	}

	// 一旦指定了源端口，后续每一轮都复用同一个 socket：symmetric-NAT 判定的
	// 前提就是"同一个本地 socket，打两个不同目的地"。
	for i := 1; i <= *tries; i++ {
		txid := make([]byte, 12)
		for j := range txid {
			txid[j] = byte(rand.Intn(256))
		}
		if _, err := conn.WriteTo(stun.BindingRequest(txid), raddr); err != nil {
			log.Printf("probe %d: send: %v", i, err)
			time.Sleep(*gap)
			continue
		}
		deadline := time.Now().Add(*gap)
		if err := conn.SetReadDeadline(deadline); err != nil {
			log.Fatalf("set read deadline: %v", err)
		}
		buf := make([]byte, 2048)
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				break // 本轮超时，下一轮重发
			}
			ip, port, ok := stun.MappedAddress(buf[:n], txid)
			if !ok {
				continue // 别人家的事务，丢弃
			}
			// txid 一起打印：harness 用它把这一行和 reflector 自己记下的观测
			// （/tmp/stun-observed.log 里同一 txid 的 src）对齐，两个证人必须一致。
			fmt.Printf("txid=%s local=%s reflector=%s observed=%s:%d\n",
				hex.EncodeToString(txid), conn.LocalAddr(), *reflector, ip, port)
			os.Exit(0)
		}
		time.Sleep(*gap)
	}
	fmt.Printf("UNAVAILABLE: %d 次探测都没在 %v 内收到匹配回包（reflector=%s）\n",
		*tries, *gap, *reflector)
	os.Exit(1)
}
