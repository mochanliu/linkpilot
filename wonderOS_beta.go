package main

import (
	"crypto"
	"crypto/rsa"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"math/big"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// SR data structure
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

func convert_port(s string) uint16 {
	t, _ := strconv.ParseInt(s, 0, 16)
	return ntohs(uint16(t))
}

func convert_flowid(s string) uint32 {
	t, _ := strconv.ParseInt(s, 0, 32)
	return ntohl(uint32(t))
}

// configuraion module
type leaf map[string]string

type list struct {
	leaves leaf
}

type container struct {
	containers map[string]container
	lists      map[string][]list
}

type module struct {
	name string
	c    map[string]container
}

func parse_module(c []byte) (mod module) {
	j := 0
	i := 0
	var k int
	start := false
	for ; i < len(c); i++ {
		switch string(c[i]) {
		case string(10):
			if !start {
				j += 1
			}
		case string(32):
			if !start {
				j += 1
			}
		case "{":
			start = true
			temp := strings.Split(string(c[j:i]), " ")
			if temp[0] == "module" {
				fmt.Println(temp)
				mod.name = temp[1]
				start = false
				j = i + 1
			} else if temp[0] == "container" {
				fmt.Println(temp)
				if mod.c == nil {
					mod.c = make(map[string]container)
				}
				mod.c[temp[1]], k = parse_container(c[i+1:])
				i += k
				j = i + 1
				start = false
			}
		case "}":
			start = true
			return mod
		default:
			start = true
		}
	}
	return mod
}

func parse_container(c []byte) (con container, i int) {
	j := 0
	i = 0
	var k int
	start := false
	for ; i < len(c); i++ {
		switch string(c[i]) {
		case string(10):
			if !start {
				j += 1
			}
		case string(32):
			if !start {
				j += 1
			}
		case "{":
			start = true
			temp := strings.Split(string(c[j:i]), " ")
			switch temp[0] {
			case "container":
				fmt.Println(temp)
				if con.containers == nil {
					con.containers = make(map[string]container)
				}
				con.containers[temp[1]], k = parse_container(c[i+1:])
				i += k
				j = i + 1
				start = false
			case "list":
				fmt.Println(temp)
				var l list
				l, k = parse_list(c[i+1:])
				if con.lists == nil {
					con.lists = make(map[string][]list)
				}
				con.lists[temp[1]] = append(con.lists[temp[1]], l)
				i += k
				j = i + 1
				start = false
			}
		case "}":
			start = true
			i++
			return con, i
		default:
			start = true
		}
	}
	return con, i
}

func parse_list(c []byte) (l list, i int) {
	j := 0
	i = 0
	//var k int
	start := false
	for ; i < len(c); i++ {
		switch string(c[i]) {
		case string(10):
			if !start {
				j += 1
			}
		case string(32):
			if !start {
				j += 1
			}
		case ";":
			start = true
			temp := strings.Split(string(c[j:i]), " ")
			if temp[0] == "leaf" {
				if l.leaves == nil {
					l.leaves = make(leaf)
				}
				l.leaves[temp[1]] = temp[2]
			}
			j = i + 1
			start = false
		case "}":
			start = true
			i++
			return l, i
		default:
			start = true
		}
	}
	return l, i
}

func config_tunnel_list(lists []list) {
	var srconfig sr
	var key uint16
	var t int
	for _, v := range lists {
		switch v.leaves["action"] {
		case "encapsulation":
			fmt.Println("Found a SR tunnel!Configuring...")
			t, _ = strconv.Atoi(v.leaves["MagicNumber"])
			srconfig.srou.MagicNumber = uint8(t)
			t, _ = strconv.Atoi(v.leaves["FlowIDLen"])
			srconfig.srou.FlowIDLen = uint8(t)
			t, _ = strconv.Atoi(v.leaves["ProtoID"])
			srconfig.srou.ProtoID = uint8(t)
			t, _ = strconv.Atoi(v.leaves["SRLen"])
			srconfig.srou.SRLen = uint8(t)
			srconfig.srou.FlowID = convert_flowid(v.leaves["FlowID"])
			t, _ = strconv.Atoi(v.leaves["SegType"])
			srconfig.srh.SegType = uint8(t)
			t, _ = strconv.Atoi(v.leaves["LastEntry"])
			srconfig.srh.LastEntry = uint8(t)
			t, _ = strconv.Atoi(v.leaves["SegLeft"])
			srconfig.srh.SegLeft = uint8(t)
			t, _ = strconv.Atoi(v.leaves["SRHLen"])
			srconfig.srh.SRHLen = uint8(t)
			srconfig.OriSrc.ip = convertIP(v.leaves["OriSrc-ip"])
			srconfig.OriSrc.port = convert_port(v.leaves["OriSrc-port"])
			srconfig.OriSrc.arg = convert_port(v.leaves["OriSrc-arg"])
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
			key = convert_port(v.leaves["source-port"])
			send_map, _ := ebpf.LoadPinnedMap("/sys/fs/bpf/tc/globals/send_map", nil)
			defer send_map.Close()
			send_map.Put(key, &srconfig)
			var value uint32 = convert_flowid(v.leaves["FlowID"])
			receive_map, _ := ebpf.LoadPinnedMap("/sys/fs/bpf/xdp/globals/receive_map", nil)
			defer receive_map.Close()
			receive_map.Put(key, &value)
			cmd := "ip link add tunnel" + v.leaves["source-port"] + " type wireguard"
			_ = exec.Command("bash", "-c", cmd).Run()
			cmd = "ip link set tunnel" + v.leaves["source-port"] + " up"
			_ = exec.Command("bash", "-c", cmd).Run()
			cmd = "ip link set tunnel" + v.leaves["source-port"] + " mtu 1360"
			_ = exec.Command("bash", "-c", cmd).Run()
			cmd = "wg set tunnel" + v.leaves["source-port"] + " private-key " + v.leaves["private"] + " listen-port " + v.leaves["source-port"]
			_ = exec.Command("bash", "-c", cmd).Run()
			cmd = "wg set tunnel" + v.leaves["source-port"] + " peer " + v.leaves["public"] + " allowed-ips 0.0.0.0/0"
			if v.leaves["destination-ip"] != "0.0.0.0" {
				cmd = cmd + " endpoint " + v.leaves["destination-ip"] + ":" + v.leaves["destination-port"]
			}
			_ = exec.Command("bash", "-c", cmd).Run()
		case "xmit":
			fmt.Println("Found transmit tunnel!Configuring...")
			key = convert_port(v.leaves["source-port"])
			var value uint32 = convert_flowid(v.leaves["FlowID"])
			receive_map, _ := ebpf.LoadPinnedMap("/sys/fs/bpf/xdp/globals/receive_map", nil)
			defer receive_map.Close()
			receive_map.Put(key, &value)
		case "none":
			fmt.Println("Found general VPN tunnel!Configuring...")
			cmd := "ip link add tunnel" + v.leaves["source-port"] + " type wireguard"
			_ = exec.Command("bash", "-c", cmd).Run()
			cmd = "ip link set tunnel" + v.leaves["source-port"] + " up"
			_ = exec.Command("bash", "-c", cmd).Run()
			cmd = "ip link set tunnel" + v.leaves["source-port"] + " mtu 1360"
			_ = exec.Command("bash", "-c", cmd).Run()
			cmd = "wg set tunnel" + v.leaves["source-port"] + " private-key " + v.leaves["private"] + " listen-port " + v.leaves["source-port"]
			_ = exec.Command("bash", "-c", cmd).Run()
			cmd = "wg set tunnel" + v.leaves["source-port"] + " peer " + v.leaves["public"] + " allowed-ips 0.0.0.0/0"
			if v.leaves["destination-ip"] != "0.0.0.0" {
				cmd = cmd + " endpoint " + v.leaves["destination-ip"] + ":" + v.leaves["destination-port"]
			}
			_ = exec.Command("bash", "-c", cmd).Run()
		}
	}
}

func config_container(c container) {
	for k, v := range c.containers {
		switch k {
		case "underlay":
			fmt.Println("Found underlay configuration.Configuring...")
			config_container(v)
		}
	}
	for k, v := range c.lists {
		switch k {
		case "tunnel":
			fmt.Println("Found tunnel configuration.Configuring...")
			config_tunnel_list(v)
		}
	}
}

func load_config(config_module module) {
	fmt.Println("Finding modules...")
	for k, v := range config_module.c {
		switch k {
		case "SDWAN":
			fmt.Println("SDWAN module found!Configuring...")
			config_container(v)
		}
	}
	fmt.Println("Configuraion loaded!!!")
	fmt.Println("-----------------------------------------------")
}

func modifyFlowid(flowid uint32, config_module module) {
	var srconfig *sr
	var key uint16
	var value uint32
	var path []byte
	send_map, _ := ebpf.LoadPinnedMap("/sys/fs/bpf/tc/globals/send_map", nil)
	receive_map, _ := ebpf.LoadPinnedMap("/sys/fs/bpf/xdp/globals/receive_map", nil)
	defer send_map.Close()
	defer receive_map.Close()
	for _, v := range config_module.c["SDWAN"].containers["underlay"].lists["tunnel"] {
		switch v.leaves["action"] {
		case "encapsulation":
			key = convert_port(v.leaves["source-port"])
			send_map.Lookup(key, &path)
			srconfig = *(**sr)(unsafe.Pointer(&path))
			fmt.Println("send before:", srconfig.srou.FlowID)
			srconfig.srou.FlowID = (srconfig.srou.FlowID&0x0000ffff)<<16 + flowid
			send_map.Put(key, srconfig)
			fmt.Println("send after:", srconfig.srou.FlowID)
			receive_map.Lookup(key, &value)
			fmt.Println("receive before", value)
			value = (value&0x0000ffff)<<16 + flowid
			receive_map.Put(key, &value)
			fmt.Println("receive after", value)
		case "xmit":
			key = convert_port(v.leaves["source-port"])
			receive_map.Lookup(key, &value)
			value = (value&0x0000ffff)<<16 + flowid
			receive_map.Put(key, &value)
			fmt.Println(value)
		}
	}
}

func shutdownService(config_module module) {
	var key uint16
	var value []byte
	send_map, _ := ebpf.LoadPinnedMap("/sys/fs/bpf/tc/globals/send_map", nil)
	receive_map, _ := ebpf.LoadPinnedMap("/sys/fs/bpf/xdp/globals/receive_map", nil)
	defer send_map.Close()
	defer receive_map.Close()
	for _, v := range config_module.c["SDWAN"].containers["underlay"].lists["tunnel"] {
		switch v.leaves["action"] {
		case "encapsulation":
			key = convert_port(v.leaves["source-port"])
			receive_map.LookupAndDelete(key, &value)
			send_map.LookupAndDelete(key, &value)
		case "xmit":
			key = convert_port(v.leaves["source-port"])
			receive_map.LookupAndDelete(key, &value)
		}
	}
}

func checklicense() bool {
	n := new(big.Int)
	n.SetString("21801701870319918667061053920094708811892896382471984251374101802864987277138970044474932739464632613520378170201966101001971303553736280226891088693480768240592078024260400206229725045244276636355240541863010786343828885739255853635863932806858292489942589691357311685309693954935285255646829473799995668040300846332368569844682192704583528013119330265006538960652263564209109909521122145027476096816528753886991448698742042548021306658206484287185679078125666069624709498331699039486399918917306843393144433169370392336736670847335151921501546223723917836254518614731819820483447746603193957451480517484382686605843", 10)
	var e int = 65537
	d := new(big.Int)
	d.SetString("12756941320521663198566569963956114767512531706898937273169264017826060578050951725979595048491535033084814106091291881551239689773390281430959907673796489013690358241517582847986595297847437637532245362152108543795308758568884499980592796363541233202684261249129044486446681169169451134222570710610528920731771344097024979000903112826198468381710368626733813696183004832913003462058454237262542769036118212548559413267084207095281439331139105674388414569862860505190944755154081003109344776860326597151153170968146985560478322301046316695745984106686511721189106707576539407233426524699626051846531214241282852511393", 10)
	var pub rsa.PublicKey = rsa.PublicKey{N: n, E: e}
	a := new(big.Int)
	b := new(big.Int)
	a.SetString("136677190428946601444316171726465999959902450258637573374511802472629687541602365968849367698452626850569433502292301910714530503419885439876628355149675045920720896334842567465979662459305522517415433830502090392574631733117519979510338911767740081169625230264352522133376372102492213438740613481076101098023", 10)
	b.SetString("159512364878862609510242619991488258800597401421158187978033712956374143620155135596770427503387504532120923424130514591529690271693254511688065908511030949418525307469553700017261587398461730871162258811606150262794880236475051956356896255319223580901872463051913341641583991866942680701325588061641979660341", 10)
	dp := new(big.Int)
	dq := new(big.Int)
	qinv := new(big.Int)
	dp.SetString("41987302972458030225344728708499625817366007462306029643545571496733959431696300319679636234080698481506849637634479978766431826683439180326169319227740172719561069409789667070396395079011826675666034901040611957729298879760364828226523235891479790869097834214446943377195576537520419505960980380769720057471", 10)
	dq.SetString("32590301138715082187804578813281471311472896303299024017362274997113840778092943919324290466001780455087952830448564786007637712101144054679085135342824731261944456826179471798085549464659697214777341738062565451864190401855454868175516744109806731285778602625465304249215094543515304249366764181231763853273", 10)
	qinv.SetString("56238396727858012523135216227347833355974347809060722616290399205955638462692747638843566714260478154699956354814213482255370466739437021032558379978182653259659282684043241007269006267894762912435722259389862730447166041095996587269680061539406904156857372115675551105784231726752045674569375349626611445906", 10)
	var privateKey = rsa.PrivateKey{PublicKey: pub, D: d, Primes: []*big.Int{a, b}, Precomputed: rsa.PrecomputedValues{Dp: dp, Dq: dq, Qinv: qinv, CRTValues: []rsa.CRTValue{}}}
	f, _ := os.Open("license.txt")
	license, err := ioutil.ReadAll(f)
	if err != nil {
		log.Fatal(err)
	}
	var encryptedBytes []byte
	for _, i := range strings.Split(string(license), " ") {
		temp, _ := strconv.Atoi(i)
		encryptedBytes = append(encryptedBytes, byte(uint8(temp)))
	}
	decryptedBytes, err := privateKey.Decrypt(nil, encryptedBytes, &rsa.OAEPOptions{Hash: crypto.SHA256})
	if err != nil {
		return false
	}
	lic := strings.Split(string(decryptedBytes), " ")
	year, _ := strconv.ParseInt(lic[0], 0, 16)
	month, _ := strconv.Atoi(lic[1])
	day, _ := strconv.Atoi(lic[2])
	beijing := time.FixedZone("Beijing Time", 0)
	expiretime := time.Date(int(year), time.Month(month), day, 0, 0, 0, 0, beijing)
	fmt.Println(expiretime)
	return expiretime.After(time.Now())
}

func main() {
	if len(os.Args) > 2 {
		fmt.Println(os.Args[1])
		fmt.Println(os.Args[2])
	}
	if !checklicense() {
		return
	}
	f, _ := os.Open("SDWAN-router.yang")
	config, err := ioutil.ReadAll(f)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Reading SDWAN-router.yang...")
	var config_module module = parse_module(config)
	fmt.Println(config_module)
	fmt.Println("-------------------------------------------------")
	load_config(config_module)

	const salt string = "OAITS6AP7JGIOQMH23435TW7DKYSJBF2A5ZDE34VF7RS7F555S4J5PC5PTBZHD4DYNWB267OPYWYOLGTDVZCTPYZPKVOC63TD33VBO7CHNXNLHLGEV4ISEO4NJLRXIRNNRVLTZASYXZVVCBNXBE7ETPDBAICGQY6UOVLRU74E74VWOBCATDXFEHUW3A3FK5QEMQ6H6AKKG432TLOW4ZN62EMDPFR4H6ADWGKPAICAM7OGGLQ2RASK3NNRTJGSVH57WOVD6AGHYJBLFBGIN63ODJ2A57BW3S2JHIBPETX2WQA5SOAWQ7QC4JENJEFOLKI6JFM4N4NZO7PHTWNO2EIQRLLKMCWTJQT2SE5I3ZTKXDSUDGH5VAY5AQTXFF7H76V7XKCXTHWIXHJ3SGHZWE36GQUGU"
	var i uint64 = 0
	const interval uint64 = 1
	time.Sleep(time.Second * time.Duration(60-uint8(time.Now().Second())))
	for {
		time.Sleep(time.Second * 30)
		key, err := totp.GenerateCodeCustom(salt, time.Now(), totp.ValidateOpts{uint(60), uint(1), otp.DigitsEight, otp.AlgorithmSHA512})
		if err != nil {
			panic(err)
		}
		var flowid uint32
		for s := 0; s < 8; s++ {
			num, _ := strconv.Atoi(string(key[s]))
			flowid += uint32(num * int(math.Pow(10, float64(7-s))))
		}
		flowid = (flowid >> 16) + (flowid & 0x0000ffff)
		flowid = (flowid >> 16) + (flowid & 0x0000ffff)
		if i%interval == 0 {
			fmt.Println(flowid)
			modifyFlowid(flowid, config_module)
		}
		time.Sleep(time.Second * time.Duration(60-uint8(time.Now().Second())))
		i++
		if i%1500 == 0 {
			if !checklicense() {
				shutdownService(config_module)
				return
			}
		}
	}
}
