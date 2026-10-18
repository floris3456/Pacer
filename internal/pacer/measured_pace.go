package pacer

import (
	"context"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/routing"
)

// Routing's measured inputs come from the usage page (docs/pace-routing.md,
// "Spending before the reset"): the value of 1 % of each account's shared
// 7-day meter (its profile's weekly average, with the page's exclusions) and
// each provider's spend per hour over the last days. They are refreshed with
// the usage record and dropped when that stops.
const (
	demandDays  = 7
	measuredAge = time.Hour
)

type measuredPace struct {
	at     time.Time
	value  map[string]float64 // credential account -> USD per 1 % of its 7-day meter
	demand map[string]float64 // provider -> USD per hour
}

// refreshMeasuredPace reads both from the usage record's facts.
func (p *Plugin) refreshMeasuredPace(ctx context.Context, reader *historyreports.Reader) error {
	exclusions, _, err := p.usageExclusions(ctx)
	if err != nil {
		return err
	}
	ex := exclusions.Map()
	now := p.now().UTC()
	from := now.AddDate(0, 0, -demandDays).Truncate(24 * time.Hour)
	all, err := reader.Compare(ctx, historyreports.CompareQuery{}, ex)
	if err != nil {
		return err
	}
	recent, err := reader.Compare(ctx, historyreports.CompareQuery{From: from.Format("2006-01-02")}, ex)
	if err != nil {
		return err
	}
	view, err := p.usageAccountsView(ctx)
	if err != nil {
		return err
	}
	ix := p.usageIndex()
	p.mu.Lock()
	defer p.mu.Unlock()
	creds := []credential{}
	for _, inventory := range []map[string]credential{p.creds, p.usageCreds} {
		for _, c := range inventory {
			creds = append(creds, c)
		}
	}
	source := p.measurementSource
	p.measured = measure(now, from, all, recent, view.Profiles, creds, func(c credential) (string, bool) {
		owner, ok := ix.owner[historyreports.ForwardAccountRef(source, c.Provider, c.Account)]
		return owner.Ref, ok
	})
	return nil
}

// measure: each credential's value is the weekly ("all") average of the
// profile it belongs to now; a provider's demand is its profiles' recorded
// value since from, per hour.
func measure(now, from time.Time, all, recent historyreports.CompareView, profiles []usageProfile, creds []credential, accountRef func(credential) (string, bool)) measuredPace {
	rates := map[string]float64{}
	for _, prof := range all.Profiles {
		for _, pool := range prof.Pools {
			if r := pool.Average.USDPerPercent; pool.Kind == "weekly" && pool.Scope == "all" && pool.Average.Status == "available" && r != nil && r.Low > 0 {
				rates[prof.Reference] = (r.Low + r.High) / 2
			}
		}
	}
	demand := map[string]float64{}
	hours := now.Sub(from).Hours()
	for _, prof := range recent.Profiles {
		if prof.Provider != "" && hours > 0 {
			demand[prof.Provider] += float64(prof.Selected.ValueLowNanoUSD+prof.Selected.ValueHighNanoUSD) / 2e9 / hours
		}
	}
	value := map[string]float64{}
	ms := now.UnixMilli()
	for _, c := range creds {
		ref, ok := accountRef(c)
		if !ok {
			continue
		}
		for _, prof := range profiles {
			for _, member := range prof.Accounts {
				if !prof.Archived && member.Account == ref && member.FromMS <= ms && (member.ToMS == 0 || member.ToMS > ms) && rates[prof.Reference] > 0 {
					value[c.Account] = rates[prof.Reference]
				}
			}
		}
	}
	return measuredPace{at: now, value: value, demand: demand}
}

// routingConfigLocked is the page settings' routing configuration with the
// provider's measured spend, while the measurements are current.
func (p *Plugin) routingConfigLocked(provider string, now time.Time) routing.Config {
	c := p.settings.routing()
	if p.measuredCurrentLocked(now) {
		c.Demand = p.measured.demand[provider]
	}
	return c
}

func (p *Plugin) measuredValueLocked(account string, now time.Time) float64 {
	if !p.measuredCurrentLocked(now) {
		return 0
	}
	return p.measured.value[account]
}

// measuredDemandLocked is each provider's measured spend per hour, for the page.
func (p *Plugin) measuredDemandLocked(now time.Time) map[string]float64 {
	if !p.measuredCurrentLocked(now) {
		return map[string]float64{}
	}
	return p.measured.demand
}

func (p *Plugin) measuredCurrentLocked(now time.Time) bool {
	return !p.measured.at.IsZero() && now.Sub(p.measured.at) < measuredAge
}
