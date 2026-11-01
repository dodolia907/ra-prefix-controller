// ra-prefix-controller keeps a CiliumLoadBalancerIPPool inside the IPv6 prefix
// that the upstream router currently advertises by RA.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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

	h := newHealth()
	listener, err := net.Listen("tcp", cfg.healthAddr)
	if err != nil {
		return fmt.Errorf("health endpoint: %w", err)
	}
	server := &http.Server{Handler: h.handler(), ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	go func() {
		// A dead endpoint fails the liveness probe, which restarts the pod.
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("health endpoint", "error", err)
		}
	}()

	c := &controller{
		health:    h,
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
