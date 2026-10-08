package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Key struct {
	Reference   string   `json:"reference"`
	CurrentHash string   `json:"current_hash"`
	Alias       string   `json:"alias"`
	Lineage     []string `json:"lineage_hashes"`
	Imported    bool     `json:"imported"`
}

// Group is a new display-only multi-client-key group. It is deliberately not a
// CPAMP provider-account profile or an authorization/routing rule.
type Group struct {
	Reference string   `json:"reference"`
	Name      string   `json:"name"`
	Members   []string `json:"key_references"`
}
type Metadata struct {
	Revision int64   `json:"revision"`
	Keys     []Key   `json:"keys"`
	Groups   []Group `json:"groups"`
}

func safeLabel(s string) string {
	lower := strings.ToLower(s)
	if !utf8.ValidString(s) || len([]rune(s)) > 120 || strings.Contains(lower, "sk-") || strings.Contains(lower, "bearer ") || strings.Contains(lower, "cmp_admin_") || strings.Contains(lower, "-----begin") || strings.Contains(lower, "api_key=") || strings.Contains(lower, "access_token") {
		return "[redacted]"
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "[redacted]"
		}
	}
	return s
}
func normalizeMetadata(c *Metadata) {
	sort.Slice(c.Keys, func(i, j int) bool { return c.Keys[i].Reference < c.Keys[j].Reference })
	for i := range c.Keys {
		sort.Strings(c.Keys[i].Lineage)
	}
	sort.Slice(c.Groups, func(i, j int) bool { return c.Groups[i].Reference < c.Groups[j].Reference })
	for i := range c.Groups {
		sort.Strings(c.Groups[i].Members)
	}
}

func metadata(ctx context.Context, tx *sql.Tx) (Metadata, error) {
	c := Metadata{Keys: []Key{}, Groups: []Group{}}
	var pending int
	if tx.QueryRowContext(ctx, `SELECT count(*) FROM history_import WHERE complete=0`).Scan(&pending) != nil {
		return c, ErrStorage
	}
	if pending != 0 {
		return c, ErrPending
	}
	var raw []byte
	e := tx.QueryRowContext(ctx, `SELECT payload FROM history_metadata ORDER BY revision DESC LIMIT 1`).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return c, nil
	}
	if e != nil || json.Unmarshal(raw, &c) != nil {
		return c, ErrStorage
	}
	return c, nil
}

// Keys returns safe management DTOs; it never reads settings, raw_json,
// credentials, auth filenames, source SQL or error strings into an API response.
func (s *Store) Keys(ctx context.Context) (Metadata, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return Metadata{}, ErrStorage
	}
	defer tx.Rollback()
	return metadata(ctx, tx)
}

type Edit struct {
	Revision  int64    `json:"revision"`
	Action    string   `json:"action"` // add-key, alias, rotate-key, put-group, delete-group
	Reference string   `json:"reference"`
	Hash      string   `json:"hash,omitempty"`
	Name      string   `json:"name,omitempty"`
	Members   []string `json:"key_references,omitempty"`
}

// Edit changes display metadata under optimistic revision control. Rotation
// preserves the stable reference and complete prior hash lineage; it does not
// create/revoke a CPA key, authorize a caller, change a quota or move an account.
func (s *Store) Edit(ctx context.Context, e Edit) (Metadata, error) {
	if e.Revision < 0 || safeLabel(e.Name) != e.Name || len(e.Members) > 10000 {
		return Metadata{}, ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Metadata{}, ErrStorage
	}
	defer tx.Rollback()
	c, err := metadata(ctx, tx)
	if err != nil {
		return c, err
	}
	if c.Revision != e.Revision {
		return Metadata{}, ErrConflict
	}
	idx := -1
	for i, k := range c.Keys {
		if k.Reference == e.Reference {
			idx = i
		}
	}
	switch e.Action {
	case "add-key", "rotate-key":
		if !validHash(e.Hash) || e.Action == "rotate-key" && idx < 0 {
			return Metadata{}, ErrInput
		}
		for _, k := range c.Keys {
			for _, h := range k.Lineage {
				if h == e.Hash {
					return Metadata{}, ErrConflict
				}
			}
		}
		if e.Action == "add-key" {
			if e.Reference != "" || len(c.Keys) >= 10000 {
				return Metadata{}, ErrInput
			}
			c.Keys = append(c.Keys, Key{Reference: "key:" + e.Hash, CurrentHash: e.Hash, Alias: e.Name, Lineage: []string{e.Hash}})
		} else {
			c.Keys[idx].Lineage = append(c.Keys[idx].Lineage, e.Hash)
			c.Keys[idx].CurrentHash = e.Hash
		}
	case "alias":
		if idx < 0 {
			return Metadata{}, ErrInput
		}
		c.Keys[idx].Alias = e.Name
	case "put-group":
		if !strings.HasPrefix(e.Reference, "group:") || !validHash(strings.TrimPrefix(e.Reference, "group:")) || strings.TrimSpace(e.Name) == "" {
			return Metadata{}, ErrInput
		}
		seen := map[string]bool{}
		for _, ref := range e.Members {
			exists := false
			for _, k := range c.Keys {
				if k.Reference == ref {
					exists = true
				}
			}
			if !exists || seen[ref] {
				return Metadata{}, ErrInput
			}
			seen[ref] = true
		}
		g := Group{Reference: e.Reference, Name: e.Name, Members: append([]string{}, e.Members...)}
		found := false
		for i := range c.Groups {
			if c.Groups[i].Reference == e.Reference {
				c.Groups[i] = g
				found = true
			}
		}
		if !found {
			if len(c.Groups) >= 1000 {
				return Metadata{}, ErrInput
			}
			c.Groups = append(c.Groups, g)
		}
	case "delete-group":
		found := false
		for i, g := range c.Groups {
			if g.Reference == e.Reference {
				c.Groups = append(c.Groups[:i], c.Groups[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			return Metadata{}, ErrInput
		}
	default:
		return Metadata{}, ErrInput
	}
	// Duplicate nonempty alias names are rejected for native edits, while frozen
	// source aliases remain exact (even when the source itself had collisions).
	if e.Action == "alias" || e.Action == "add-key" {
		target := e.Reference
		if e.Action == "add-key" {
			target = "key:" + e.Hash
		}
		for _, k := range c.Keys {
			if k.Reference != target && e.Name != "" && strings.EqualFold(strings.TrimSpace(k.Alias), strings.TrimSpace(e.Name)) {
				return Metadata{}, ErrConflict
			}
		}
	}
	c.Revision++
	normalizeMetadata(&c)
	if len(encode(c)) > 8<<20 {
		return Metadata{}, ErrInput
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO history_metadata(revision,payload) VALUES(?,?)`, c.Revision, encode(c)); err != nil {
		return Metadata{}, ErrStorage
	}
	if tx.Commit() != nil {
		return Metadata{}, ErrStorage
	}
	return c, nil
}

// These source DTOs decode only the view fields. The complete source JSON,
// including plans and unknown fields, remains byte-exact in the private archive.
type sourceProfiles struct {
	Revision int64 `json:"revision"`
	Profiles []struct {
		ID          string             `json:"id"`
		Name        string             `json:"name"`
		Provider    string             `json:"provider"`
		Archived    bool               `json:"archived"`
		Selection   bool               `json:"selection"`
		Accounts    []string           `json:"accounts"`
		Memberships []sourceMembership `json:"memberships"`
	} `json:"profiles"`
	Bindings []struct {
		Profile string `json:"profile"`
		Account string `json:"account"`
		Start   int64  `json:"start_ms"`
		End     int64  `json:"end_ms"`
	} `json:"bindings"`
}
type sourceMembership struct {
	Account string `json:"account"`
	Start   int64  `json:"start_ms"`
	End     int64  `json:"end_ms"`
}
type Membership struct {
	AccountReference string `json:"account_reference"`
	Start            int64  `json:"start_ms"`
	End              int64  `json:"end_ms"`
}
type Profile struct {
	Reference   string       `json:"reference"`
	Name        string       `json:"name"`
	Provider    string       `json:"provider"`
	Archived    bool         `json:"archived"`
	Selection   bool         `json:"selection"`
	Memberships []Membership `json:"memberships"`
}
type Profiles struct {
	Revision int64     `json:"source_revision"`
	Profiles []Profile `json:"profiles"`
}

// Profiles returns the requested CPAMP revision (-1 means latest). Source
// revisions are immutable and independent of native metadata revisions.
func (s *Store) Profiles(ctx context.Context, revision int64) (Profiles, error) {
	if revision < -1 {
		return Profiles{}, ErrInput
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return Profiles{}, ErrStorage
	}
	defer tx.Rollback()
	var complete bool
	err := tx.QueryRowContext(ctx, `SELECT complete FROM history_import WHERE id=1`).Scan(&complete)
	if errors.Is(err, sql.ErrNoRows) {
		return Profiles{}, ErrPending
	}
	if err != nil {
		return Profiles{}, ErrStorage
	}
	if !complete {
		return Profiles{}, ErrPending
	}
	var spec []byte
	var t tableSpec
	if tx.QueryRowContext(ctx, `SELECT spec FROM history_tables WHERE name='analytics_profile_revisions'`).Scan(&spec) != nil || json.Unmarshal(spec, &t) != nil {
		return Profiles{}, ErrStorage
	}
	query := `SELECT cells FROM history_rows WHERE tbl='analytics_profile_revisions'`
	args := []any{}
	if revision >= 0 {
		query += ` AND key=?`
		args = append(args, encode([]Cell{{Type: "integer", Data: strconv.FormatInt(revision, 10)}}))
	}
	query += ` ORDER BY ordinal DESC LIMIT 1`
	var raw []byte
	var cells []Cell
	e = tx.QueryRowContext(ctx, query, args...).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return Profiles{}, ErrInput
	}
	if e != nil || json.Unmarshal(raw, &cells) != nil {
		return Profiles{}, ErrStorage
	}
	m := rowMap(t, archiveRow{Cells: cells})
	var c sourceProfiles
	if json.Unmarshal([]byte(m["payload"].text()), &c) != nil {
		return Profiles{}, ErrStorage
	}
	out := Profiles{Revision: c.Revision, Profiles: []Profile{}}
	for _, p := range c.Profiles {
		v := Profile{Reference: "profile:" + hash([]byte(p.ID)), Name: safeLabel(p.Name), Provider: safeLabel(p.Provider), Archived: p.Archived, Selection: p.Selection, Memberships: []Membership{}}
		members := p.Memberships
		if members == nil {
			if p.Selection {
				for _, a := range p.Accounts {
					members = append(members, sourceMembership{Account: a})
				}
			} else {
				for _, b := range c.Bindings {
					if b.Profile == p.ID {
						members = append(members, sourceMembership{Account: b.Account, Start: b.Start, End: b.End})
					}
				}
			}
		}
		for _, m := range members {
			v.Memberships = append(v.Memberships, Membership{AccountReference: "account:" + hash([]byte(m.Account)), Start: m.Start, End: m.End})
		}
		out.Profiles = append(out.Profiles, v)
	}
	return out, nil
}
