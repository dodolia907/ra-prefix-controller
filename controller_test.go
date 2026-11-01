package main

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vishvananda/netlink"
)

const (
	testLink = 7
	testCIDR = "2001:db8:1::beef:0/112"
)

// harness runs a controller loop whose reads and reconciles are recorded.
type harness struct {
	mu           sync.Mutex
	addrs        []raAddr
	readErr      error
	reconcileErr error
	cidrs        []string
	updates      chan netlink.AddrUpdate
	result       chan error
	cancel       context.CancelFunc
	health       *health
}

// startLoop must be called inside a synctest bubble.
func startLoop(t *testing.T, h *harness) {
	t.Helper()
	if h.addrs == nil {
		h.addrs = []raAddr{addr("2001:db8:1::1", 64, 3600, true)}
	}
	h.updates = make(chan netlink.AddrUpdate)
	h.health = newHealth()
	h.result = make(chan error, 1)
	c := &controller{
		iface:     "eth0",
		linkIndex: testLink,
		poolName:  "gua",
		hostPart:  netip.MustParseAddr("::beef:0"),
		bits:      112,
		readAddrs: func() ([]raAddr, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.addrs, h.readErr
		},
		health: h.health,
		reconcile: func(_ context.Context, cidr string) (bool, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.cidrs = append(h.cidrs, cidr)
			return true, h.reconcileErr
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.result <- c.loop(ctx, h.updates) }()
	t.Cleanup(cancel)
	synctest.Wait()
}

func (h *harness) wantSyncs(t *testing.T, want int) {
	t.Helper()
	synctest.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.cidrs) != want {
		t.Fatalf("%d syncs, want %d", len(h.cidrs), want)
	}
}

func (h *harness) stop(t *testing.T) {
	t.Helper()
	h.cancel()
	if err := <-h.result; err != nil {
		t.Fatalf("loop returned %v", err)
	}
}

func TestLoopSyncsAtStartupAndPeriodically(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &harness{}
		startLoop(t, h)
		h.wantSyncs(t, 1)
		h.mu.Lock()
		cidrs := slices.Clone(h.cidrs)
		h.mu.Unlock()
		if !slices.Equal(cidrs, []string{testCIDR}) {
			t.Fatalf("cidrs = %v", cidrs)
		}
		time.Sleep(resyncPeriod - time.Second)
		h.wantSyncs(t, 1)
		time.Sleep(time.Second)
		h.wantSyncs(t, 2)
		h.stop(t)
	})
}

func TestLoopDebouncesAddressUpdates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &harness{}
		startLoop(t, h)
		h.wantSyncs(t, 1)

		h.updates <- netlink.AddrUpdate{LinkIndex: testLink}
		time.Sleep(debounce - time.Second)
		h.wantSyncs(t, 1)

		// A second update restarts the wait.
		h.updates <- netlink.AddrUpdate{LinkIndex: testLink}
		time.Sleep(debounce - time.Second)
		h.wantSyncs(t, 1)

		time.Sleep(time.Second)
		h.wantSyncs(t, 2)
		h.stop(t)
	})
}

func TestLoopIgnoresOtherInterfaces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &harness{}
		startLoop(t, h)
		h.wantSyncs(t, 1)
		h.updates <- netlink.AddrUpdate{LinkIndex: testLink + 1}
		time.Sleep(2 * debounce)
		h.wantSyncs(t, 1)
		h.stop(t)
	})
}

func TestLoopRetriesAfterErrors(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*harness)
		// Reconciles seen after the first attempt and after the retry.
		first, second int
	}{
		{"reconcile", func(h *harness) { h.reconcileErr = errors.New("boom") }, 1, 2},
		{"read addresses", func(h *harness) { h.readErr = errors.New("boom") }, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := &harness{}
				tt.setup(h)
				startLoop(t, h)
				h.wantSyncs(t, tt.first)

				h.mu.Lock()
				h.readErr, h.reconcileErr = nil, nil
				h.mu.Unlock()
				time.Sleep(retryPeriod - time.Second)
				h.wantSyncs(t, tt.first)
				time.Sleep(time.Second)
				h.wantSyncs(t, tt.second)

				// Back to the normal period once it succeeds.
				time.Sleep(retryPeriod)
				h.wantSyncs(t, tt.second)
				h.stop(t)
			})
		})
	}
}

func TestLoopLeavesPoolAloneWithoutPrefix(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &harness{addrs: []raAddr{}}
		startLoop(t, h)
		h.wantSyncs(t, 0)
		time.Sleep(retryPeriod)
		h.wantSyncs(t, 0)

		// The prefix is picked up at the next resync.
		h.mu.Lock()
		h.addrs = []raAddr{addr("2001:db8:1::1", 64, 3600, true)}
		h.mu.Unlock()
		time.Sleep(resyncPeriod - retryPeriod)
		h.wantSyncs(t, 1)
		h.stop(t)
	})
}

func TestLoopFailsWhenSubscriptionCloses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &harness{}
		startLoop(t, h)
		close(h.updates)
		if err := <-h.result; err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestLoopKeepsPrefixUntilOtherLeadsByMargin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const week = 604800
		set := func(h *harness, one, two int) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.addrs = []raAddr{addr("2001:db8:1::1", 64, one, true), addr("2001:db8:2::1", 64, two, true)}
		}
		h := &harness{}
		startLoop(t, h)
		h.wantSyncs(t, 1)

		// The second prefix edges ahead, as lifetimes do between RAs.
		set(h, week-600, week)
		h.updates <- netlink.AddrUpdate{LinkIndex: testLink}
		time.Sleep(debounce)
		h.wantSyncs(t, 2)

		// The first prefix stops being advertised and falls behind.
		set(h, week-switchMargin-1, week)
		h.updates <- netlink.AddrUpdate{LinkIndex: testLink}
		time.Sleep(debounce)
		h.wantSyncs(t, 3)

		h.mu.Lock()
		cidrs := slices.Clone(h.cidrs)
		h.mu.Unlock()
		want := []string{testCIDR, testCIDR, "2001:db8:2::beef:0/112"}
		if !slices.Equal(cidrs, want) {
			t.Fatalf("cidrs = %v, want %v", cidrs, want)
		}
		h.stop(t)
	})
}

func TestLoopDoesNotAdoptPrefixWhenReconcileFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const week = 604800
		h := &harness{addrs: []raAddr{addr("2001:db8:1::1", 64, week, true)}, reconcileErr: errors.New("boom")}
		startLoop(t, h)
		h.wantSyncs(t, 1)
		if h.health.isReady() {
			t.Fatal("ready after a failed reconcile")
		}
		// Nothing was written, so the first prefix has no claim to be kept.
		h.mu.Lock()
		h.addrs = []raAddr{addr("2001:db8:1::1", 64, week-600, true), addr("2001:db8:2::1", 64, week, true)}
		h.reconcileErr = nil
		h.mu.Unlock()
		time.Sleep(retryPeriod)
		h.wantSyncs(t, 2)
		h.mu.Lock()
		last := h.cidrs[1]
		h.mu.Unlock()
		if last != "2001:db8:2::beef:0/112" {
			t.Fatalf("cidr = %s", last)
		}
		if !h.health.isReady() {
			t.Fatal("not ready after a successful reconcile")
		}
		h.stop(t)
	})
}
