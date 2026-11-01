// ra-prefix-controller keeps a CiliumLoadBalancerIPPool inside the IPv6 prefix
// that the upstream router currently advertises by RA.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
)

const (
	debounce     = 5 * time.Second
	resyncPeriod = 5 * time.Minute
	retryPeriod  = 30 * time.Second
)

func run(ctx context.Context) error {
	conf, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	cfg := conf.pool
	iface, hostPart, bits := conf.iface, conf.hostPart, conf.prefixBits

	client, err := newInClusterClient()
	if err != nil {
		return err
	}
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("interface %s: %w", iface, err)
	}

	updates := make(chan netlink.AddrUpdate, 16)
	done := make(chan struct{})
	defer close(done)
	if err := netlink.AddrSubscribe(updates, done); err != nil {
		return fmt.Errorf("subscribe to address changes: %w", err)
	}

	sync := func() time.Duration {
		addrs, err := linkAddrs(link)
		if err != nil {
			slog.Error("read addresses", "error", err)
			return retryPeriod
		}
		prefix, ok := selectPrefix(addrs)
		if !ok {
			// A lost RA must not take the public addresses away.
			slog.Warn("no preferred RA prefix found; leaving the pool unchanged", "interface", iface)
			return resyncPeriod
		}
		cidr := poolCIDR(prefix, hostPart, bits)
		changed, err := reconcilePool(ctx, client, cfg, cidr)
		if err != nil {
			slog.Error("reconcile pool", "pool", cfg.name, "error", err)
			return retryPeriod
		}
		if changed {
			slog.Info("pool updated", "pool", cfg.name, "cidr", cidr)
		}
		return resyncPeriod
	}

	// The periodic sync only repairs drift and missed events; changes arrive
	// through netlink.
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
			if u.LinkIndex == link.Attrs().Index {
				timer.Reset(debounce)
			}
		case <-timer.C:
			timer.Reset(sync())
		}
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("exiting", "error", err)
		os.Exit(1)
	}
}
