// Package netinfo resolves the agent host's public IP and country via
// ip-api.com. The free tier is HTTP-only; the data is non-sensitive (it is
// the same identifier the WS backend already sees as the connecting peer).
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
var LookupURL = "http://ip-api.com/json/?fields=status,country,countryCode,query"

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
		return Info{}, fmt.Errorf("ip-api status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return Info{}, err
	}
	var r struct {
		Status      string `json:"status"`
		Country     string `json:"country"`
		CountryCode string `json:"countryCode"`
		Query       string `json:"query"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Info{}, err
	}
	if r.Status != "success" {
		return Info{}, errors.New("ip-api status not success")
	}
	return Info{PublicIP: r.Query, Country: r.Country, CountryCode: r.CountryCode}, nil
}
