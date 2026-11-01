package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"
)

func status(h *health, path string) int {
	w := httptest.NewRecorder()
	h.handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w.Code
}

func TestReadiness(t *testing.T) {
	h := newHealth()
	if got := status(h, "/readyz"); got != http.StatusServiceUnavailable {
		t.Fatalf("before the first sync: %d", got)
	}
	h.record(nil)
	if got := status(h, "/readyz"); got != http.StatusOK {
		t.Fatalf("after a success: %d", got)
	}
	h.record(errNoPrefix)
	if got := status(h, "/readyz"); got != http.StatusServiceUnavailable {
		t.Fatalf("without a prefix: %d", got)
	}
}

func TestLiveness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHealth()
		if got := status(h, "/healthz"); got != http.StatusOK {
			t.Fatalf("at startup: %d", got)
		}
		time.Sleep(livenessWindow - time.Second)
		// A failed sync still shows that the loop is running.
		h.record(errors.New("boom"))
		time.Sleep(livenessWindow - time.Second)
		if got := status(h, "/healthz"); got != http.StatusOK {
			t.Fatalf("after a failed sync: %d", got)
		}
		time.Sleep(time.Second)
		if got := status(h, "/healthz"); got != http.StatusServiceUnavailable {
			t.Fatalf("after the loop stalled: %d", got)
		}
	})
}
