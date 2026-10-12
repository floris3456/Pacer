// Package policyadapter binds CPA's schema-7 RPCs to durable cache policy state.
// It owns no HTTP authentication, provider dispatch, quota polling or first-install initialization.
package policyadapter

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/cachepolicy"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type Route struct {
	ProviderRoute string
	// Models are exact host-resolved built-in Claude IDs, not aliases or patterns.
	Models []string
	TTLs   []cachepolicy.TTL
}

// EmptyClaim contains no capability. The hook must rely on a separate trusted
// authenticated client attestation, not just these client-selected identifiers.
type EmptyClaim struct {
	Scope        pluginapi.CachePolicyScope
	ContextEpoch string
	OperationID  string
}

type NewEmptyAuthority func(context.Context, EmptyClaim) (bool, error)

type Options struct {
	Clock             func() time.Time
	Routes            []Route
	NewEmptyAuthority NewEmptyAuthority // nil denies new-empty enrollment
	AuxiliaryNoCache  bool              // trusted host removes/rejects auxiliary cache-write instructions
	NativeCompaction  bool              // trusted host consumes an exact-credential replay capability
}

type Adapter struct {
	mu        sync.Mutex
	store     *cachepolicy.Store
	models    map[string]map[string]bool
	authority NewEmptyAuthority
	auxiliary bool
	native    bool
	requests  map[string]*logical
	completed map[string]cachepolicy.Outcome // no private credentials retained after completion
}

type logical struct {
	auth         cachepolicy.Credentials
	resolve      pluginapi.CachePolicyResolveRequest // Capability cleared; auth holds it privately
	policy       pluginapi.CachePolicy
	referenceTTL cachepolicy.TTL // admission reference; auxiliary selections intentionally carry no TTL
	attempts     map[string]*physical
	closing      cachepolicy.Outcome
	stopped      bool // a canceled physical attempt cannot be retried in this logical request
}

type physical struct {
	snapshot   cachepolicy.Snapshot
	validated  bool
	outcome    cachepolicy.Outcome
	dispatched bool
}

var _ pluginapi.CachePolicyProvider = (*Adapter)(nil)
var _ pluginapi.RequestLifecyclePlugin = (*Adapter)(nil)

// PolicyOptions validates and copies the policy subset of configuration. An
// explicit first-install controller may pass it to cachepolicy.Create outside
// every request path. Open itself only ever calls cachepolicy.Open.
func PolicyOptions(opts Options) (cachepolicy.Options, error) {
	if opts.Clock == nil {
		return cachepolicy.Options{}, reject(400, "invalid", "An injected clock is required.")
	}
	result := cachepolicy.Options{Clock: opts.Clock}
	seen := make(map[string]bool)
	for _, route := range opts.Routes {
		if !id(route.ProviderRoute) || seen[route.ProviderRoute] || len(route.Models) == 0 || len(route.TTLs) == 0 {
			return cachepolicy.Options{}, reject(400, "invalid", "Each route requires a unique ID, exact Claude model allowlist and supported TTLs.")
		}
		seen[route.ProviderRoute] = true
		models := make(map[string]bool)
		for _, model := range route.Models {
			if !id(model) || !strings.HasPrefix(model, "claude-") || strings.ContainsAny(model, "*/\\") || models[model] {
				return cachepolicy.Options{}, reject(400, "invalid", "Only unique, exact built-in Claude model IDs may be configured.")
			}
			models[model] = true
		}
		for _, ttl := range route.TTLs {
			if ttl != cachepolicy.TTL5m && ttl != cachepolicy.TTL1h {
				return cachepolicy.Options{}, reject(400, "invalid", "Unsupported configured TTL.")
			}
		}
		result.Routes = append(result.Routes, cachepolicy.Route{ProviderRoute: route.ProviderRoute, TTLs: append([]cachepolicy.TTL(nil), route.TTLs...)})
	}
	return result, nil
}

func Open(ctx context.Context, db *sql.DB, opts Options) (*Adapter, error) {
	policy, err := PolicyOptions(opts)
	if err != nil {
		return nil, err
	}
	store, err := cachepolicy.Open(ctx, db, policy)
	if err != nil {
		return nil, rpcError(err)
	}
	a := &Adapter{store: store, models: make(map[string]map[string]bool), authority: opts.NewEmptyAuthority, auxiliary: opts.AuxiliaryNoCache, native: opts.NativeCompaction, requests: make(map[string]*logical), completed: make(map[string]cachepolicy.Outcome)}
	for _, route := range opts.Routes {
		a.models[route.ProviderRoute] = make(map[string]bool)
		for _, model := range route.Models {
			a.models[route.ProviderRoute][model] = true
		}
	}
	return a, nil
}

func id(s string) bool {
	if len(s) < 1 || len(s) > 256 {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

func scope(s pluginapi.CachePolicyScope) cachepolicy.Scope {
	return cachepolicy.Scope{InstallationID: s.InstallationID, SessionID: s.SessionID, ProviderRoute: s.ProviderRoute}
}

func credentials(s pluginapi.CachePolicyScope, capability string) (cachepolicy.Credentials, error) {
	b, err := hex.DecodeString(s.CallerScope)
	if err != nil || len(b) != 32 || strings.ToLower(s.CallerScope) != s.CallerScope {
		return cachepolicy.Credentials{}, reject(403, "scope_denied", "Authenticated caller scope is required.")
	}
	if !id(s.InstallationID) || !id(s.SessionID) || !id(s.ProviderRoute) {
		return cachepolicy.Credentials{}, reject(400, "invalid", "Invalid policy scope.")
	}
	if capability == "" {
		return cachepolicy.Credentials{}, reject(403, "scope_denied", "A private policy capability is required.")
	}
	cap, err := base64.RawURLEncoding.DecodeString(capability)
	if err != nil || len(cap) != 32 || base64.RawURLEncoding.EncodeToString(cap) != capability {
		return cachepolicy.Credentials{}, reject(400, "invalid", "Invalid policy capability encoding.")
	}
	// CallerScope is already CPA's authenticated digest. Never hash it again.
	return cachepolicy.Credentials{CallerDigest: s.CallerScope, Capability: capability}, nil
}

func sameCredentials(a, b cachepolicy.Credentials) bool {
	return a.CallerDigest == b.CallerDigest && subtle.ConstantTimeCompare([]byte(a.Capability), []byte(b.Capability)) == 1
}

func (a *Adapter) route(route string) error {
	if a.models[route] == nil {
		return reject(422, "unsupported", "The provider route is not configured for managed Claude cache policy.")
	}
	return nil
}

func reject(status int, code, message string) error {
	return pluginabi.NewError("pacer_cache_"+code, message, status)
}

func rpcError(err error) error {
	if err == nil {
		return nil
	}
	var policy *cachepolicy.Error
	if errors.As(err, &policy) {
		return pluginabi.NewError(policy.Code, policy.Message, policy.Status)
	}
	var rpc *pluginabi.Error
	if errors.As(err, &rpc) {
		return rpc
	}
	return reject(503, "unavailable", "Durable cache policy is unavailable; reconcile before retrying.")
}
