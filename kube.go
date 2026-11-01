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
