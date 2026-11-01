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
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	poolsPath = "/apis/cilium.io/v2/ciliumloadbalancerippools"
	saDir     = "/var/run/secrets/kubernetes.io/serviceaccount"
)

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

type lbPool struct {
	APIVersion string     `json:"apiVersion,omitempty"`
	Kind       string     `json:"kind,omitempty"`
	Metadata   *poolMeta  `json:"metadata,omitempty"`
	Spec       lbPoolSpec `json:"spec"`
}

type poolMeta struct {
	Name string `json:"name"`
}

type lbPoolSpec struct {
	Blocks          []poolBlock      `json:"blocks"`
	ServiceSelector *serviceSelector `json:"serviceSelector,omitempty"`
}

type poolBlock struct {
	CIDR string `json:"cidr"`
}

type serviceSelector struct {
	MatchLabels map[string]string `json:"matchLabels"`
}

// reconcilePool creates the pool or points it at cidr, and reports whether it
// changed anything.
func reconcilePool(ctx context.Context, c *apiClient, cfg poolConfig, cidr string) (bool, error) {
	status, data, err := c.do(ctx, http.MethodGet, poolsPath+"/"+cfg.name, "", nil)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusNotFound:
		if err := createPool(ctx, c, cfg, cidr); err != nil {
			return false, err
		}
		return true, nil
	case http.StatusOK:
	default:
		return false, fmt.Errorf("get pool: HTTP %d: %s", status, data)
	}

	var pool lbPool
	if err := json.Unmarshal(data, &pool); err != nil {
		return false, err
	}
	if len(pool.Spec.Blocks) == 1 && pool.Spec.Blocks[0].CIDR == cidr {
		return false, nil
	}
	if err := patchPoolCIDR(ctx, c, cfg.name, cidr); err != nil {
		return false, err
	}
	return true, nil
}

func createPool(ctx context.Context, c *apiClient, cfg poolConfig, cidr string) error {
	pool := lbPool{
		APIVersion: "cilium.io/v2",
		Kind:       "CiliumLoadBalancerIPPool",
		Metadata:   &poolMeta{Name: cfg.name},
		Spec:       lbPoolSpec{Blocks: []poolBlock{{CIDR: cidr}}},
	}
	if len(cfg.selector) > 0 {
		pool.Spec.ServiceSelector = &serviceSelector{MatchLabels: cfg.selector}
	}
	body, err := json.Marshal(pool)
	if err != nil {
		return err
	}
	status, data, err := c.do(ctx, http.MethodPost, poolsPath, "application/json", body)
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("create pool: HTTP %d: %s", status, data)
	}
	return nil
}

// patchPoolCIDR replaces only spec.blocks, so other fields keep their values.
func patchPoolCIDR(ctx context.Context, c *apiClient, name, cidr string) error {
	body, err := json.Marshal(lbPool{Spec: lbPoolSpec{Blocks: []poolBlock{{CIDR: cidr}}}})
	if err != nil {
		return err
	}
	status, data, err := c.do(ctx, http.MethodPatch, poolsPath+"/"+name, "application/merge-patch+json", body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("patch pool: HTTP %d: %s", status, data)
	}
	return nil
}
