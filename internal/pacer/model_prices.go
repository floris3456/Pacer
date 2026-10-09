package pacer

// Model prices for Model Settings and the usage record: models.dev is fetched
// once a day (and on request) through CPA's HTTP client; prices typed by hand
// are append-only revisions in the forward store. The usage record values
// requests with: a manual price, else the imported price book, else models.dev.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	modelsRoot      = "/v0/management/plugins/pacer/models/"
	priceSyncEvery  = 24 * time.Hour
	priceSyncCheck  = time.Hour
	priceSyncLimit  = 32 << 20
	priceSyncWindow = 45 * time.Second
)

type priceSync struct{ mu sync.Mutex }

func (p *Plugin) startPriceSync() {
	p.workerWait.Add(1)
	go func() {
		defer p.workerWait.Done()
		timer := time.NewTimer(2 * time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-p.workerDone:
				return
			case <-timer.C:
			}
			p.mu.Lock()
			store := p.recordStore
			p.mu.Unlock()
			if store != nil {
				ctx, cancel := context.WithTimeout(context.Background(), priceSyncWindow)
				last, err := store.PriceSync(ctx)
				if err == nil && p.now().Sub(time.UnixMilli(last.CheckedMS)) >= priceSyncEvery {
					_, _ = p.syncPrices(ctx)
				}
				cancel()
			}
			timer.Reset(priceSyncCheck)
		}
	}()
}

// syncPrices fetches the models.dev catalog through CPA's HTTP client.
func (p *Plugin) syncPrices(ctx context.Context) (historyreports.PriceSync, error) {
	p.priceSync.mu.Lock()
	defer p.priceSync.mu.Unlock()
	p.mu.Lock()
	store, host := p.recordStore, p.Host
	p.mu.Unlock()
	if store == nil || host == nil {
		return historyreports.PriceSync{}, errUsageExclusionsUnavailable
	}
	last, err := store.PriceSync(ctx)
	if err != nil {
		return last, err
	}
	now := p.now()
	last.CheckedMS = now.UnixMilli()
	fail := func(message string) (historyreports.PriceSync, error) {
		last.Error = message
		_ = store.SetPriceSync(ctx, last)
		return last, errors.New(message)
	}
	opRaw, err := host("host.http.operation_open", map[string]any{})
	if err != nil {
		return fail("CPA could not start the request.")
	}
	var op struct {
		ID string `json:"operation_id"`
	}
	if json.Unmarshal(opRaw, &op) != nil || op.ID == "" {
		return fail("CPA could not start the request.")
	}
	headers := map[string][]string{"Accept": {"application/json"}, "User-Agent": {"Pacer/1 (price sync)"}}
	if last.ETag != "" && len(last.Catalog) > 0 {
		headers["If-None-Match"] = []string{last.ETag}
	}
	cancelled := make(chan struct{})
	timer := time.AfterFunc(40*time.Second, func() { _, _ = host("host.http.cancel", map[string]string{"operation_id": op.ID}); close(cancelled) })
	raw, err := host("host.http.do", map[string]any{"operation_id": op.ID, "method": "GET", "url": historyreports.ModelsDevURL, "headers": headers})
	if !timer.Stop() {
		<-cancelled
	}
	if err != nil {
		return fail("models.dev could not be reached.")
	}
	if len(raw) > priceSyncLimit {
		return fail("models.dev answered with too much data.")
	}
	var response pluginapi.HTTPResponse
	if json.Unmarshal(raw, &response) != nil {
		return fail("models.dev answered with something unreadable.")
	}
	switch response.StatusCode {
	case 304:
		last.Error, last.SyncedAtMS = "", now.UnixMilli()
		return last, store.SetPriceSync(ctx, last)
	case 200:
	default:
		return fail("models.dev answered with HTTP " + itoa(response.StatusCode) + ".")
	}
	catalog, err := historyreports.ParseModelsDev(response.Body)
	if err != nil {
		return fail("models.dev answered with an unreadable catalog.")
	}
	last.Catalog, last.SyncedAtMS, last.Error, last.ETag = catalog, now.UnixMilli(), "", ""
	if response.Headers != nil {
		last.ETag = response.Headers.Get("ETag")
	}
	if err = store.SetPriceSync(ctx, last); err != nil {
		return last, err
	}
	p.wakeUsageLive()
	return last, nil
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

type modelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type modelPriceQuery struct {
	Models []modelRef `json:"models"`
}

type priceView struct {
	Input      float64           `json:"input"`
	Output     float64           `json:"output"`
	CacheRead  *float64          `json:"cache_read,omitempty"`
	CacheWrite *float64          `json:"cache_write,omitempty"`
	Tiers      []priceTierView   `json:"context_tiers,omitempty"`
	Fast       *priceView        `json:"fast,omitempty"`
	SourceID   string            `json:"source_model,omitempty"`
	Extra      map[string]string `json:"-"`
}

type priceTierView struct {
	AboveTokens int64     `json:"above_tokens"`
	Price       priceView `json:"price"`
}

func optional(v float64, set bool) *float64 {
	if !set {
		return nil
	}
	return &v
}

func viewPrice(p historyreports.ForwardModelPrice) priceView {
	v := priceView{Input: p.Prompt, Output: p.Completion, CacheRead: optional(p.CacheRead, p.CacheReadConfigured), CacheWrite: optional(p.CacheCreation, p.CacheCreationConfigured), SourceID: p.SourceModelID}
	for _, t := range p.ContextTiers {
		v.Tiers = append(v.Tiers, priceTierView{AboveTokens: t.ThresholdTokens, Price: priceView{Input: t.Prompt, Output: t.Completion, CacheRead: optional(t.CacheRead, t.CacheReadConfigured), CacheWrite: optional(t.CacheCreation, t.CacheCreationConfigured)}})
	}
	for _, t := range p.ServiceTiers {
		if t.Mode == "fast" {
			v.Fast = &priceView{Input: t.Prompt, Output: t.Completion, CacheRead: optional(t.CacheRead, t.CacheReadConfigured), CacheWrite: optional(t.CacheCreation, t.CacheCreationConfigured)}
		}
	}
	return v
}

type modelsDevView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
	Context     int64    `json:"context,omitempty"`
	Input       int64    `json:"input,omitempty"`
	Output      int64    `json:"output,omitempty"`
	InputKinds  []string `json:"input_modalities,omitempty"`
	OutputKinds []string `json:"output_modalities,omitempty"`
	Reasoning   bool     `json:"reasoning"`
	ToolCall    bool     `json:"tool_call"`
	ReleaseDate string   `json:"release_date,omitempty"`
}

type modelPriceRow struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Price is what the usage record values this model with; Source names it.
	Price  *priceView `json:"price"`
	Source string     `json:"source"`
	// Catalog is models.dev's current price when it differs from Price.
	Catalog   *priceView     `json:"catalog_price,omitempty"`
	ModelsDev *modelsDevView `json:"models_dev,omitempty"`
}

type modelPricesView struct {
	Revision int64                    `json:"revision"`
	Editable bool                     `json:"editable"`
	Sync     historyreports.PriceSync `json:"sync"`
	Source   string                   `json:"sync_source"`
	Models   []modelPriceRow          `json:"models"`
}

type manualPriceEdit struct {
	Revision   int64    `json:"revision"`
	Model      string   `json:"model"`
	Clear      bool     `json:"clear,omitempty"`
	Input      float64  `json:"input,omitempty"`
	Output     float64  `json:"output,omitempty"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
}

func (p *Plugin) modelPrices(ctx context.Context) (historyreports.ModelPrices, historyreports.PriceSync, bool, error) {
	settings, err := p.readUsageSettings(ctx)
	if err != nil {
		return historyreports.ModelPrices{}, historyreports.PriceSync{}, false, err
	}
	return settings.prices, settings.sync, settings.ok, nil
}

func (p *Plugin) modelsAPI(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	op := strings.TrimPrefix(req.Path, modelsRoot)
	trimmed := bytes.TrimSpace(req.Body)
	if len(req.Body) > 256<<10 || len(req.Query) != 0 || len(trimmed) == 0 || trimmed[0] != '{' {
		return reportError(historyreports.ErrInput)
	}
	ctx, cancel := context.WithTimeout(context.Background(), priceSyncWindow)
	defer cancel()
	p.mu.Lock()
	reader := p.reports
	p.mu.Unlock()
	switch {
	case op == "prices/refresh" && req.Method == "POST":
		var empty struct{}
		if decodeStrictReport(req.Body, &empty) != nil {
			return reportError(historyreports.ErrInput)
		}
		state, err := p.syncPrices(ctx)
		if errors.Is(err, errUsageExclusionsUnavailable) {
			return resource(409, map[string]string{"error": "Prices need Pacer's usage record; it is not configured."})
		}
		state.Catalog = nil
		return resource(200, map[string]any{"sync": state})
	case op == "prices" && req.Method == "POST":
		var q modelPriceQuery
		if decodeStrictReport(req.Body, &q) != nil || len(q.Models) > 5000 {
			return reportError(historyreports.ErrInput)
		}
		prices, state, editable, err := p.modelPrices(ctx)
		if err != nil {
			return reportError(err)
		}
		var imported historyreports.ForwardComparisonBook
		if reader != nil {
			if _, imported, err = reader.Imported(ctx); err != nil {
				return reportError(err)
			}
		}
		out := modelPricesView{Revision: prices.Revision, Editable: editable, Sync: state, Source: historyreports.ModelsDevURL, Models: []modelPriceRow{}}
		out.Sync.Catalog = nil
		for _, m := range q.Models {
			if m.Model == "" || len(m.Model) > 256 || len(m.Provider) > 64 {
				return reportError(historyreports.ErrInput)
			}
			row := modelPriceRow{Provider: m.Provider, Model: m.Model}
			if price, source, ok := historyreports.ModelPriceFor(imported, prices, m.Provider, m.Model); ok {
				v := viewPrice(price)
				row.Price, row.Source = &v, source
			}
			if id, md, ok := prices.Catalog.Match(m.Provider, m.Model); ok {
				view := &modelsDevView{ID: id, Name: md.Name, Reasoning: md.Reasoning, ToolCall: md.ToolCall, ReleaseDate: md.ReleaseDate}
				if md.Limit != nil {
					view.Context, view.Input, view.Output = md.Limit.Context, md.Limit.Input, md.Limit.Output
				}
				if md.Modalities != nil {
					view.InputKinds, view.OutputKinds = md.Modalities.Input, md.Modalities.Output
				}
				row.ModelsDev = view
				if row.Source != historyreports.PriceModelsDev {
					if cp, ok := prices.Catalog.Price(m.Provider, m.Model, 0); ok {
						v := viewPrice(cp)
						if row.Price == nil || !samePrice(*row.Price, v) {
							row.Catalog = &v
						}
					}
				}
			}
			out.Models = append(out.Models, row)
		}
		return boundedReportResponse(out)
	case op == "prices" && req.Method == "PATCH":
		var e manualPriceEdit
		if decodeStrictReport(req.Body, &e) != nil || e.Model == "" || len(e.Model) > 256 {
			return reportError(historyreports.ErrInput)
		}
		price := historyreports.ForwardModelPrice{Prompt: e.Input, Completion: e.Output, PromptConfigured: true, CompletionConfigured: true, Source: historyreports.PriceManual}
		if e.CacheRead != nil {
			price.CacheRead, price.Cache, price.CacheReadConfigured = *e.CacheRead, *e.CacheRead, true
		}
		if e.CacheWrite != nil {
			price.CacheCreation, price.CacheCreationConfigured = *e.CacheWrite, true
		}
		if !e.Clear && !historyreports.ValidManualPrice(price) {
			return reportError(historyreports.ErrInput)
		}
		var out historyreports.ModelPrices
		err := p.withForwardStore(ctx, func(c context.Context, s *historyreports.ForwardStore) error {
			var err error
			out, err = s.UpdateModelPrices(c, e.Revision, func(m *historyreports.ModelPrices) error {
				if m.ManualAtMS == nil {
					m.ManualAtMS = map[string]int64{}
				}
				if e.Clear {
					if _, ok := m.Manual[e.Model]; !ok {
						return historyreports.ErrInput
					}
					delete(m.Manual, e.Model)
					delete(m.ManualAtMS, e.Model)
					return nil
				}
				m.Manual[e.Model] = price
				m.ManualAtMS[e.Model] = p.now().UnixMilli()
				return nil
			})
			return err
		})
		if err != nil {
			return editError(err, "Prices")
		}
		p.wakeUsageLive()
		return resource(200, map[string]any{"revision": out.Revision})
	}
	return resource(404, map[string]string{"error": "not_found"})
}

func samePrice(a, b priceView) bool {
	eq := func(x, y *float64) bool {
		return x == nil && y == nil || x != nil && y != nil && math.Abs(*x-*y) < 1e-12
	}
	return math.Abs(a.Input-b.Input) < 1e-12 && math.Abs(a.Output-b.Output) < 1e-12 && eq(a.CacheRead, b.CacheRead) && eq(a.CacheWrite, b.CacheWrite)
}
