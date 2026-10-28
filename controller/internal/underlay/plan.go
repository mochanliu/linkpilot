package underlay

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Mode is the topology the tenant picked for one batch.
type Mode string

const (
	ModeFullMesh Mode = "full-mesh"
	ModeHubSpoke Mode = "hub-spoke"
)

// SpokeMode is the "spoke 间隧道" dropdown, the only extra choice hub-spoke
// needs. It maps to the dataplane as follows:
//
//	none              hub-spoke only, every tunnel direct
//	via-hub           full mesh; the spoke<->spoke tunnels carry an SR path
//	                  through the hub, hub<->spoke stay direct
//	hub-transit-only  spoke<->spoke full mesh through the hub; the hub builds
//	                  NO tunnel at all and exists only as a relay hop
type SpokeMode string

const (
	SpokeNone       SpokeMode = "none"
	SpokeViaHub     SpokeMode = "via-hub"
	SpokeHubTransit SpokeMode = "hub-transit-only"
)

// AutoRequest is one "build everything that can be built" batch.
type AutoRequest struct {
	Nodes     []uint16
	Mode      Mode
	Hub       uint16
	SpokeMode SpokeMode
	BatchID   string
	Now       int64
}

// Skip is one pair the batch did not build, with the reason shown to the user.
// Silently dropping pairs is how a topology ends up with holes nobody can
// explain.
type Skip struct {
	A, B   uint16
	Reason string
}

// Plan is what a batch would do (or just did). Add holds fully allocated
// tunnels: ports and overlay /32s are already assigned, so the preview's
// numbers are the ones the apply will produce.
type Plan struct {
	Add  []Tunnel `json:"add"`
	Skip []Skip   `json:"skip"`
}

// Pairs is the number of device pairs the batch considered, built or not.
func (p Plan) Pairs() int { return len(p.Add) + len(p.Skip) }

type pair struct {
	a, b  uint16
	relay bool
}

// AutoPlan computes the batch without committing it.
func (s *State) AutoPlan(req AutoRequest, views map[uint16]NodeView) (Plan, error) {
	c := s.Clone()
	return c.auto(req, views)
}

// AutoApply commits the batch. It runs on a clone and swaps it in, so a batch
// that fails halfway (a validation bug, not a pool shortage) changes nothing.
func (s *State) AutoApply(req AutoRequest, views map[uint16]NodeView) (Plan, error) {
	c := s.Clone()
	p, err := c.auto(req, views)
	if err != nil {
		return p, err
	}
	*s = *c
	return p, nil
}

func (s *State) auto(req AutoRequest, views map[uint16]NodeView) (Plan, error) {
	pairs, err := autoPairs(req)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{Add: []Tunnel{}, Skip: []Skip{}}
	hubNode := req.Hub
	var hubTemplate string
	for _, pr := range pairs {
		skip := func(format string, args ...any) {
			plan.Skip = append(plan.Skip, Skip{A: pr.a, B: pr.b, Reason: fmt.Sprintf(format, args...)})
		}
		if s.HasPair(pr.a, pr.b) {
			skip("这对设备之间已经有隧道，跳过")
			continue
		}
		va, err := viewOf(views, "本端", pr.a)
		if err != nil {
			skip("%v", err)
			continue
		}
		vb, err := viewOf(views, "对端", pr.b)
		if err != nil {
			skip("%v", err)
			continue
		}
		if va.Stale || vb.Stale {
			skip("设备离线")
			continue
		}
		if va.Symmetric && vb.Symmetric {
			skip("两端都是对称 NAT，不可能建立")
			continue
		}
		if va.Observed == "" || vb.Observed == "" {
			skip("缺少观测到的外层地址")
			continue
		}
		if strings.TrimSpace(va.DefaultIface) == "" || strings.TrimSpace(vb.DefaultIface) == "" {
			skip("设备没有上报可用的物理端口")
			continue
		}
		var path *TunnelPath
		if pr.relay {
			hubView, err := viewOf(views, "中转设备", hubNode)
			if err != nil {
				skip("%v", err)
				continue
			}
			if hubView.Stale {
				skip("中转设备离线")
				continue
			}
			if hubTemplate == "" {
				hubTemplate = s.hubTemplate(hubNode, hubView, req.Now)
			}
			path = &TunnelPath{TemplateID: hubTemplate}
		}
		roles := Roles{}
		if req.Mode == ModeHubSpoke {
			if pr.a == hubNode {
				roles.A = "hub"
			} else {
				roles.A = "spoke"
			}
			if pr.b == hubNode {
				roles.B = "hub"
			} else {
				roles.B = "spoke"
			}
		}
		t, err := s.create(CreateRequest{
			A:       EndpointSpec{NodeID: pr.a},
			B:       EndpointSpec{NodeID: pr.b},
			Path:    path,
			BatchID: req.BatchID,
			Roles:   roles,
			Now:     req.Now,
		}, views)
		if err != nil {
			if errors.Is(err, ErrOverlayExhausted) || errors.Is(err, ErrPortExhausted) {
				skip("%v", err)
				continue
			}
			return plan, fmt.Errorf("设备 %d <-> %d: %w", pr.a, pr.b, err)
		}
		plan.Add = append(plan.Add, t)
	}
	return plan, nil
}

// hubTemplate reuses the auto template for a hub if one exists, so a second
// batch through the same hub shares it and an edit moves both batches.
func (s *State) hubTemplate(hub uint16, v NodeView, now int64) string {
	for _, tpl := range s.Paths {
		if tpl.NodeID == hub && tpl.Addr == "" && tpl.Name == autoTemplateName(hub, v) {
			return tpl.ID
		}
	}
	tpl, err := s.createPath(autoTemplateName(hub, v), hub, "", now)
	if err != nil {
		return ""
	}
	return tpl.ID
}

func autoTemplateName(hub uint16, v NodeView) string {
	if name := trimName(v.Name); name != "" {
		return "经 " + name + " 中转"
	}
	return fmt.Sprintf("经设备 %d 中转", hub)
}

func autoPairs(req AutoRequest) ([]pair, error) {
	nodes := dedupeNodes(req.Nodes)
	if len(nodes) < 2 {
		return nil, fmt.Errorf("至少选择 2 台设备")
	}
	switch req.Mode {
	case ModeFullMesh:
		return allPairs(nodes, false), nil
	case ModeHubSpoke:
		hub := req.Hub
		if hub == 0 || !contains(nodes, hub) {
			return nil, fmt.Errorf("hub-spoke 需要选择一个在选中设备里的 hub")
		}
		switch req.SpokeMode {
		case "", SpokeNone:
			out := make([]pair, 0, len(nodes)-1)
			for _, n := range nodes {
				if n == hub {
					continue
				}
				out = append(out, orderedPair(hub, n, false))
			}
			return out, nil
		case SpokeViaHub:
			all := allPairs(nodes, false)
			for i := range all {
				all[i].relay = all[i].a != hub && all[i].b != hub
			}
			return all, nil
		case SpokeHubTransit:
			spokes := make([]uint16, 0, len(nodes)-1)
			for _, n := range nodes {
				if n != hub {
					spokes = append(spokes, n)
				}
			}
			if len(spokes) < 2 {
				return nil, fmt.Errorf("“hub 仅中转”至少需要 2 台 spoke 之间才有隧道可建")
			}
			return allPairs(spokes, true), nil
		default:
			return nil, fmt.Errorf("未知的 spoke 间隧道模式 %q", req.SpokeMode)
		}
	default:
		return nil, fmt.Errorf("未知的模型 %q", req.Mode)
	}
}

func allPairs(nodes []uint16, relay bool) []pair {
	out := make([]pair, 0, len(nodes)*(len(nodes)-1)/2)
	for i := 0; i < len(nodes); i++ {
		for j := i + 1; j < len(nodes); j++ {
			out = append(out, pair{a: nodes[i], b: nodes[j], relay: relay})
		}
	}
	return out
}

func orderedPair(a, b uint16, relay bool) pair {
	if a > b {
		a, b = b, a
	}
	return pair{a: a, b: b, relay: relay}
}

func dedupeNodes(in []uint16) []uint16 {
	seen := map[uint16]bool{}
	out := make([]uint16, 0, len(in))
	for _, n := range in {
		if n == 0 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func contains(list []uint16, v uint16) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// HasPair reports whether any tunnel already connects two devices. Duplicate
// detection is at the PAIR level: a second tunnel between the same two devices
// is a deliberate manual act, never something a batch re-creates.
func (s *State) HasPair(a, b uint16) bool {
	for _, t := range s.Tunnels {
		if (t.A.NodeID == a && t.B.NodeID == b) || (t.A.NodeID == b && t.B.NodeID == a) {
			return true
		}
	}
	return false
}
