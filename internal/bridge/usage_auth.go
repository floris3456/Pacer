package bridge

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

func validateUsageAuthURL(raw string) (string, error) {
	if raw == "" {
		raw = "http://127.0.0.1:8317/v1/models"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") ||
		u.Port() == "" || u.Path != "/v1/models" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errors.New("usage_auth_url must be the loopback CPA /v1/models endpoint")
	}
	return u.String(), nil
}

var usageAuthClient = &http.Client{
	Timeout:       5 * time.Second,
	Transport:     &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second},
	CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("usage authentication cannot redirect") },
}

// Delegate to CPA's existing frontend authentication without running a model or
// copying its configured keys. Check that the endpoint actually requires auth;
// an accidentally unauthenticated model-list server must not expose quota data.
func (p *Plugin) authenticateUsageKey(key string) (bool, error) {
	endpoint, err := validateUsageAuthURL(p.usageAuthURL)
	if err != nil {
		return false, err
	}
	for _, token := range []string{"", key} {
		req, _ := http.NewRequest("GET", endpoint, nil)
		req.Header.Set("Accept", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := usageAuthClient.Do(req)
		if err != nil {
			return false, errors.New("CPA usage authentication unavailable")
		}
		body, readErr := io.ReadAll(io.LimitReader(res.Body, 2<<20))
		res.Body.Close()
		if readErr != nil {
			return false, errors.New("CPA usage authentication response unavailable")
		}
		if token == "" {
			if res.StatusCode != 401 && res.StatusCode != 403 {
				return false, errors.New("CPA model list must require authentication")
			}
			continue
		}
		if res.StatusCode == 401 || res.StatusCode == 403 {
			return false, nil
		}
		var models struct {
			Object string          `json:"object"`
			Data   json.RawMessage `json:"data"`
		}
		if res.StatusCode != 200 || json.Unmarshal(body, &models) != nil || models.Object != "list" || len(models.Data) == 0 || models.Data[0] != '[' {
			return false, errors.New("unexpected CPA model-list response")
		}
	}
	return true, nil
}
