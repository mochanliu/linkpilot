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
	self := "1.1.1.1"
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
	putResp, err := cli.KV.Put(context.TODO(), "testtest", "bingo!!!")
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(putResp.Header.Revision)
	if putResp.PrevKv != nil {
		fmt.Printf("prev Value: %s \n CreateRevision : %d \n ModRevision: %d \n Version: %d \n",
			string(putResp.PrevKv.Value), putResp.PrevKv.CreateRevision, putResp.PrevKv.ModRevision, putResp.PrevKv.Version)
	}
	putResp.OpResponse().Del()

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
		putResp, err = cli.KV.Put(context.TODO(), self+":"+re.Addr, fmt.Sprint(re))
		if err != nil {
			fmt.Println(err)
			return
		}
		putResp.OpResponse().Del()
		ch <- re
	}
}
