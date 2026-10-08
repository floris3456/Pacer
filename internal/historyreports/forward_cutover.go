package historyreports

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"
)

// Forward continuity (forward-continuity-contract.md §1-§3). Every object below
// is written once per store by offline preparation; a new cutover is a new store.

func (s *ForwardStore) objectNamed(ctx context.Context, kind, name string, v any) error {
	var d string
	var body []byte
	if err := s.reader.QueryRowContext(ctx, `SELECT digest,body FROM forward_objects WHERE kind=? AND name=?`, kind, name).Scan(&d, &body); err != nil {
		return err
	}
	if hash(string(body)) != d {
		return ErrForwardConflict
	}
	return json.Unmarshal(body, v)
}

// SeedFrozenForwardProfiles assigns branch revision 1 from the frozen artifact's
// own profile configuration at H and records its provenance. The frozen
// profiles are read-only afterwards; EditProfiles refuses to change them.
func (s *ForwardStore) SeedFrozenForwardProfiles(ctx context.Context, r *Reader, baseDigest string) (int64, ForwardProfileSeed, error) {
	if r == nil || !validHash(baseDigest) {
		return 0, ForwardProfileSeed{}, ErrInput
	}
	base, err := s.loadForwardBase(ctx, baseDigest)
	if err != nil {
		return 0, ForwardProfileSeed{}, err
	}
	if forwardDigest(base.Boundary) != forwardDigest(r.meta.Boundary) || forwardDigest(base.Pin) != forwardDigest(r.meta.Pin) {
		return 0, ForwardProfileSeed{}, ErrForwardConflict
	}
	_, c, err := r.ForwardMetadata(ctx)
	if err != nil {
		return 0, ForwardProfileSeed{}, err
	}
	seed := ForwardProfileSeed{BaseDigest: baseDigest, ArtifactDigest: base.ArtifactDigest, FrozenRevision: r.meta.Pin.Revision, ConfigDigest: forwardDigest(c)}
	rev, err := s.seedForwardProfiles(ctx, c, &seed)
	return rev, seed, err
}

// PrepareForwardCutover records H (the base boundary) and N0 (the latest origin
// certificate StartMS). N0 > H refuses unless the coverage gap is declared; a
// declared gap is recorded, never filled.
func (s *ForwardStore) PrepareForwardCutover(ctx context.Context, originDigest string, declareCoverageGap bool) (ForwardCutover, error) {
	if !validHash(originDigest) {
		return ForwardCutover{}, ErrInput
	}
	var o ForwardOrigins
	if err := s.object(ctx, "origins", originDigest, &o); err != nil {
		return ForwardCutover{}, err
	}
	if !validHash(o.BaseDigest) {
		return ForwardCutover{}, ErrSource
	}
	base, err := s.loadForwardBase(ctx, o.BaseDigest)
	if err != nil {
		return ForwardCutover{}, err
	}
	var ready bool
	if err = s.reader.QueryRowContext(ctx, `SELECT ready FROM forward_origin_state WHERE digest=?`, originDigest).Scan(&ready); err != nil {
		return ForwardCutover{}, err
	}
	if !ready {
		return ForwardCutover{}, ErrForwardPending
	}
	cut, err := forwardCutoverFor(base, o, declareCoverageGap)
	if err != nil {
		return ForwardCutover{}, err
	}
	rec := forwardCutoverRecord{BaseDigest: o.BaseDigest, OriginDigest: originDigest, Cutover: cut}
	err = s.write(ctx, func(tx *sql.Tx) error { _, e := forwardObject(tx, ctx, "cutover", "1", rec); return e })
	return cut, err
}

func forwardCutoverFor(base ForwardBase, o ForwardOrigins, declared bool) (ForwardCutover, error) {
	b := base.Boundary
	c := ForwardCutover{HMS: b.HMS, EventsThrough: b.EventsThrough, ObservationsThrough: b.ObservationsThrough}
	if len(o.Certificates) == 0 || b.HMS <= 0 || b.HMS > math.MaxInt64/int64(time.Millisecond) {
		return c, ErrInput
	}
	for _, cert := range o.Certificates {
		c.N0MS = max(c.N0MS, cert.StartMS)
	}
	if c.N0MS <= c.HMS {
		if declared {
			return c, ErrInput
		}
		return c, nil
	}
	if !declared {
		return c, ErrForwardCoverageGap
	}
	accounts := []string{}
	seen := map[string]bool{}
	for _, cert := range o.Certificates {
		if cert.StartMS > c.HMS && !seen[cert.LegacyAccount] {
			seen[cert.LegacyAccount] = true
			accounts = append(accounts, cert.LegacyAccount)
		}
	}
	sort.Strings(accounts)
	c.CoverageGap = &ForwardCoverageGap{FromMS: c.HMS, ToMS: c.N0MS, Accounts: accounts}
	return c, nil
}

func (s *ForwardStore) preparedCutover(ctx context.Context) (*forwardCutoverRecord, error) {
	var rec forwardCutoverRecord
	err := s.objectNamed(ctx, "cutover", "1", &rec)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// PreparedCutover returns the store's recorded cutover, or nil without one.
func (s *ForwardStore) PreparedCutover(ctx context.Context) (*ForwardCutover, error) {
	rec, err := s.preparedCutover(ctx)
	if rec == nil || err != nil {
		return nil, err
	}
	c := rec.Cutover
	return &c, nil
}

// ForwardSelection is the runtime gate. A store without a base keeps the
// native-only selection. A store with a base is accepted only with a complete
// base, ready origins with no missing baseline links, revision 1 seeded from the
// same frozen artifact, and a recorded H/N0 (N0 ≤ H or a declared coverage gap).
// It is metadata-only and never prepares, imports or edits anything.
func (s *ForwardStore) ForwardSelection(ctx context.Context, bookDigest string) (ForwardSeal, error) {
	var bases int
	if err := s.reader.QueryRowContext(ctx, `SELECT count(*) FROM forward_base_meta`).Scan(&bases); err != nil {
		return ForwardSeal{BookDigest: bookDigest}, ErrSource
	}
	if bases == 0 {
		return s.NativeForwardSelection(ctx, bookDigest)
	}
	q := ForwardSeal{BookDigest: bookDigest}
	if err := s.acceptBaseline(ctx, &q); err != nil {
		return ForwardSeal{BookDigest: bookDigest}, ErrSource
	}
	return q, nil
}

func (s *ForwardStore) acceptBaseline(ctx context.Context, q *ForwardSeal) error {
	var book ForwardBook
	if err := s.object(ctx, "book", q.BookDigest, &book); err != nil {
		return err
	}
	if book.Book.Policy != "profile-api-value-v3" || book.ID == "" {
		return ErrSource
	}
	rec, err := s.preparedCutover(ctx)
	if err != nil || rec == nil {
		return ErrSource
	}
	base, err := s.loadForwardBase(ctx, rec.BaseDigest)
	if err != nil || !validHash(rec.BaseDigest) {
		return ErrSource
	}
	if h := book.Book.Historical; h != nil && (h.At != base.Boundary.HMS || h.SourceCheckpoint != base.Boundary.CheckpointSHA256 || h.EventsThrough != base.Boundary.EventsThrough || h.ObservationsThrough != base.Boundary.ObservationsThrough) {
		return ErrSource
	}
	var ready bool
	var missing int64
	if err = s.reader.QueryRowContext(ctx, `SELECT ready,missing_count FROM forward_origin_state WHERE digest=?`, rec.OriginDigest).Scan(&ready, &missing); err != nil || !ready || missing != 0 {
		return ErrSource
	}
	var o ForwardOrigins
	if err = s.object(ctx, "origins", rec.OriginDigest, &o); err != nil || o.BaseDigest != rec.BaseDigest {
		return ErrSource
	}
	if want, e := forwardCutoverFor(base, o, rec.Cutover.CoverageGap != nil); e != nil || forwardDigest(want) != forwardDigest(rec.Cutover) {
		return ErrSource
	}
	var seed ForwardProfileSeed
	if err = s.objectNamed(ctx, "profile_seed", "1", &seed); err != nil {
		return ErrSource
	}
	seeded, _, err := s.ProfilesAt(ctx, 1)
	if err != nil || seed.BaseDigest != rec.BaseDigest || seed.ArtifactDigest != base.ArtifactDigest || seed.FrozenRevision != base.Pin.Revision || seed.ConfigDigest != forwardDigest(seeded) {
		return ErrSource
	}
	if err = s.reader.QueryRowContext(ctx, `SELECT profile_revision FROM forward_meta WHERE id=1`).Scan(&q.ProfileRevision); err != nil || q.ProfileRevision < 1 {
		return ErrSource
	}
	if _, _, err = s.ProfilesAt(ctx, q.ProfileRevision); err != nil {
		return ErrSource
	}
	q.BaseDigest, q.OriginDigest = rec.BaseDigest, rec.OriginDigest
	return nil
}

// GenerationCutover reads the cutover status compiled into a sealed generation's
// default summary; nil for generations without a cutover.
func (s *ForwardStore) GenerationCutover(ctx context.Context, generation string) (*ForwardCutoverStatus, error) {
	if !validHash(generation) {
		return nil, ErrInput
	}
	var raw []byte
	if err := s.reader.QueryRowContext(ctx, `SELECT summary FROM forward_generations WHERE id=? AND sealed=1`, generation).Scan(&raw); err != nil {
		return nil, err
	}
	var r struct {
		Cutover *ForwardCutoverStatus `json:"cutover"`
	}
	if len(raw) > 8<<20 || json.Unmarshal(raw, &r) != nil {
		return nil, ErrSource
	}
	return r.Cutover, nil
}

// Generation-level counts of what the combined domain excludes by construction.
func (s *ForwardStore) forwardCutoverStatus(ctx context.Context, m forwardManifest) (*ForwardCutoverStatus, error) {
	st := &ForwardCutoverStatus{ForwardCutover: *m.Cutover}
	n0 := min(m.Cutover.N0MS*int64(time.Millisecond), m.At.UnixNano())
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM forward_native n WHERE n.started<? AND n.sequence<=? AND n.revision=(SELECT max(x.revision) FROM forward_native x WHERE x.event_id=n.event_id AND x.sequence<=?)`, n0, m.Head.Through, m.Head.Through).Scan(&st.PreCutoverNative); err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM forward_base_events WHERE started>=?`, m.Cutover.HMS*int64(time.Millisecond)).Scan(&st.PostHBaseline); err != nil {
		return nil, err
	}
	return st, tx.Commit()
}

// Profiles in a frozen-provenance revision 1 are read-only. nil without a seed.
func forwardFrozenProfileIDs(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM forward_objects WHERE kind='profile_seed' AND name='1'`).Scan(&n); err != nil || n == 0 {
		return nil, err
	}
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT body FROM forward_objects WHERE kind='profiles' AND name='1'`).Scan(&raw); err != nil {
		return nil, err
	}
	var c ForwardProfileConfig
	if json.Unmarshal(raw, &c) != nil {
		return nil, ErrSource
	}
	ids := map[string]bool{}
	for _, p := range c.Profiles {
		ids[p.ID] = true
	}
	return ids, nil
}
