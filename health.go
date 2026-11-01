package main

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// livenessWindow is how long the loop may go without finishing a sync. Failed
// syncs count as finished: restarting cannot fix an API or RA outage.
const livenessWindow = 3 * resyncPeriod

type health struct {
	mu       sync.Mutex
	lastSync time.Time
	lastErr  error
	synced   bool
}

func newHealth() *health {
	return &health{lastSync: time.Now()}
}

func (h *health) record(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastSync, h.lastErr, h.synced = time.Now(), err, true
}

func (h *health) isReady() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.synced && h.lastErr == nil
}

func (h *health) isLive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return time.Since(h.lastSync) < livenessWindow
}

func (h *health) handler() http.Handler {
	respond := func(check func() bool) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			if !check() {
				w.WriteHeader(http.StatusServiceUnavailable)
				io.WriteString(w, "unhealthy\n")
				return
			}
			io.WriteString(w, "ok\n")
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/healthz", respond(h.isLive))
	mux.Handle("/readyz", respond(h.isReady))
	return mux
}
