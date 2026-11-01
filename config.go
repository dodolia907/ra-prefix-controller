package main

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

type config struct {
	iface      string
	hostPart   netip.Addr
	prefixBits int
	pool       poolConfig
}

// loadConfig reads the configuration through getenv, normally os.Getenv.
func loadConfig(getenv func(string) string) (config, error) {
	envOr := func(key, fallback string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fallback
	}
	iface := getenv("INTERFACE")
	if iface == "" {
		return config{}, errors.New("INTERFACE is required")
	}
	hostPart, err := netip.ParseAddr(envOr("POOL_HOST_PART", "::beef:0"))
	if err != nil {
		return config{}, fmt.Errorf("invalid POOL_HOST_PART: %w", err)
	}
	if !hostPart.Is6() {
		return config{}, fmt.Errorf("invalid POOL_HOST_PART: %s is not an IPv6 address", hostPart)
	}
	bits, err := strconv.Atoi(envOr("POOL_PREFIX_LENGTH", "112"))
	if err != nil || bits < 64 || bits > 128 {
		return config{}, errors.New("POOL_PREFIX_LENGTH must be between 64 and 128")
	}
	selector, err := parseLabels(getenv("POOL_SERVICE_SELECTOR"))
	if err != nil {
		return config{}, fmt.Errorf("POOL_SERVICE_SELECTOR: %w", err)
	}
	return config{
		iface:      iface,
		hostPart:   hostPart,
		prefixBits: bits,
		pool:       poolConfig{name: envOr("POOL_NAME", "ipv6-ndp-pool-gua"), selector: selector},
	}, nil
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
