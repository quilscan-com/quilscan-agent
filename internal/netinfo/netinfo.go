// Package netinfo resolves the agent host's public IP and country via an HTTPS
// geo-IP endpoint. The data is non-sensitive (it is the same identifier the WS
// backend already sees as the connecting peer), but HTTPS prevents casual
// network-path tampering of the displayed location.
package netinfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Info is the resolved netinfo for the host.
type Info struct {
	PublicIP    string `json:"public_ip"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
}

// LookupURL is the upstream endpoint. Exposed for tests.
var LookupURL = "https://ifconfig.co/json"

// Lookup queries the upstream and returns the host's public IP + country.
func Lookup(ctx context.Context) (Info, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", LookupURL, nil)
	if err != nil {
		return Info{}, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Info{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Info{}, fmt.Errorf("geo-ip status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return Info{}, err
	}
	var r struct {
		IP          string `json:"ip"`
		Status      string `json:"status"`
		Country     string `json:"country"`
		CountryCode string `json:"countryCode"`
		CountryISO  string `json:"country_iso"`
		Query       string `json:"query"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Info{}, err
	}
	if r.Status != "" && r.Status != "success" {
		return Info{}, errors.New("geo-ip status not success")
	}
	publicIP := firstNonEmpty(r.IP, r.Query)
	countryCode := firstNonEmpty(r.CountryISO, r.CountryCode)
	if publicIP == "" || r.Country == "" || countryCode == "" {
		return Info{}, errors.New("geo-ip response missing required fields")
	}
	return Info{PublicIP: publicIP, Country: r.Country, CountryCode: countryCode}, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
