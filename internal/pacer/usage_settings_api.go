package pacer

// Usage page settings routes: the record's accounts and logins, account links
// (re-logins), profile edits and the model price list. Accounts and logins are
// named by opaque references (historyreports.AccountRef); profiles by their
// existing "profile:<sha256>" reference.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"strings"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type usageMembership struct {
	Account string `json:"account"` // account reference
	FromMS  int64  `json:"from_ms"`
	ToMS    int64  `json:"to_ms,omitempty"`
}

type usageProfile struct {
	Reference string            `json:"reference"`
	Name      string            `json:"name"`
	Provider  string            `json:"provider"`
	Archived  bool              `json:"archived"`
	Accounts  []usageMembership `json:"accounts"`
}

type usageAccountsView struct {
	Accounts         []historyreports.UsageAccount `json:"accounts"`
	Profiles         []usageProfile                `json:"profiles"`
	LinksRevision    int64                         `json:"links_revision"`
	ProfilesRevision int64                         `json:"profiles_revision"`
	Editable         bool                          `json:"editable"`
	Building         bool                          `json:"building"`
}

// usageIndex resolves the page's references for one request.
type usageIndex struct {
	accounts []historyreports.UsageAccount
	byRef    map[string]historyreports.UsageAccount // account reference
	login    map[string]historyreports.UsageLogin   // login reference
	owner    map[string]historyreports.UsageAccount // login ID -> account
}

func (p *Plugin) usageIndex() usageIndex {
	p.mu.Lock()
	record := p.record
	p.mu.Unlock()
	ix := usageIndex{byRef: map[string]historyreports.UsageAccount{}, login: map[string]historyreports.UsageLogin{}, owner: map[string]historyreports.UsageAccount{}}
	if record == nil {
		return ix
	}
	ix.accounts = record.Accounts()
	for _, a := range ix.accounts {
		ix.byRef[a.Ref] = a
		for _, l := range a.Logins {
			ix.login[l.Ref] = l
			ix.owner[l.ID] = a
		}
	}
	return ix
}

func (ix usageIndex) known(id string) (string, bool) {
	a, ok := ix.owner[id]
	return a.Provider, ok
}

func (ix usageIndex) members(account string) []string {
	a, ok := ix.byRef[account]
	if !ok {
		return nil
	}
	out := []string{}
	for _, l := range a.Logins {
		out = append(out, l.ID)
	}
	return out
}

func profileReference(id string) string { return "profile:" + historyreportsHash(id) }

func (p *Plugin) usageAccountsView(ctx context.Context) (usageAccountsView, error) {
	ix := p.usageIndex()
	out := usageAccountsView{Accounts: ix.accounts, Profiles: []usageProfile{}}
	p.mu.Lock()
	reader := p.reports
	p.mu.Unlock()
	if reader == nil {
		return out, historyreports.ErrNotPrepared
	}
	settings, err := p.readUsageSettings(ctx)
	if err != nil {
		return out, err
	}
	imported, _, err := reader.Imported(ctx)
	if err != nil {
		return out, err
	}
	out.Editable = settings.ok && len(ix.accounts) > 0
	out.Building = len(ix.accounts) == 0
	out.LinksRevision, out.ProfilesRevision = settings.links.Revision, settings.profiles.Revision
	config := historyreports.EffectiveProfiles(imported, settings.profiles)
	for _, prof := range config.Profiles {
		up := usageProfile{Reference: profileReference(prof.ID), Name: prof.Name, Provider: prof.Provider, Archived: prof.Archived, Accounts: []usageMembership{}}
		for _, m := range historyreports.ExactProfileMemberships(config, prof) {
			a, ok := ix.owner[m.Account]
			if !ok {
				continue
			}
			up.Accounts = append(up.Accounts, usageMembership{Account: a.Ref, FromMS: m.Start, ToMS: m.End})
		}
		sort.SliceStable(up.Accounts, func(i, j int) bool {
			if up.Accounts[i].Account != up.Accounts[j].Account {
				return up.Accounts[i].Account < up.Accounts[j].Account
			}
			return up.Accounts[i].FromMS < up.Accounts[j].FromMS
		})
		up.Accounts = mergeUsageMemberships(up.Accounts)
		out.Profiles = append(out.Profiles, up)
	}
	return out, nil
}

// Several logins of one account appear once, with their combined intervals.
func mergeUsageMemberships(in []usageMembership) []usageMembership {
	out := []usageMembership{}
	for _, m := range in {
		if n := len(out); n > 0 && out[n-1].Account == m.Account && (out[n-1].ToMS == 0 || m.FromMS <= out[n-1].ToMS) {
			if out[n-1].ToMS != 0 && (m.ToMS == 0 || m.ToMS > out[n-1].ToMS) {
				out[n-1].ToMS = m.ToMS
			}
			continue
		}
		out = append(out, m)
	}
	return out
}

type usageLinkEdit struct {
	Revision int64  `json:"revision"`
	Login    string `json:"login"`
	SameAs   string `json:"same_as,omitempty"`
	Linked   bool   `json:"linked"`
	Note     string `json:"note,omitempty"`
}

type usageProfileEdit struct {
	Revision int64  `json:"revision"`
	Op       string `json:"op"`
	Profile  string `json:"profile,omitempty"`
	Name     string `json:"name,omitempty"`
	Provider string `json:"provider,omitempty"`
	Account  string `json:"account,omitempty"`
	FromMS   int64  `json:"from_ms,omitempty"`
	AtMS     int64  `json:"at_ms,omitempty"`
}

func newProfileID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "pacer-" + hex.EncodeToString(b)
}

func editError(err error, what string) pluginapi.ManagementResponse {
	switch {
	case errors.Is(err, errUsageExclusionsUnavailable):
		return resource(409, map[string]string{"error": what + " need Pacer's usage store; it is not configured."})
	case errors.Is(err, historyreports.ErrForwardConflict):
		return resource(409, map[string]string{"error": "Something changed meanwhile. Reload and try again."})
	case errors.Is(err, historyreports.ErrInput):
		return resource(400, map[string]string{"error": "That change is not possible: check the account, profile and dates."})
	default:
		return resource(503, map[string]string{"error": "The change could not be saved."})
	}
}

func (p *Plugin) usageSettingsAPI(ctx context.Context, op string, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	switch {
	case op == "accounts" && req.Method == "POST":
		var empty struct{}
		if decodeStrictReport(req.Body, &empty) != nil {
			return reportError(historyreports.ErrInput)
		}
		view, err := p.usageAccountsView(ctx)
		if err != nil {
			return reportError(err)
		}
		return boundedReportResponse(view)
	case op == "links" && req.Method == "PATCH":
		var e usageLinkEdit
		if decodeStrictReport(req.Body, &e) != nil {
			return reportError(historyreports.ErrInput)
		}
		ix := p.usageIndex()
		login, ok := ix.login[e.Login]
		if !ok {
			return editError(historyreports.ErrInput, "")
		}
		sameAs := ""
		if e.Linked {
			target, ok := ix.login[e.SameAs]
			if !ok {
				// An account reference names the account's first login.
				a, found := ix.byRef[e.SameAs]
				if !found || len(a.Logins) == 0 {
					return editError(historyreports.ErrInput, "")
				}
				target = a.Logins[0]
				for _, l := range a.Logins {
					if l.ID == a.ID {
						target = l
					}
				}
			}
			sameAs = target.ID
		}
		var out historyreports.AccountLinks
		err := p.withForwardStore(ctx, func(c context.Context, s *historyreports.ForwardStore) error {
			var e2 error
			out, e2 = s.SetAccountLink(c, e.Revision, login.ID, sameAs, e.Linked, strings.TrimSpace(e.Note), p.now(), ix.known)
			return e2
		})
		if err != nil {
			return editError(err, "Account links")
		}
		p.wakeUsageLive()
		return resource(200, map[string]any{"revision": out.Revision})
	case op == "profile" && req.Method == "PATCH":
		var e usageProfileEdit
		if decodeStrictReport(req.Body, &e) != nil {
			return reportError(historyreports.ErrInput)
		}
		return p.editUsageProfile(ctx, e)
	}
	return resource(404, map[string]string{"error": "not_found"})
}

func (p *Plugin) editUsageProfile(ctx context.Context, e usageProfileEdit) pluginapi.ManagementResponse {
	p.mu.Lock()
	reader := p.reports
	p.mu.Unlock()
	if reader == nil {
		return reportError(historyreports.ErrNotPrepared)
	}
	ix := p.usageIndex()
	imported, _, err := reader.Imported(ctx)
	if err != nil {
		return reportError(err)
	}
	var revision int64
	err = p.withForwardStore(ctx, func(c context.Context, s *historyreports.ForwardStore) error {
		forward, err := s.LatestProfiles(c)
		if err != nil {
			return err
		}
		if forward.Revision != e.Revision {
			return historyreports.ErrForwardConflict
		}
		config := historyreports.EffectiveProfiles(imported, forward)
		profileID := ""
		provider := ""
		for _, prof := range config.Profiles {
			if profileReference(prof.ID) == e.Profile {
				profileID, provider = prof.ID, prof.Provider
			}
		}
		if e.Op != "create" && profileID == "" {
			return historyreports.ErrInput
		}
		members := ix.members(e.Account)
		if e.Account != "" {
			if a := ix.byRef[e.Account]; len(members) == 0 || provider != "" && a.Provider != provider {
				return historyreports.ErrInput
			}
		}
		edit := historyreports.ProfileEdit{Op: e.Op, Profile: profileID, Name: e.Name, Provider: e.Provider, FromMS: e.FromMS, AtMS: e.AtMS}
		prof, err := historyreports.ApplyProfileEdit(config, edit, members, newProfileID())
		if err != nil {
			return err
		}
		revision, err = s.EditProfilesKnown(c, forward.Revision, historyreports.ForwardProfileEdit{Profile: prof}, ix.known)
		return err
	})
	if err != nil {
		return editError(err, "Profile edits")
	}
	p.wakeUsageLive()
	return resource(200, map[string]any{"revision": revision})
}

// historyreportsHash is the profile reference digest (sha256 hex).
func historyreportsHash(s string) string { return historyreports.Hash(s) }
