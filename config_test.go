package main

import (
	"maps"
	"net/netip"
	"strings"
	"testing"
)

func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{"INTERFACE": "enp3s0"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.iface != "enp3s0" || cfg.hostPart != netip.MustParseAddr("::beef:0") || cfg.prefixBits != 112 || cfg.healthAddr != "127.0.0.1:18091" ||
		cfg.pool.name != "ipv6-ndp-pool-gua" || len(cfg.pool.selector) != 0 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"INTERFACE":             "eth0",
		"POOL_NAME":             "gua",
		"POOL_HOST_PART":        "::cafe:0",
		"POOL_PREFIX_LENGTH":    "120",
		"POOL_SERVICE_SELECTOR": "a=1,b=2",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.iface != "eth0" || cfg.hostPart != netip.MustParseAddr("::cafe:0") || cfg.prefixBits != 120 || cfg.pool.name != "gua" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if want := map[string]string{"a": "1", "b": "2"}; !maps.Equal(cfg.pool.selector, want) {
		t.Fatalf("selector = %v, want %v", cfg.pool.selector, want)
	}
}

func TestLoadConfigRejectsInvalid(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
	}{
		{"missing interface", "INTERFACE", ""},
		{"unparsable host part", "POOL_HOST_PART", "nope"},
		{"ipv4 host part", "POOL_HOST_PART", "192.0.2.1"},
		{"non-numeric length", "POOL_PREFIX_LENGTH", "x"},
		{"length too short", "POOL_PREFIX_LENGTH", "63"},
		{"length too long", "POOL_PREFIX_LENGTH", "129"},
		{"non-numeric health port", "HEALTH_PORT", "x"},
		{"health port out of range", "HEALTH_PORT", "65536"},
		{"label without value", "POOL_SERVICE_SELECTOR", "a"},
		{"label without key", "POOL_SERVICE_SELECTOR", "=b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := map[string]string{"INTERFACE": "eth0"}
			vars[tt.key] = tt.val
			if _, err := loadConfig(env(vars)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestLoadConfigExplainsIPv4HostPart(t *testing.T) {
	_, err := loadConfig(env(map[string]string{"INTERFACE": "eth0", "POOL_HOST_PART": "192.0.2.1"}))
	if err == nil || strings.Contains(err.Error(), "<nil>") {
		t.Fatalf("got %v, want a descriptive error", err)
	}
}
