package pacer

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/cachepolicy"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/policyadapter"
)

type PolicyConfig struct {
	Enabled                   bool                       `yaml:"enabled"`
	Routes                    []PolicyRoute              `yaml:"routes"`
	TrustedEmptyInstallations []TrustedEmptyInstallation `yaml:"trusted_empty_installations"`
}
type PolicyRoute struct {
	ID     string   `yaml:"id"`
	Models []string `yaml:"models"`
}
type TrustedEmptyInstallation struct {
	CallerDigest string `yaml:"caller_digest"`
	Installation string `yaml:"installation_id"`
}

func (p *Plugin) policyOptions(cfg PolicyConfig) (policyadapter.Options, error) {
	opts := policyadapter.Options{Clock: p.now}
	for _, route := range cfg.Routes {
		opts.Routes = append(opts.Routes, policyadapter.Route{ProviderRoute: route.ID, Models: route.Models, TTLs: []cachepolicy.TTL{cachepolicy.TTL5m, cachepolicy.TTL1h}})
	}
	if cfg.Enabled && len(opts.Routes) == 0 {
		return opts, errors.New("cache policy requires explicit supported routes")
	}
	trusted := map[string]bool{}
	for _, entry := range cfg.TrustedEmptyInstallations {
		bytes, err := hex.DecodeString(entry.CallerDigest)
		if err != nil || len(bytes) != 32 || strings.ToLower(entry.CallerDigest) != entry.CallerDigest || entry.Installation == "" || strings.ContainsAny(entry.Installation, "\x00\r\n ") {
			return opts, errors.New("invalid trusted empty-installation binding")
		}
		trusted[entry.CallerDigest+"\x00"+entry.Installation] = true
	}
	if len(trusted) > 0 {
		// This is explicit operator authority for this authenticated installation
		// to attest its own Core history, not proof from a client-selected label.
		// Default configuration trusts no caller; matching caller AND installation
		// is required, and capability/epoch checks still belong to the policy store.
		opts.NewEmptyAuthority = func(_ context.Context, claim policyadapter.EmptyClaim) (bool, error) {
			return trusted[claim.Scope.CallerScope+"\x00"+claim.Scope.InstallationID], nil
		}
	}
	_, err := policyadapter.PolicyOptions(opts)
	return opts, err
}
