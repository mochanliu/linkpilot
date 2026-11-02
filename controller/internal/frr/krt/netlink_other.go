//go:build !linux

package krt

import (
	"context"
	"errors"
	"net/netip"

	"sdwan/controller/internal/frr"
)

// 盒子是 Linux；这里只是让仓库在别的开发机上也能 go build。
type Client struct{}

var errUnsupported = errors.New("krt: 只有 Linux 才能直写内核路由表")

func New() (*Client, error) { return nil, errUnsupported }

func (c *Client) Close() error { return nil }

func (c *Client) RouteAdd(context.Context, frr.Route) error { return errUnsupported }

func (c *Client) RouteReplace(context.Context, frr.Route) error { return errUnsupported }

func (c *Client) RouteDelete(context.Context, frr.Route) error { return errUnsupported }

func (c *Client) Owns(context.Context, frr.Route) (bool, error) { return false, errUnsupported }

func (c *Client) LocalSubnets(uint32) map[netip.Prefix]bool { return nil }

func WatchLinks(context.Context) (<-chan LinkEvent, error) { return nil, errUnsupported }

var _ frr.DP = (*Client)(nil)
