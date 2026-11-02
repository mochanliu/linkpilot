//go:build !linux

package krt

import (
	"context"
	"errors"

	"sdwan/controller/internal/frr"
)

// 盒子是 Linux；这里只是让仓库在别的开发机上也能 go build。
type Client struct{}

var errUnsupported = errors.New("krt: 只有 Linux 才能直写内核路由表")

func New() (*Client, error) { return nil, errUnsupported }

func (c *Client) Close() error { return nil }

func (c *Client) RouteAdd(context.Context, frr.Route) error { return errUnsupported }

func (c *Client) RouteDelete(context.Context, frr.Route) error { return errUnsupported }

var _ frr.DP = (*Client)(nil)
