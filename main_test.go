package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

func addr(s string, bits, preferred int, dynamic bool) raAddr {
	return raAddr{ip: netip.MustParseAddr(s), bits: bits, preferred: preferred, dynamic: dynamic}
}

func TestSelectPrefix(t *testing.T) {
	tests := []struct {
		name  string
		addrs []raAddr
		want  string
	}{
		{"slaac", []raAddr{addr("2001:db8:1:2:aaaa::1", 64, 3600, true)}, "2001:db8:1:2::/64"},
		{"ignores ula and link-local", []raAddr{addr("fd00:20::1", 64, 3600, true), addr("fe80::1", 64, 3600, true)}, ""},
		{"ignores static gua", []raAddr{addr("2001:db8::1", 64, 3600, false)}, ""},
		{"ignores non /64", []raAddr{addr("2001:db8::1", 128, 3600, true)}, ""},
		{"renumbering skips deprecated", []raAddr{addr("2001:db8:1::1", 64, 0, true), addr("2001:db8:2::1", 64, 1800, true)}, "2001:db8:2::/64"},
		{"longest preferred wins", []raAddr{addr("2001:db8:1::1", 64, 100, true), addr("2001:db8:2::1", 64, 1800, true)}, "2001:db8:2::/64"},
		{"nothing", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := selectPrefix(tt.addrs)
			if tt.want == "" {
				if ok {
					t.Fatalf("got %s, want none", got)
				}
				return
			}
			if !ok || got.String() != tt.want {
				t.Fatalf("got %s (%v), want %s", got, ok, tt.want)
			}
		})
	}
}

func TestPoolCIDR(t *testing.T) {
	got := poolCIDR(netip.MustParsePrefix("2001:db8:1:2::/64"), netip.MustParseAddr("::beef:0"), 112)
	if want := "2001:db8:1:2::beef:0/112"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestReconcilePool(t *testing.T) {
	const cidr = "2001:db8:2::beef:0/112"
	tests := []struct {
		name        string
		existing    string
		wantMethod  string
		wantChanged bool
	}{
		{"creates missing pool", "", http.MethodPost, true},
		{"patches old prefix", `{"spec":{"blocks":[{"cidr":"2001:db8:1::beef:0/112"}]}}`, http.MethodPatch, true},
		{"leaves matching pool alone", `{"spec":{"blocks":[{"cidr":"` + cidr + `"}]}}`, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var method string
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if tt.existing == "" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					io.WriteString(w, tt.existing)
					return
				}
				method = r.Method
				json.NewDecoder(r.Body).Decode(&body)
				if r.Method == http.MethodPost {
					w.WriteHeader(http.StatusCreated)
				}
			}))
			defer srv.Close()
			token := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(token, []byte("t"), 0o600); err != nil {
				t.Fatal(err)
			}
			c := &apiClient{base: srv.URL, http: srv.Client(), tokenFile: token}
			cfg := poolConfig{name: "gua", selector: map[string]string{"io.cilium/lb-pool": "gua"}}
			changed, err := reconcilePool(context.Background(), c, cfg, cidr)
			if err != nil {
				t.Fatal(err)
			}
			if changed != tt.wantChanged || method != tt.wantMethod {
				t.Fatalf("changed=%v method=%q, want %v %q", changed, method, tt.wantChanged, tt.wantMethod)
			}
			if method == http.MethodPost {
				spec := body["spec"].(map[string]any)
				if _, ok := spec["serviceSelector"]; !ok {
					t.Fatal("created pool has no serviceSelector")
				}
			}
		})
	}
}

func TestReconcilePoolReportsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("t"), 0o600)
	c := &apiClient{base: srv.URL, http: srv.Client(), tokenFile: token}
	if _, err := reconcilePool(context.Background(), c, poolConfig{name: "gua"}, "2001:db8::/112"); err == nil {
		t.Fatal("expected an error")
	}
}
