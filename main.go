// ra-prefix-controller keeps a CiliumLoadBalancerIPPool inside the IPv6 prefix
// that the upstream router currently advertises by RA.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const (
	poolsPath    = "/apis/cilium.io/v2/ciliumloadbalancerippools"
	saDir        = "/var/run/secrets/kubernetes.io/serviceaccount"
	debounce     = 5 * time.Second
	resyncPeriod = 5 * time.Minute
	retryPeriod  = 30 * time.Second
)

// raAddr is the part of an interface address that prefix selection needs.
type raAddr struct {
	ip        netip.Addr
	bits      int
	preferred int
	dynamic   bool
}

// selectPrefix returns the /64 of the SLAAC address with the longest preferred
// lifetime. After renumbering the old prefix stays on the link, deprecated,
// until its valid lifetime ends, so deprecated addresses are skipped.
func selectPrefix(addrs []raAddr) (netip.Prefix, bool) {
	global := netip.MustParsePrefix("2000::/3")
	var best raAddr
	found := false
	for _, a := range addrs {
		if !a.dynamic || a.bits != 64 || a.preferred <= 0 || !global.Contains(a.ip) {
			continue
		}
		if !found || a.preferred > best.preferred {
			best, found = a, true
		}
	}
	if !found {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(best.ip, 64).Masked(), true
}

// poolCIDR places hostPart in the interface identifier of a /64 prefix.
func poolCIDR(prefix netip.Prefix, hostPart netip.Addr, bits int) string {
	p, h := prefix.Addr().As16(), hostPart.As16()
	copy(p[8:], h[8:])
	return netip.PrefixFrom(netip.AddrFrom16(p), bits).Masked().String()
}

type apiClient struct {
	base      string
	http      *http.Client
	tokenFile string
}

func newInClusterClient() (*apiClient, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("KUBERNETES_SERVICE_HOST/PORT are not set")
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("no certificates in ca.crt")
	}
	return &apiClient{
		base:      "https://" + net.JoinHostPort(host, port),
		tokenFile: saDir + "/token",
		http: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}},
		},
	}, nil
}

func (c *apiClient) do(ctx context.Context, method, path, contentType string, body []byte) (int, []byte, error) {
	// The projected token rotates, so it is read for every request.
	token, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, data, err
}

type poolConfig struct {
	name     string
	selector map[string]string
}

// reconcilePool creates the pool or points it at cidr, and reports whether it
// changed anything.
func reconcilePool(ctx context.Context, c *apiClient, cfg poolConfig, cidr string) (bool, error) {
	blocks := []map[string]string{{"cidr": cidr}}
	status, data, err := c.do(ctx, http.MethodGet, poolsPath+"/"+cfg.name, "", nil)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusNotFound:
		spec := map[string]any{"blocks": blocks}
		if len(cfg.selector) > 0 {
			spec["serviceSelector"] = map[string]any{"matchLabels": cfg.selector}
		}
		body, err := json.Marshal(map[string]any{
			"apiVersion": "cilium.io/v2",
			"kind":       "CiliumLoadBalancerIPPool",
			"metadata":   map[string]any{"name": cfg.name},
			"spec":       spec,
		})
		if err != nil {
			return false, err
		}
		status, data, err = c.do(ctx, http.MethodPost, poolsPath, "application/json", body)
		if err != nil {
			return false, err
		}
		if status != http.StatusCreated {
			return false, fmt.Errorf("create pool: HTTP %d: %s", status, data)
		}
		return true, nil
	case http.StatusOK:
	default:
		return false, fmt.Errorf("get pool: HTTP %d: %s", status, data)
	}

	var pool struct {
		Spec struct {
			Blocks []struct {
				CIDR string `json:"cidr"`
			} `json:"blocks"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(data, &pool); err != nil {
		return false, err
	}
	if len(pool.Spec.Blocks) == 1 && pool.Spec.Blocks[0].CIDR == cidr {
		return false, nil
	}
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"blocks": blocks}})
	if err != nil {
		return false, err
	}
	status, data, err = c.do(ctx, http.MethodPatch, poolsPath+"/"+cfg.name, "application/merge-patch+json", body)
	if err != nil {
		return false, err
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("patch pool: HTTP %d: %s", status, data)
	}
	return true, nil
}

func linkAddrs(link netlink.Link) ([]raAddr, error) {
	list, err := netlink.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		return nil, err
	}
	out := make([]raAddr, 0, len(list))
	for _, a := range list {
		ip, ok := netip.AddrFromSlice(a.IP)
		if !ok {
			continue
		}
		bits, _ := a.Mask.Size()
		out = append(out, raAddr{
			ip:        ip.Unmap(),
			bits:      bits,
			preferred: a.PreferedLft,
			dynamic:   a.Flags&unix.IFA_F_PERMANENT == 0,
		})
	}
	return out, nil
}

// parseLabels parses "key=value,key=value".
func parseLabels(s string) (map[string]string, error) {
	labels := map[string]string{}
	if s == "" {
		return labels, nil
	}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid label %q", kv)
		}
		labels[k] = v
	}
	return labels, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func run(ctx context.Context) error {
	iface := os.Getenv("INTERFACE")
	if iface == "" {
		return errors.New("INTERFACE is required")
	}
	hostPart, err := netip.ParseAddr(envOr("POOL_HOST_PART", "::beef:0"))
	if err != nil || !hostPart.Is6() {
		return fmt.Errorf("invalid POOL_HOST_PART: %v", err)
	}
	bits, err := strconv.Atoi(envOr("POOL_PREFIX_LENGTH", "112"))
	if err != nil || bits < 64 || bits > 128 {
		return errors.New("POOL_PREFIX_LENGTH must be between 64 and 128")
	}
	selector, err := parseLabels(os.Getenv("POOL_SERVICE_SELECTOR"))
	if err != nil {
		return fmt.Errorf("POOL_SERVICE_SELECTOR: %w", err)
	}
	cfg := poolConfig{name: envOr("POOL_NAME", "ipv6-ndp-pool-gua"), selector: selector}

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
