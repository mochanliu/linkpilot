package underlay

import "fmt"

// FanRequest is the shape an operator actually asks for by hand: one local
// device, one path, and a set of peers to reach through that same path ("从 CPE A
// 经同一条路径到 B C D E F"). Only the local end is specified in detail; the far
// end of each tunnel takes the defaults the devices already reported (its default
// egress port, its observed outer address, an allocated tunnel port), because
// asking per-peer for things a device reports is not a question a batch can ask.
type FanRequest struct {
	Local EndpointSpec
	Peers []uint16
	Path  *TunnelPath // nil = direct
	Roles Roles
	// Color applies to every tunnel this batch creates; a batch that recolors
	// one peer would otherwise have to be edited peer by peer.
	Color uint8
	// Weight / Dynamic apply to every tunnel this batch creates, same rule as
	// Color（weight 0 = 默认 1；Dynamic 开时 Weight 是动态档位的基数上限）。
	Weight  uint8
	Dynamic bool
	// BatchID labels the whole fan; empty means Fan makes one, so the page can
	// always say which batch a tunnel came from.
	BatchID string
	Now     int64
}

// FanSkip is one peer this fan could not reach, with the reason the tunnel would
// have been refused. It is a result, not an error: the tunnels that DID get built
// stay built.
type FanSkip struct {
	NodeID uint16 `json:"node_id"`
	Reason string `json:"reason"`
}

// FanResult is what a fan did. Created/Skipped together cover every peer asked
// for (after de-duplication), so the page can show a count that adds up.
type FanResult struct {
	BatchID string
	Created []Tunnel
	Skipped []FanSkip
}

// Fan builds one tunnel per peer, each exactly the way a manual single create
// would have built it -- same validation, same allocator, same errors.
//
// Each peer commits on its own: the batch is a list of independent pairs, and a
// peer that already has a tunnel must not roll back the peers that do not. That
// is why one peer's failure is a skip while a bad path or a bad local end is a
// hard error -- those would fail every peer, and reporting them N times would
// look like N problems.
func (s *State) Fan(req FanRequest, views map[uint16]NodeView) (FanResult, error) {
	if !s.Configured() {
		return FanResult{}, fmt.Errorf("还没有发布全局配置：先保存并发布“全局配置”，中转端口随它一起下发")
	}
	peers := dedupeNodes(req.Peers)
	if len(peers) == 0 {
		return FanResult{}, fmt.Errorf("对端至少要选一台设备")
	}
	if _, err := viewOf(views, "本端", req.Local.NodeID); err != nil {
		return FanResult{}, err
	}
	// A bad color would fail every peer the same way, so it is a hard error here
	// rather than N skip lines.
	if err := checkColor(req.Color); err != nil {
		return FanResult{}, err
	}
	if _, err := s.resolve(req.Path); err != nil {
		return FanResult{}, err
	}
	batchID := req.BatchID
	if batchID == "" {
		batchID = s.NewBatchID()
	}

	out := FanResult{BatchID: batchID, Created: []Tunnel{}, Skipped: []FanSkip{}}
	for _, peer := range peers {
		// Fan is an auto batch: one tunnel per pair, ever. A pair that already
		// has a tunnel is skipped even though Create would now happily allocate
		// a second key slot — stacking duplicates is a manual, deliberate act
		// (UCMP), not something a re-run of a full-mesh batch should do silently.
		if id, ok := s.pairTunnel(req.Local.NodeID, peer); ok {
			out.Skipped = append(out.Skipped, FanSkip{NodeID: peer,
				Reason: fmt.Sprintf("设备对之间已有隧道 %s（编排批次每对只建一条，第二条请手动创建）", id)})
			continue
		}
		c := s.Clone()
		t, err := c.create(CreateRequest{
			A: req.Local,
			B: EndpointSpec{NodeID: peer},
			// The path is cloned per tunnel: a tunnel owns its path, and two tunnels
			// sharing one *TunnelPath would both move if either is edited later.
			Path:    clonePath(req.Path),
			Color:   req.Color,
			Weight:  req.Weight,
			Dynamic: req.Dynamic,
			BatchID: batchID,
			Roles:   req.Roles,
			Now:     req.Now,
		}, views)
		if err != nil {
			out.Skipped = append(out.Skipped, FanSkip{NodeID: peer, Reason: err.Error()})
			continue
		}
		c.bump()
		*s = *c
		out.Created = append(out.Created, t)
	}
	return out, nil
}
