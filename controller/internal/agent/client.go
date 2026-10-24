package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"sdwan/controller/internal/wire"
)

// ErrStreamClosed 表示 watch 流被服务端（或中途的网络）关掉了。
// 它和"解码出错"必须分开：前者是要重连的，后者是要报出来的。
var ErrStreamClosed = errors.New("agent: watch 流已关闭")

// Error 是控制器回的非 200。Detail 是服务端 http.Error 写下的那行人话。
//
// 状态码在这里不是日志装饰，是要被分支处理的：401=证书不对（要重 enroll），
// 403=node_id 与证书不一致（本地状态脏了），404=这台设备还没注册过。
// 把它们都糊成"请求失败"，agent 的自愈就只能靠重启。
type Error struct {
	Status int
	Path   string
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("agent: %s 返回 %d: %s", e.Path, e.Status, e.Detail)
}

// StatusOf 取出任意错误携带的 HTTP 状态码（没有则 0）。
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// Client 是与某个 vBOND 实例的会话。身份从证书来，node_id 只是对账字段。
//
// 这个客户端**故意不设 http.Client.Timeout**：watch 是长连接，一个全局超时
// 会把正常运行的流在固定秒数后掐断。所有请求的期限由调用方传入的 context 决定。
type Client struct {
	base   string
	nodeID uint16
	hc     *http.Client
}

// NewClient 用已 enroll 的身份建一个 mTLS 客户端。
func NewClient(base string, id *Identity) (*Client, error) {
	name, err := serverNameFromURL(base)
	if err != nil {
		return nil, err
	}
	tc, err := id.TLSConfig(name)
	if err != nil {
		return nil, err
	}
	return &Client{
		base:   base,
		nodeID: id.NodeID(),
		hc:     &http.Client{Transport: &http.Transport{TLSClientConfig: tc}},
	}, nil
}

// Enroll 是不带客户端证书的公开入口：设备此刻还没有证书，能靠的只有 claim code。
//
// roots 必须是操作者预先放进设备的控制器 CA。这里没有 trust-on-first-use：
// enroll 回来的东西是整套信任的根（节点证书 + 租户 CA + p-key 环），
// 第一次连接就无条件接受，等于把"中间人伪造整个租户"的成本降到一次抓包。
func Enroll(ctx context.Context, base string, roots *x509.CertPool, req wire.EnrollRequest) (wire.EnrollResponse, error) {
	if roots == nil {
		return wire.EnrollResponse{}, errors.New("agent: enroll 需要预先固定控制器 CA")
	}
	name, err := serverNameFromURL(base)
	if err != nil {
		return wire.EnrollResponse{}, err
	}
	c := &Client{
		base: base,
		hc:   &http.Client{Transport: &http.Transport{TLSClientConfig: enrollTLS(roots, name)}},
	}
	var resp wire.EnrollResponse
	if err := c.post(ctx, "/v1/enroll", req, &resp); err != nil {
		return wire.EnrollResponse{}, err
	}
	return resp, nil
}

// enrollTLS 是 enroll 这一步专用的传输配置：只带固定的根，不带客户端证书
// （设备此时还没有证书），也不跳过任何校验。
//
// InsecureSkipVerify 在这里不是"方便测试自签证书"的开关：控制器发的是 CA 签的
// 节点证书，而 enroll 应答里含整套信任根，跳过校验就是把第一步当成唯一一步。
// 测试要覆盖自签场景，就把测试 CA 作为 roots 传进来。
func enrollTLS(roots *x509.CertPool, serverName string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: serverName,
	}
}

// EnrollRequest 组装这台设备的注册请求。
//
// public_key 交的是**身份密钥**（Ed25519）而不是 WG 公钥：证书要绑的是"这台设备是谁"。
// WG 公钥走 key binding，等知道 (tenant, node_id) 之后才有意义 —— 见 wire 包注释。
func (id *Identity) EnrollRequest(claimCode, agentVersion string, reflectors int) wire.EnrollRequest {
	pub := id.IdentityPublicKey()
	return wire.EnrollRequest{
		Serial:        id.Serial(),
		ClaimCode:     claimCode,
		PublicKey:     base64.StdEncoding.EncodeToString(pub),
		ProtocolVer:   wire.ProtocolVersion,
		AgentVersion:  agentVersion,
		UDPRelfectorN: reflectors,
	}
}

// Heartbeat 上报存活与当前映射。映射搭心跳走是刻意的：NAT 重新绑定不该
// 还要多一次专门的注册往返。
func (c *Client) Heartbeat(ctx context.Context, req wire.HeartbeatRequest) (wire.HeartbeatResponse, error) {
	req.NodeID = c.nodeID
	var resp wire.HeartbeatResponse
	if err := c.post(ctx, "/v1/heartbeat", req, &resp); err != nil {
		return resp, err
	}
	return resp, nil
}

// MappingAck 是 /v1/mapping 的应答。服务端目前回的是内联 map，
// 字段名以 cmd/sr-ctrl 的实现为准。
type MappingAck struct {
	OK bool `json:"ok"`
	// BindingErr 非空 = 节点照常注册成功，但它自报的 WG 公钥没能通过身份验证。
	// 这时候对端配不出它的 peer，症状是"隧道在但没有流量"，必须往上报。
	BindingErr string `json:"binding_err,omitempty"`
}

// ReportMapping 描述"我是谁、我从哪儿能被看到、我的数据面在听哪些端口"。
func (c *Client) ReportMapping(ctx context.Context, req wire.MappingReport) (MappingAck, error) {
	var ack MappingAck
	if err := c.post(ctx, "/v1/mapping", req, &ack); err != nil {
		return ack, err
	}
	return ack, nil
}

// ReportObservations 把数据面的外层源观测表交给控制器做归因。
// req.Reset=true 表示"交表前我清过"，只有那样缺席的条目才等于"真的没了"。
func (c *Client) ReportObservations(ctx context.Context, req wire.ObserveReport) (wire.ObserveResponse, error) {
	req.NodeID = c.nodeID
	var resp wire.ObserveResponse
	if err := c.post(ctx, "/v1/observe", req, &resp); err != nil {
		return resp, err
	}
	return resp, nil
}

// ReportReach 上报一次打洞结果，控制器据此决定是否改用中继。
func (c *Client) ReportReach(ctx context.Context, req wire.ReachReport) error {
	return c.post(ctx, "/v1/reach", req, nil)
}

// ReportMetrics 上报一个采样窗口的隧道内遥测（BFD 状态 + 探针质量）。
// 每个窗口自带增量，所以丢一次上报不会污染下一次。
func (c *Client) ReportMetrics(ctx context.Context, req wire.MetricsReport) error {
	req.NodeID = c.nodeID
	return c.post(ctx, "/v1/metrics", req, nil)
}

// RegistryView 是 /v1/registry 的一次快照（也是仪表盘拓扑页读的同一份）。
type RegistryView struct {
	TenantID string          `json:"tenant_id"`
	Self     uint16          `json:"self"`
	Epoch    uint32          `json:"epoch"`
	Peers    []wire.PeerInfo `json:"peers"`
}

// Registry 拉全量视图。日常靠 Watch，这个只用于对账与启动时自检。
func (c *Client) Registry(ctx context.Context) (RegistryView, error) {
	var v RegistryView
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/registry", nil)
	if err != nil {
		return v, err
	}
	if err := c.do(req, &v); err != nil {
		return v, err
	}
	return v, nil
}

// WatchStream 是一条 application/jsonl 的推送流。
//
// 用 json.Decoder 直接啃响应体，而不是 bufio.Scanner 按行切：一行就是一个事件，
// 事件里塞的是整份 peer 视图，长度没有上界。Scanner 的 MAX_SCAN_TOKEN_SIZE 会把
// 一个合法的大 snapshot 当成错误，那种错误看起来像"控制器坏了"。
type WatchStream struct {
	resp *http.Response
	dec  *json.Decoder
}

// Watch 打开推送流。调用方负责 Close；ctx 取消会让正在阻塞的 Next 返回错误。
func (c *Client) Watch(ctx context.Context) (*WatchStream, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/watch", nil)
	if err != nil {
		return nil, err
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent: 打开 watch: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		detail := readDetail(res.Body)
		res.Body.Close()
		return nil, &Error{Status: res.StatusCode, Path: "/v1/watch", Detail: detail}
	}
	return &WatchStream{resp: res, dec: json.NewDecoder(res.Body)}, nil
}

// Next 取下一个事件。服务端每 15 秒发一个 ping，调用方可以把它当活性证明。
func (s *WatchStream) Next() (wire.WatchEvent, error) {
	var ev wire.WatchEvent
	err := s.dec.Decode(&ev)
	if errors.Is(err, io.EOF) {
		return ev, ErrStreamClosed
	}
	if err != nil {
		return ev, fmt.Errorf("agent: watch 解码: %w", err)
	}
	return ev, nil
}

func (s *WatchStream) Close() error {
	if s == nil || s.resp == nil {
		return nil
	}
	return s.resp.Body.Close()
}

// post 发一个 JSON 请求；out 为 nil 表示不关心应答体。
func (c *Client) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("agent: 编 %s 请求体: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	res, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("agent: %s: %w", req.URL.Path, err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return &Error{Status: res.StatusCode, Path: req.URL.Path, Detail: readDetail(res.Body)}
	}
	if out == nil {
		io.Copy(io.Discard, res.Body) // 读空再关，连接才能复用
		return nil
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return fmt.Errorf("agent: 解 %s 应答: %w", req.URL.Path, err)
	}
	return nil
}

// readDetail 取错误响应的前 1KB 文本。限制长度是因为 Detail 会进错误信息、
// 进而进日志和上报，一个发疯的服务端不该把 agent 的磁盘写满。
func readDetail(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 1024))
	return string(bytes.TrimSpace(b))
}
