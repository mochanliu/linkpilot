package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/go-ping/ping"
	"go.etcd.io/etcd/clientv3"
)

type sla struct {
	Addr      string
	lossrate  float32
	AvgRtt    uint16
	StdDevRtt uint16
}

type srouhdr struct {
	MagicNumber uint8
	FlowIDLen   uint8
	ProtoID     uint8
	SRLen       uint8
	FlowID      uint32
}

type srousrh struct {
	SegType   uint8
	LastEntry uint8
	SegLeft   uint8
	SRHLen    uint8
}

type srlistv4 struct {
	ip   uint32
	port uint16
	arg  uint16
}

type sr struct {
	srou   srouhdr
	OriSrc srlistv4
	srh    srousrh
	seg0   srlistv4
	seg1   srlistv4
	seg2   srlistv4
	seg3   srlistv4
	seg4   srlistv4
}

func convert_port(s string) uint16 {
	t, _ := strconv.ParseInt(s, 0, 16)
	return ntohs(uint16(t))
}

func ntohs(n uint16) uint16 {
	return (n<<8)&0xff00 | n>>8
}

func ntohl(n uint32) uint32 {
	return (n<<24)&0xff000000 | (n >> 24) | (n<<8)&0x00ff0000 | (n>>8)&0x0000ff00
}

func convertIP(s string) uint32 {
	var ipadd uint32 = 0
	t := strings.Split(s, ".")
	var a, b, c, d int
	a, _ = strconv.Atoi(t[0])
	b, _ = strconv.Atoi(t[1])
	c, _ = strconv.Atoi(t[2])
	d, _ = strconv.Atoi(t[3])
	ipadd = ipadd + uint32(a) + uint32(b)*256 + uint32(c)*256*256 + uint32(d)*256*256*256
	return ipadd
}

func telemetry(ch chan sla) {
	var err error
	var pinger *ping.Pinger
	var dst sla
	var ok bool
	for {
		dst, ok = <-ch
		if !ok {
			break
		}
		pinger, err = ping.NewPinger(dst.Addr)
		if err != nil {
			panic(err)
		}
		pinger.Count = 20
		pinger.TTL = 64
		pinger.Interval = time.Second
		pinger.Timeout = 20 * time.Second
		pinger.OnFinish = func(stats *ping.Statistics) {
			dst.lossrate = float32(stats.PacketLoss / 20)
			dst.AvgRtt = uint16(stats.AvgRtt.Milliseconds())
			dst.StdDevRtt = uint16(stats.StdDevRtt.Milliseconds())
			ch <- dst
		}
		err = pinger.Run()
		if err != nil {
			panic(err)
		}
		pinger.Stop()
	}
}

func main() {
	var temp string
	tunnel := map[string]string{"10.0.0.2": "39.107.73.224:4097"}
	active := "120.78.65.189:4096"
	backup := "8.210.246.191:4096"
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{"39.107.73.224:2379"},
		DialTimeout: 5 * time.Second,
		Username:    "user0",
		Password:    "testtest",
	})
	if err != nil {
		fmt.Println(err)
	}
	defer cli.Close()

	pinglist := []string{"10.0.0.2"}
	ch := make(chan sla)
	var re sla
	for _, p := range pinglist {
		go telemetry(ch)
		ch <- sla{p, 0, 0, 0}
	}
	defer close(ch)
	send_map, _ := ebpf.LoadPinnedMap("/sys/fs/bpf/tc/globals/send_map", nil)
	defer send_map.Close()
	for {
		re = <-ch
		if (re.lossrate > 0.1) || (re.AvgRtt > 100) {
			var result []byte
			key := convert_port(strings.Split(tunnel[re.Addr], ":")[1])
			err = send_map.Lookup(key, &result)
			if err != nil {
				fmt.Println(err)
				continue
			}
			var insert *sr
			insert = *(**sr)(unsafe.Pointer(&result))
			var tmp [4]*srlistv4
			tmp[0] = &insert.seg1
			tmp[1] = &insert.seg2
			tmp[2] = &insert.seg3
			tmp[3] = &insert.seg4
			paths := strings.Split(backup, ",")
			for seq, path := range paths {
				tmp[seq].ip = convertIP(strings.Split(path, ":")[0])
				tmp[seq].port = convert_port(strings.Split(path, ":")[1])
			}
			insert.srh.SegLeft = uint8(len(paths))
			insert.srh.LastEntry = uint8(len(paths))
			time.Sleep(time.Second)
			send_map.Put(key, insert)
			putResp, err := cli.KV.Put(context.TODO(), tunnel[re.Addr], backup)
			if err != nil {
				fmt.Println(err)
			}
			defer putResp.OpResponse().Del()
			temp = active
			active = backup
			backup = temp
		}
		ch <- re
	}
}
