package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

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
