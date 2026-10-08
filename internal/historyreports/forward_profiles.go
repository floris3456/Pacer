package historyreports

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
)

// Only the forward durable encoding overrides the legacy DTO's omitempty.
// nil means legacy ownership; [] means an explicitly empty selection. Existing
// profile objects keep their original bytes/digests and still decode normally.
type forwardStoredProfiles struct{ ForwardProfileConfig }

func (c forwardStoredProfiles) MarshalJSON() ([]byte, error) {
	type profile struct {
		ForwardProfile
		Memberships *[]ForwardMembership `json:"memberships,omitempty"`
	}
	var profiles []profile
	if c.Profiles != nil {
		profiles = make([]profile, len(c.Profiles))
	}
	for i, p := range c.Profiles {
		profiles[i].ForwardProfile = p
		if p.Memberships != nil {
			members := p.Memberships
			profiles[i].Memberships = &members
		}
	}
	return json.Marshal(struct {
		ForwardProfileConfig
		Profiles []profile `json:"profiles"`
	}{c.ForwardProfileConfig, profiles})
}

func (s *ForwardStore) RegisterForwardBook(ctx context.Context, b ForwardBook) (d string, err error) {
	if b.ID == "" || len(b.ID) > 256 || b.Book.Policy != "profile-api-value-v3" || len(b.Book.Prices) > 10000 || len(b.Qualifications) > 10000 {
		return "", ErrInput
	}
	if _, err = json.Marshal(b); err != nil {
		return "", ErrInput
	}
	for _, p := range b.Book.Prices {
		if p.RawJSON != "" {
			return "", ErrInput
		}
		rates := []float64{p.Prompt, p.Completion, p.Cache, p.CacheRead, p.CacheCreation}
		last := int64(-1)
		for _, tier := range p.ContextTiers {
			if tier.ThresholdTokens <= last {
				return "", ErrInput
			}
			last = tier.ThresholdTokens
			rates = append(rates, tier.Prompt, tier.Completion, tier.Cache, tier.CacheRead, tier.CacheCreation)
		}
		seen := map[string]bool{}
		for _, tier := range p.ServiceTiers {
			local := map[string]bool{}
			for _, key := range []string{tier.Mode, tier.ServiceTier} {
				if key == "" {
					continue
				}
				key = strings.ToLower(strings.TrimSpace(key))
				if local[key] {
					continue
				}
				local[key] = true
				if seen[key] {
					return "", ErrInput
				}
				seen[key] = true
			}
			rates = append(rates, tier.Prompt, tier.Completion, tier.Cache, tier.CacheRead, tier.CacheCreation)
		}
		for _, rate := range rates {
			if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
				return "", ErrInput
			}
		}
	}
	seen := map[ForwardPriceQualification]bool{}
	for _, q := range b.Qualifications {
		if q.SourceID == "" || q.Provider == "" || q.Model == "" || q.AccountID == "" || seen[q] {
			return "", ErrInput
		}
		seen[q] = true
	}
	err = s.write(ctx, func(tx *sql.Tx) error { var e error; d, e = forwardObject(tx, ctx, "book", b.ID, b); return e })
	return
}

func (s *ForwardStore) ProfilesAt(ctx context.Context, revision int64) (c ForwardProfileConfig, d string, err error) {
	var body []byte
	err = s.reader.QueryRowContext(ctx, `SELECT digest,body FROM forward_objects WHERE kind='profiles' AND name=?`, strconv.FormatInt(revision, 10)).Scan(&d, &body)
	if err != nil {
		return
	}
	if hash(string(body)) != d {
		return c, d, ErrForwardConflict
	}
	err = json.Unmarshal(body, &c)
	return
}

// SeedForwardProfiles preserves legacy bindings/ordering and original revision
// inside the copied configuration, assigning one local immutable branch revision.
func (s *ForwardStore) SeedForwardProfiles(ctx context.Context, c ForwardProfileConfig) (int64, error) {
	return s.seedForwardProfiles(ctx, c, nil)
}

func (s *ForwardStore) seedForwardProfiles(ctx context.Context, c ForwardProfileConfig, seed *ForwardProfileSeed) (int64, error) {
	if len(c.Profiles) > 1000 || len(c.Bindings) > 10000 {
		return 0, ErrLimit
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		var current int64
		if e := tx.QueryRowContext(ctx, `SELECT profile_revision FROM forward_meta WHERE id=1`).Scan(&current); e != nil {
			return e
		}
		if current != 0 {
			return ErrForwardConflict
		}
		if _, e := forwardObject(tx, ctx, "profiles", "1", forwardStoredProfiles{c}); e != nil {
			return e
		}
		if seed != nil {
			if _, e := forwardObject(tx, ctx, "profile_seed", "1", *seed); e != nil {
				return e
			}
		}
		_, e := tx.ExecContext(ctx, `UPDATE forward_meta SET profile_revision=1 WHERE id=1`)
		return e
	})
	return 1, err
}

type ForwardProfileEdit struct {
	Profile   ForwardProfile
	ArchiveID string
}

func (s *ForwardStore) EditProfiles(ctx context.Context, expected int64, edit ForwardProfileEdit) (revision int64, err error) {
	if expected < 0 || (edit.ArchiveID == "") == (edit.Profile.ID == "") {
		return 0, ErrInput
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		var current int64
		if e := tx.QueryRowContext(ctx, `SELECT profile_revision FROM forward_meta WHERE id=1`).Scan(&current); e != nil {
			return e
		}
		if current != expected {
			return ErrForwardConflict
		}
		if current == math.MaxInt64 {
			return ErrLimit
		}
		var raw []byte
		if e := tx.QueryRowContext(ctx, `SELECT body FROM forward_objects WHERE kind='profiles' AND name=?`, strconv.FormatInt(current, 10)).Scan(&raw); e != nil {
			return e
		}
		var c ForwardProfileConfig
		if json.Unmarshal(raw, &c) != nil {
			return ErrSource
		}
		if edit.ArchiveID != "" {
			found := false
			for i := range c.Profiles {
				if c.Profiles[i].ID == edit.ArchiveID {
					c.Profiles[i].Archived = true
					found = true
				}
			}
			if !found {
				return ErrInput
			}
		} else {
			p := edit.Profile
			if p.ID == "unassigned" || p.ID == "all" || len(p.ID) > 256 || p.Provider == "" || p.Memberships == nil || len(p.Memberships) > 1000 || len(p.Accounts) > 0 || p.ActiveAccount != "" {
				return ErrInput
			}
			for i, m := range p.Memberships {
				if m.Account == "" || m.Start < 0 || m.End < 0 || m.End > 0 && m.End <= m.Start {
					return ErrInput
				}
				var n int
				if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM forward_base_events WHERE account=? AND provider=? UNION ALL SELECT 1 FROM forward_native WHERE account=? AND provider=?)`, m.Account, p.Provider, m.Account, p.Provider).Scan(&n); e != nil {
					return e
				}
				if n == 0 {
					return ErrInput
				}
				for _, prev := range p.Memberships[:i] {
					if prev.Account == m.Account && (prev.End == 0 || m.Start < prev.End) && (m.End == 0 || prev.Start < m.End) {
						return ErrInput
					}
				}
			}
			found := false
			for i := range c.Profiles {
				if c.Profiles[i].ID == p.ID {
					if c.Profiles[i].Provider != p.Provider {
						return ErrForwardConflict
					}
					c.Profiles[i] = p
					found = true
				}
			}
			if !found {
				c.Profiles = append(c.Profiles, p)
			}
			if len(c.Profiles) > 1000 {
				return ErrLimit
			}
		}
		revision = current + 1
		c.Revision = revision
		if _, e := forwardObject(tx, ctx, "profiles", strconv.FormatInt(revision, 10), forwardStoredProfiles{c}); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, `UPDATE forward_meta SET profile_revision=? WHERE id=1`, revision)
		return e
	})
	return
}

func originForBaseline(o ForwardOrigins, e ForwardEvent) (*ForwardOriginCertificate, error) {
	var out *ForwardOriginCertificate
	for i := range o.Certificates {
		c := &o.Certificates[i]
		if c.LegacyAccount == e.Event.Account && c.Provider == e.Event.Provider && e.Event.Started >= c.StartMS && (c.EndMS == 0 || e.Event.Started < c.EndMS) {
			if out != nil {
				return nil, ErrForwardConflict
			}
			out = c
		}
	}
	return out, nil
}
func originForNative(o ForwardOrigins, e ForwardEvent) (*ForwardOriginCertificate, error) {
	var out *ForwardOriginCertificate
	for i := range o.Certificates {
		c := &o.Certificates[i]
		if c.SourceID == e.SourceID && c.NativeAccount == e.Event.Account && c.NativeCredentialRef == e.Attribution.CredentialRef && c.Provider == e.Event.Provider && e.StartedAt.UnixMilli() >= c.StartMS && (c.EndMS == 0 || e.StartedAt.UnixMilli() < c.EndMS) {
			if out != nil {
				return nil, ErrForwardConflict
			}
			out = c
		}
	}
	return out, nil
}

// RegisterForwardOrigins builds an indexed crosswalk in bounded, resumable
// batches. Certificates are trusted evidence inputs, never management overrides.
func (s *ForwardStore) RegisterForwardOrigins(ctx context.Context, o ForwardOrigins) (string, error) {
	if !validHash(o.BaseDigest) {
		return "", ErrInput
	}
	if len(o.Certificates) > 1000 {
		return "", ErrLimit
	}
	for i, c := range o.Certificates {
		if c.LegacyAccount == "" || c.Provider == "" || c.SourceID == "" || c.NativeAccount == "" || c.EvidenceRef == "" || c.ProducerVersion == "" || c.ParserVersion == "" || c.StartMS < 0 || c.EndMS < 0 || c.EndMS > 0 && c.EndMS <= c.StartMS || c.StartMS > math.MaxInt64/int64(time.Millisecond) || c.EndMS > math.MaxInt64/int64(time.Millisecond) {
			return "", ErrInput
		}
		for _, previous := range o.Certificates[:i] {
			if previous.Provider != c.Provider || previous.EndMS > 0 && c.StartMS >= previous.EndMS || c.EndMS > 0 && previous.StartMS >= c.EndMS {
				continue
			}
			if previous.LegacyAccount == c.LegacyAccount || previous.SourceID == c.SourceID && previous.NativeAccount == c.NativeAccount && previous.NativeCredentialRef == c.NativeCredentialRef {
				return "", ErrForwardConflict
			}
		}
	}
	d := forwardDigest(o)
	err := s.write(ctx, func(tx *sql.Tx) error {
		var complete bool
		if e := tx.QueryRowContext(ctx, `SELECT complete FROM forward_base_meta WHERE id=1 AND digest=?`, o.BaseDigest).Scan(&complete); e != nil {
			return e
		}
		if !complete {
			return ErrForwardPending
		}
		if _, e := forwardObject(tx, ctx, "origins", d, o); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, `INSERT INTO forward_origin_state(digest) VALUES(?) ON CONFLICT DO NOTHING`, d)
		return e
	})
	if err != nil {
		return "", err
	}
	for {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		var after string
		var ready bool
		if err = s.reader.QueryRowContext(ctx, `SELECT after_id,ready FROM forward_origin_state WHERE digest=?`, d).Scan(&after, &ready); err != nil {
			return "", err
		}
		if ready {
			return d, nil
		}
		rows, e := s.reader.QueryContext(ctx, `SELECT body FROM forward_base_events WHERE id>? ORDER BY id LIMIT 250`, after)
		if e != nil {
			return "", e
		}
		page := []ForwardEvent{}
		used := 0
		for rows.Next() {
			var raw []byte
			if e = rows.Scan(&raw); e != nil {
				break
			}
			if used+len(raw) > 2<<20 {
				if len(page) == 0 {
					e = ErrLimit
				}
				break
			}
			var f ForwardEvent
			if e = json.Unmarshal(raw, &f); e != nil {
				break
			}
			page = append(page, f)
			used += len(raw)
		}
		rowErr := rows.Err()
		rows.Close()
		if e != nil {
			return "", e
		}
		if rowErr != nil {
			return "", rowErr
		}
		err = s.write(ctx, func(tx *sql.Tx) error {
			var current string
			if e := tx.QueryRowContext(ctx, `SELECT after_id FROM forward_origin_state WHERE digest=?`, d).Scan(&current); e != nil {
				return e
			}
			if current != after {
				return ErrForwardConflict
			}
			for _, f := range page {
				if e := ctx.Err(); e != nil {
					return e
				}
				c, e := originForBaseline(o, f)
				if e != nil {
					return e
				}
				if c != nil && f.AttemptID != "" {
					if _, e = tx.ExecContext(ctx, `INSERT INTO forward_links(origin,baseline,physical,account,provider,credential,start_ns,end_ns) VALUES(?,?,?,?,?,?,?,?)`, d, f.ID, forwardPhysical(c.SourceID, f.AttemptID), ForwardAccountRef(c.SourceID, c.Provider, c.NativeAccount), c.Provider, c.NativeCredentialRef, c.StartMS*int64(time.Millisecond), c.EndMS*int64(time.Millisecond)); e != nil {
						return ErrForwardConflict
					}
				}
				after = f.ID
			}
			var missing int64
			if len(page) == 0 {
				if e := tx.QueryRowContext(ctx, `SELECT event_count-(SELECT count(*) FROM forward_links WHERE origin=?) FROM forward_base_meta WHERE id=1`, d).Scan(&missing); e != nil {
					return e
				}
			}
			_, e := tx.ExecContext(ctx, `UPDATE forward_origin_state SET after_id=?,missing_count=?,ready=? WHERE digest=?`, after, missing, len(page) == 0, d)
			return e
		})
		if err != nil {
			return "", err
		}
	}
}

func (s *ForwardStore) loadForwardBase(ctx context.Context, d string) (ForwardBase, error) {
	var b ForwardBase
	if d == "" {
		return b, nil
	}
	var body []byte
	var complete bool
	err := s.reader.QueryRowContext(ctx, `SELECT body,complete FROM forward_base_meta WHERE digest=?`, d).Scan(&body, &complete)
	if err != nil {
		return b, err
	}
	if !complete {
		return b, ErrForwardPending
	}
	if hash(string(body)) != d {
		return b, ErrForwardConflict
	}
	return b, json.Unmarshal(body, &b)
}

// Keep the original predicate, including first-binding ownership and overlapping
// selection profiles. Archived profiles retain their recorded memberships.
func forwardMemberships(c ForwardProfileConfig, account, provider string, at int64) []string {
	out := []string{}
	for _, p := range c.Profiles {
		if p.Provider == provider && cpamp.ForwardContains(c, p, account, at) {
			out = append(out, p.ID)
		}
	}
	if len(out) == 0 {
		out = append(out, "unassigned")
	}
	return out
}
