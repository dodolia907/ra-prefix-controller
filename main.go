// ra-prefix-controller keeps a CiliumLoadBalancerIPPool inside the IPv6 prefix
// that the upstream router currently advertises by RA.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/vishvananda/netlink"
)

func run(ctx context.Context) error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	client, err := newInClusterClient()
	if err != nil {
		return err
	}
	link, err := netlink.LinkByName(cfg.iface)
	if err != nil {
		return fmt.Errorf("interface %s: %w", cfg.iface, err)
	}

	updates := make(chan netlink.AddrUpdate, 16)
	done := make(chan struct{})
	defer close(done)
	if err := netlink.AddrSubscribe(updates, done); err != nil {
		return fmt.Errorf("subscribe to address changes: %w", err)
	}

	c := &controller{
		iface:     cfg.iface,
		linkIndex: link.Attrs().Index,
		poolName:  cfg.pool.name,
		hostPart:  cfg.hostPart,
		bits:      cfg.prefixBits,
		readAddrs: func() ([]raAddr, error) { return linkAddrs(link) },
		reconcile: func(ctx context.Context, cidr string) (bool, error) {
			return reconcilePool(ctx, client, cfg.pool, cidr)
		},
	}
	return c.loop(ctx, updates)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("exiting", "error", err)
		os.Exit(1)
	}
}
