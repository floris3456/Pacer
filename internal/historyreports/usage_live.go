package historyreports

// The continuous usage record (docs/usage-unification.md, docs/usage-record.md):
// the prepared facts end at H; the usage record (usage_record.go) continues
// every account with the same CPAMP derivation, so the Usage Quota page has
// one window list. The page reads the record's facts once they are built.

import (
	"context"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
)

// LiveStatus describes the continuous record for the page's technical details.
type LiveStatus struct {
	State             string       `json:"state"` // starting, off, waiting, building, live, error
	ImportedThroughMS int64        `json:"imported_through_ms,omitempty"`
	BuiltAtMS         int64        `json:"built_at_ms,omitempty"`
	Record            RecordStatus `json:"record"`
	Error             string       `json:"error,omitempty"`
}

// UseLive makes the usage views read live facts (nil returns to the prepared
// facts). The exact workspace path always keeps the prepared facts.
func (r *Reader) UseLive(q *cpamp.QuotaFacts) {
	r.liveMu.Lock()
	r.live = q
	r.liveMu.Unlock()
}

// usageFacts are the live facts when set, otherwise the prepared facts.
func (r *Reader) usageFacts(ctx context.Context) (*cpamp.QuotaFacts, error) {
	frozen, err := r.quotaFacts(ctx)
	if err != nil {
		return nil, err
	}
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	if r.live != nil {
		return r.live, nil
	}
	return frozen, nil
}

// ImportedThrough is H, the end of the imported history (0 when unknown).
func (r *Reader) ImportedThrough(ctx context.Context) int64 {
	frozen, err := r.quotaFacts(ctx)
	if err != nil {
		return 0
	}
	return frozen.Filter.At
}

// Imported returns the imported profile configuration and price book.
func (r *Reader) Imported(ctx context.Context) (ForwardProfileConfig, ForwardComparisonBook, error) {
	frozen, err := r.quotaFacts(ctx)
	if err != nil {
		return ForwardProfileConfig{}, ForwardComparisonBook{}, err
	}
	return frozen.Config, frozen.PriceBook, nil
}

// ImportedModels lists every model of the imported requests with its provider.
func (r *Reader) ImportedModels(ctx context.Context) (map[string]string, error) {
	frozen, err := r.quotaFacts(ctx)
	if err != nil {
		return nil, err
	}
	m, err := r.service.EventModels(ctx, frozen)
	return m, domainError(ctx, err)
}
