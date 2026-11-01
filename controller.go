package main

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"time"

	"github.com/vishvananda/netlink"
)

const (
	debounce     = 5 * time.Second
	resyncPeriod = 5 * time.Minute
	retryPeriod  = 30 * time.Second
)

// controller points the pool at the prefix found on one network interface.
type controller struct {
	iface     string
	linkIndex int
	poolName  string
	hostPart  netip.Addr
	bits      int
	readAddrs func() ([]raAddr, error)
	reconcile func(ctx context.Context, cidr string) (bool, error)
}

// sync reconciles once and returns the delay until the next sync.
func (c *controller) sync(ctx context.Context) time.Duration {
	addrs, err := c.readAddrs()
	if err != nil {
		slog.Error("read addresses", "error", err)
		return retryPeriod
	}
	prefix, ok := selectPrefix(addrs)
	if !ok {
		// A lost RA must not take the public addresses away.
		slog.Warn("no preferred RA prefix found; leaving the pool unchanged", "interface", c.iface)
		return resyncPeriod
	}
	cidr := poolCIDR(prefix, c.hostPart, c.bits)
	changed, err := c.reconcile(ctx, cidr)
	if err != nil {
		slog.Error("reconcile pool", "pool", c.poolName, "error", err)
		return retryPeriod
	}
	if changed {
		slog.Info("pool updated", "pool", c.poolName, "cidr", cidr)
	}
	return resyncPeriod
}

// loop syncs immediately, then whenever the timer fires. The periodic sync only
// repairs drift and missed events; changes arrive through netlink.
func (c *controller) loop(ctx context.Context, updates <-chan netlink.AddrUpdate) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case u, ok := <-updates:
			if !ok {
				return errors.New("netlink subscription closed")
			}
			if u.LinkIndex == c.linkIndex {
				timer.Reset(debounce)
			}
		case <-timer.C:
			timer.Reset(c.sync(ctx))
		}
	}
}
