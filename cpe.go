package main

import (
	"context"
	"fmt"
	"time"

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
	//self := "1.1.1.1"
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
	getResp, err := cli.KV.Get(context.TODO(), "1.1.1.1")
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, temp := range getResp.Kvs {
		fmt.Println(temp.Value)
	}
	/* 	for {
		e := <-getResp
		for _, a := range e.Events {
			fmt.Println(*a)
		}
	} */
	getResp.OpResponse().Del()
	/*
		 	pinglist := []string{"8.210.246.191", "39.107.73.224"}
			ch := make(chan sla)
			var re sla
			for _, p := range pinglist {
				go telemetry(ch)
				ch <- sla{p, 0, 0, 0}
			}
			defer close(ch)
			for {
				re = <-ch
				if re.lossrate < 0.1 {
					continue
				}
				getResp, err = cli.KV.Get(context.TODO(), self+":"+re.Addr)
				if err != nil {
					fmt.Println(err)
					return
				}
				getResp.OpResponse().Del()

				send_map, _ := ebpf.LoadPinnedMap("/sys/fs/bpf/tc/globals/send_map", nil)
				defer send_map.Close()
				key := 12345
				var srconfig sr
				srconfig.srou.MagicNumber = 0
				srconfig.srou.FlowIDLen = 0
				srconfig.srou.ProtoID = 1
				srconfig.srou.SRLen = uint8(t)
				srconfig.srh.SegType = 1
				srconfig.srh.LastEntry = uint8(t)
				srconfig.srh.SegLeft = uint8(t)
				srconfig.srh.SRHLen = uint8(t)
				srconfig.OriSrc.ip = 0
				srconfig.OriSrc.port = 0
				srconfig.OriSrc.arg = 0
				srconfig.seg0.ip = convertIP(v.leaves["seg0-ip"])
				srconfig.seg0.port = convert_port(v.leaves["seg0-port"])
				srconfig.seg0.arg = convert_port(v.leaves["seg0-arg"])
				srconfig.seg1.ip = convertIP(v.leaves["seg1-ip"])
				srconfig.seg1.port = convert_port(v.leaves["seg1-port"])
				srconfig.seg1.arg = convert_port(v.leaves["seg1-arg"])
				srconfig.seg2.ip = convertIP(v.leaves["seg2-ip"])
				srconfig.seg2.port = convert_port(v.leaves["seg2-port"])
				srconfig.seg2.arg = convert_port(v.leaves["seg2-arg"])
				srconfig.seg3.ip = convertIP(v.leaves["seg3-ip"])
				srconfig.seg3.port = convert_port(v.leaves["seg3-port"])
				srconfig.seg3.arg = convert_port(v.leaves["seg3-arg"])
				srconfig.seg4.ip = convertIP(v.leaves["seg4-ip"])
				srconfig.seg4.port = convert_port(v.leaves["seg4-port"])
				srconfig.seg4.arg = convert_port(v.leaves["seg4-arg"])
				send_map.Put(key, &srconfig)

				ch <- re
			}
	*/
}
