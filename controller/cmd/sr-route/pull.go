package main

// 这一条腿补的是 B4：sr-route 重启之后，本盒客户侧前缀集没有人重建。
//
// 走一遍场景就能看见缺口。CPE 上 sr-frr 在跑，从盒内 FRR 学到客户侧的
// 10.20.0.0/16，把它 PUT 进 sr-route 的 /local-prefixes，sr-route 回 200 ——
// 这一刻两边是一致的：sr-frr 那边 localReporter 记着 lastSeen={10.20/16}、
// synced=true，sr-route 这边 c.dynamic 里也是这一条。现在 sr-route 换件重启
// （抬版本、开机清单、watch 拉活，任何一种）。新进程起来时 c.dynamic 是空的
// —— 本地前缀集只活在那一次 PUT 里，盘上没有它。而 sr-frr 那边：diffLocalPrefixes
// 算的是 lastSeen 与现值的差，集合没变，差量就是零，于是它一个字都不发。
// 谁都没出错，谁都没动，这台盒子对 RR 的本地前缀宣告从此永久少一条 10.20/16。
//
// 少的那一条不是"少 announce 一条路由"这么轻：远端不知道这条前缀属于这台盒子，
// 会继续把它当别人的路由往隧道里送；本端却知道是自己的。#271/#272 那两类回灌
// 与黑洞就是这个形状，而且它不随时间自愈 —— 只有下一次集合真的变化才碰巧补上。
//
// 所以照抄 zebra：新客户端一连上 daemon，daemon 立刻把全表 announce 一遍
// （bgpd 的 bgp_zebra_connected → bgp_zebra_announce_table）。复刻它用最轻的
// 办法 —— 开机拉一次，拿到就退出，不引入周期重发：拉到之后每一次变化本来就
// 有人推（10ms 合并的增量 + poll_sec 的兜底全量），没有缺口需要靠定时对账去盖
// （AGENTS.md §1 第 3 条）。
//
// 拉取与并发推送的先后不用额外协商：sr-frr 应答这一次 GET 的同一刻就把自己的
// reporter 降级（synced=false），下一轮一定重发全量。所以拉到的即使是一份回放
// 中途的集合，全量重发也在后面兜着 —— 不需要版本号，也不需要两边对齐时钟。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"sdwan/controller/internal/route"
)

const (
	// 缺省路径与 sr-frr 的 defaultControlSocket 是同一条。找它的阶梯照 sr-apply
	// 的 -srfrr-sock 抄：显式指定 > /etc/sr-frr.json 里的 control_socket > 缺省。
	// 中间那一跳不是多此一举 —— control_socket 是盒子自己的键，sr-apply 只叠加
	// 不覆盖（test/lab/README.md），运维改了它，读的一方得按同一条阶梯找回。
	defaultSRFrrSocket = "/run/sr-frr/control.sock"
	defaultSRFrrConf   = "/etc/sr-frr.json"

	// 开机清单里 do_route 在 do_bridge 之前（deploy/sr-box/start.sh），sr-frr 的
	// 套子往往要晚几秒才出现，所以拉不到要等；等的节拍取 sr-frr poll_sec 的量级。
	pullRetryEvery = 2 * time.Second

	// 连续失败多少条打一行日志。第一次必打（那一台盒子上没有 sr-frr 在跑的时候，
	// 这条日志是唯一说得清"本地前缀为什么是空的"的东西），之后一分钟一条。
	pullLogEvery = 30
)

// pullLocalPrefixes 向本机 sr-frr 的控制口要一份客户侧前缀现值并装上，
// 第一次成功（或 ctx 结束）为止。
func pullLocalPrefixes(ctx context.Context, c *route.Client, socket string) {
	hc := unixClient(socket)
	for attempt := 0; ; attempt++ {
		ps, err := fetchSRFrrPrefixes(ctx, hc, socket)
		switch {
		case err == nil:
			lps := make([]route.LocalPrefix, 0, len(ps))
			for _, p := range ps {
				lps = append(lps, route.LocalPrefix{VRFID: p.VRFID, Prefix: p.Prefix})
			}
			// 整套没装上的原因和 sr-frr 那边 PUT 被拒是同一个：某个 VPN 还没进
			// 本机的 VRF 表（export RT 取自 VRF 配置，宁可不装也绝不没有策略就把
			// 它播出去）。第 4 段落地以后就能装，所以这是重试，不是放弃。
			if applyErr := c.SetLocalPrefixes(lps); applyErr != nil {
				log.Printf("sr-route: sr-frr 给的客户侧前缀装不上（第 %d 次，继续重试）: %v", attempt+1, applyErr)
			} else {
				log.Printf("sr-route: 开机从 sr-frr 拉到客户侧前缀 %d 条（%s），之后的变化由它推", len(lps), socket)
				return
			}
		case ctx.Err() != nil:
			return
		case attempt == 0 || attempt%pullLogEvery == 0:
			log.Printf("sr-route: 向 sr-frr 拉客户侧前缀未成功（第 %d 次，%s，继续重试）: %v",
				attempt+1, socket, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pullRetryEvery):
		}
	}
}

// unixClient 只认一个地址：本机 sr-frr 的控制口。URL 里的 host 不起作用 ——
// 套子路径是唯一的真值来源，DialContext 根本不看它。
func unixClient(socket string) *http.Client {
	return &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				d.Timeout = time.Second
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
}

// fetchSRFrrPrefixes 读一次现值。非 200 一律当错误交回（连 sr-frr 那句"还没就绪"
// 的原文一起），调用方只会重试，绝不会拿一个没答上来的集合去装表。
func fetchSRFrrPrefixes(ctx context.Context, hc *http.Client, socket string) ([]localPrefixJSON, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://sr-frr/local-prefixes", nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", socket, err)
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, 1<<20)
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(body, 512))
		return nil, fmt.Errorf("GET %s: %s: %s", socket, resp.Status, string(msg))
	}
	// 与本机 PUT 进来的请求体同一套严格度：字段拼错了解出来是"这一份是空的"，
	// 而这是一次整表替换，空的会真的把已宣告的前缀全撤掉。
	var doc localPrefixesDoc
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("解析 sr-frr 的应答: %w", err)
	}
	return doc.Prefixes, nil
}

// envDefault 读开机清单那套键：运维在 unit 里改了 BRIDGE_CONF 的路径，这边的
// 缺省跟着同一个环境变量，不各说各话。
func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// srfrrControlSocket 按上面那条阶梯定出控制口。ok=false 表示这台盒子没配
// sr-frr：开机清单的 do_bridge 用的是同一道门（没有 $BRIDGE_CONF 就 SKIP），
// 那时不该留一个循环一直去敲没人听的套子。
func srfrrControlSocket(confPath, override string) (string, bool) {
	if override != "" {
		return override, true
	}
	raw, err := os.ReadFile(confPath)
	if err != nil {
		return "", false
	}
	var cfg struct {
		ControlSocket string `json:"control_socket"`
	}
	// 配置文件读不成也没关系：那份是 sr-frr 自己的键，它起不起来、用什么套子，
	// 不该由这边猜出第二个真值。解析不了就退回缺省路径去问，问不通自然有日志。
	if json.Unmarshal(raw, &cfg) == nil && cfg.ControlSocket != "" {
		return cfg.ControlSocket, true
	}
	return defaultSRFrrSocket, true
}
