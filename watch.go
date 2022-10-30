package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
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

func main() {
	f, _ := os.Open("watchpath.conf")
	c, err := ioutil.ReadAll(f)
	if err != nil {
		log.Fatal(err)
	}
	config := strings.Split(string(c), string(10))
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{"39.107.73.224:2379"},
		DialTimeout: 5 * time.Second,
		Username:    "user0",
		Password:    "testtest",
	})
	if err != nil {
		fmt.Println(err)
	}
	var paths []clientv3.WatchChan
	for _, key := range config {
		getResp := cli.Watcher.Watch(context.TODO(), key)
		defer cli.Watcher.Close()
		paths = append(paths, getResp)
	}
	defer cli.Close()
	var i uint64 = 0
	for {
		select {
		case e, ok := <-paths[i]:
			if !ok {
				i += 1
				i %= 3
				time.Sleep(time.Second)
				continue
			}
			var result []byte
			key := convert_port(strings.Split(config[i], ":")[1])
			send_map, _ := ebpf.LoadPinnedMap("/sys/fs/bpf/tc/globals/send_map", nil)
			defer send_map.Close()
			err = send_map.Lookup(key, &result)
			if err != nil {
				fmt.Println(err)
				continue
			}
			var insert *sr
			insert = (*sr)(unsafe.Pointer(&result))
			var tmp [4]*srlistv4
			tmp[0] = &insert.seg1
			tmp[1] = &insert.seg2
			tmp[2] = &insert.seg3
			tmp[3] = &insert.seg4
			pa := strings.Split(string(e.Events[0].Kv.Value), ",")
			for seq, p := range pa {
				tmp[seq].ip = convertIP(strings.Split(p, ":")[0])
				tmp[seq].port = convert_port(strings.Split(p, ":")[1])
			}
			time.Sleep(time.Second)
			send_map.Put(key, insert)
			time.Sleep(time.Second)
			send_map.Close()
		default:
			i += 1
			i %= 3
			time.Sleep(time.Second)
			continue
		}
	}

}
