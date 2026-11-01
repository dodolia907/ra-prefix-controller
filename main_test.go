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
	"reflect"
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

func newTestClient(t *testing.T, h http.HandlerFunc) *apiClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &apiClient{base: srv.URL, http: srv.Client(), tokenFile: token}
}

func TestReconcilePool(t *testing.T) {
	const cidr = "2001:db8:2::beef:0/112"
	selector := map[string]string{"io.cilium/lb-pool": "gua"}
	tests := []struct {
		name        string
		selector    map[string]string
		existing    string
		wantMethod  string
		wantPath    string
		wantType    string
		wantBody    string
		wantChanged bool
	}{
		{
			name:       "creates missing pool",
			selector:   selector,
			wantMethod: http.MethodPost,
			wantPath:   poolsPath,
			wantType:   "application/json",
			wantBody: `{"apiVersion":"cilium.io/v2","kind":"CiliumLoadBalancerIPPool","metadata":{"name":"gua"},` +
				`"spec":{"blocks":[{"cidr":"` + cidr + `"}],"serviceSelector":{"matchLabels":{"io.cilium/lb-pool":"gua"}}}}`,
			wantChanged: true,
		},
		{
			name:       "creates missing pool without selector",
			wantMethod: http.MethodPost,
			wantPath:   poolsPath,
			wantType:   "application/json",
			wantBody: `{"apiVersion":"cilium.io/v2","kind":"CiliumLoadBalancerIPPool","metadata":{"name":"gua"},` +
				`"spec":{"blocks":[{"cidr":"` + cidr + `"}]}}`,
			wantChanged: true,
		},
		{
			name:        "patches old prefix without touching the selector",
			selector:    selector,
			existing:    `{"spec":{"blocks":[{"cidr":"2001:db8:1::beef:0/112"}]}}`,
			wantMethod:  http.MethodPatch,
			wantPath:    poolsPath + "/gua",
			wantType:    "application/merge-patch+json",
			wantBody:    `{"spec":{"blocks":[{"cidr":"` + cidr + `"}]}}`,
			wantChanged: true,
		},
		{
			name:        "replaces extra blocks",
			existing:    `{"spec":{"blocks":[{"cidr":"` + cidr + `"},{"cidr":"2001:db8:9::/112"}]}}`,
			wantMethod:  http.MethodPatch,
			wantPath:    poolsPath + "/gua",
			wantType:    "application/merge-patch+json",
			wantBody:    `{"spec":{"blocks":[{"cidr":"` + cidr + `"}]}}`,
			wantChanged: true,
		},
		{name: "leaves matching pool alone", existing: `{"spec":{"blocks":[{"cidr":"` + cidr + `"}]}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var method, path, contentType, body string
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if tt.existing == "" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					io.WriteString(w, tt.existing)
					return
				}
				data, _ := io.ReadAll(r.Body)
				method, path, contentType, body = r.Method, r.URL.Path, r.Header.Get("Content-Type"), string(data)
				if r.Method == http.MethodPost {
					w.WriteHeader(http.StatusCreated)
				}
			})
			changed, err := reconcilePool(context.Background(), c, poolConfig{name: "gua", selector: tt.selector}, cidr)
			if err != nil {
				t.Fatal(err)
			}
			if changed != tt.wantChanged || method != tt.wantMethod || path != tt.wantPath || contentType != tt.wantType {
				t.Fatalf("changed=%v %s %s (%s), want %v %s %s (%s)",
					changed, method, path, contentType, tt.wantChanged, tt.wantMethod, tt.wantPath, tt.wantType)
			}
			if tt.wantBody != "" {
				var got, want any
				if err := json.Unmarshal([]byte(body), &got); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(tt.wantBody), &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("body = %s, want %s", body, tt.wantBody)
				}
			}
		})
	}
}

func TestReconcilePoolReportsAPIError(t *testing.T) {
	tests := []struct {
		name       string
		get, write int
	}{
		{"get", http.StatusForbidden, 0},
		{"create", http.StatusNotFound, http.StatusForbidden},
		{"patch", http.StatusOK, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.WriteHeader(tt.get)
					if tt.get == http.StatusOK {
						io.WriteString(w, `{"spec":{"blocks":[]}}`)
					}
					return
				}
				w.WriteHeader(tt.write)
			})
			changed, err := reconcilePool(context.Background(), c, poolConfig{name: "gua"}, "2001:db8::/112")
			if err == nil || changed {
				t.Fatalf("changed=%v err=%v, want false and an error", changed, err)
			}
		})
	}
}
