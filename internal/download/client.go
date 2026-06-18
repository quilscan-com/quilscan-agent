// Package download provides the HTTP client used by release/download paths.
package download

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	mu       sync.RWMutex
	proxyURL *url.URL
)

// ConfigureProxy sets the optional proxy used by agent download operations.
// Empty input clears the explicit proxy and falls back to environment proxy
// variables through http.ProxyFromEnvironment.
func ConfigureProxy(raw string) error {
	parsed, err := normalizeProxyURL(raw)
	if err != nil {
		return err
	}
	mu.Lock()
	proxyURL = parsed
	mu.Unlock()
	return nil
}

func ConfiguredProxyURL() string {
	mu.RLock()
	defer mu.RUnlock()
	if proxyURL == nil {
		return ""
	}
	return proxyURL.String()
}

// NewClient returns a client whose transport reads the current proxy setting
// at request time, so tests and startup configuration do not depend on package
// init order.
func NewClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = proxyForRequest
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}

func proxyForRequest(req *http.Request) (*url.URL, error) {
	mu.RLock()
	configured := proxyURL
	mu.RUnlock()
	if configured != nil {
		return configured, nil
	}
	return http.ProxyFromEnvironment(req)
}

func normalizeProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse download proxy url: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("download proxy url missing host")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5":
	default:
		return nil, fmt.Errorf("unsupported download proxy scheme %q", u.Scheme)
	}
	return u, nil
}
