package historyreports

// Window exclusions: the user's choice that a profile's window must not count
// towards its average (a window that is no longer relevant, or unfair to count).
// Nothing is deleted: every change is a new append-only revision object, older
// revisions stay readable, and switching an exclusion off restores the window.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const exclusionKind = "window-exclusions"
const maxExclusions = 10000

type WindowExclusion struct {
	Profile string `json:"profile"`
	Window  string `json:"window"`
	Note    string `json:"note,omitempty"`
	AtMS    int64  `json:"at_ms"`
}

type WindowExclusions struct {
	Revision int64             `json:"revision"`
	Excluded []WindowExclusion `json:"excluded"`
}

var (
	exclusionProfile = regexp.MustCompile(`^profile:[a-f0-9]{64}$`)
	exclusionWindow  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:|-]{0,255}$`) // server-issued window IDs
)

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func exclusionName(revision int64) string { return fmt.Sprintf("%020d", revision) }

func readExclusions(ctx context.Context, q rowQueryer) (WindowExclusions, error) {
	out := WindowExclusions{Excluded: []WindowExclusion{}}
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT body FROM forward_objects WHERE kind=? ORDER BY name DESC LIMIT 1`, exclusionKind).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if json.Unmarshal(raw, &out) != nil || out.Revision <= 0 {
		return WindowExclusions{Excluded: []WindowExclusion{}}, ErrSource
	}
	if out.Excluded == nil {
		out.Excluded = []WindowExclusion{}
	}
	return out, nil
}

// WindowExclusions returns the latest exclusion set (revision 0 when none).
func (s *ForwardStore) WindowExclusions(ctx context.Context) (WindowExclusions, error) {
	return readExclusions(ctx, s.reader)
}

func validExclusionNote(note string) bool {
	if !utf8.ValidString(note) || utf8.RuneCountInString(note) > 200 {
		return false
	}
	lower := strings.ToLower(note)
	for _, marker := range []string{"sk-", "bearer ", "cmp_admin_", "-----begin", "api_key=", "access_token", "refresh_token"} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	for _, r := range note {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// SetWindowExclusion switches one window in or out of a profile's average under
// optimistic revision control (expected must be the current revision).
func (s *ForwardStore) SetWindowExclusion(ctx context.Context, expected int64, profile, window string, excluded bool, note string, at time.Time) (WindowExclusions, error) {
	if expected < 0 || !exclusionProfile.MatchString(profile) || !exclusionWindow.MatchString(window) || !validExclusionNote(note) || !excluded && note != "" {
		return WindowExclusions{}, ErrInput
	}
	var out WindowExclusions
	err := s.write(ctx, func(tx *sql.Tx) error {
		current, err := readExclusions(ctx, tx)
		if err != nil {
			return err
		}
		if current.Revision != expected {
			return ErrForwardConflict
		}
		next := []WindowExclusion{}
		for _, e := range current.Excluded {
			if e.Profile != profile || e.Window != window {
				next = append(next, e)
			}
		}
		if excluded {
			next = append(next, WindowExclusion{Profile: profile, Window: window, Note: note, AtMS: at.UnixMilli()})
		}
		if len(next) > maxExclusions {
			return ErrLimit
		}
		sort.Slice(next, func(i, j int) bool {
			if next[i].Profile != next[j].Profile {
				return next[i].Profile < next[j].Profile
			}
			return next[i].Window < next[j].Window
		})
		out = WindowExclusions{Revision: current.Revision + 1, Excluded: next}
		_, err = forwardObject(tx, ctx, exclusionKind, exclusionName(out.Revision), out)
		return err
	})
	if err != nil {
		return WindowExclusions{}, err
	}
	return out, nil
}

// Exclusions maps profile reference -> window ID -> note, for the usage views.
type Exclusions map[string]map[string]string

func (w WindowExclusions) Map() Exclusions {
	out := Exclusions{}
	for _, e := range w.Excluded {
		if out[e.Profile] == nil {
			out[e.Profile] = map[string]string{}
		}
		out[e.Profile][e.Window] = e.Note
	}
	return out
}

func (x Exclusions) has(profile, window string) bool {
	_, ok := x[profile][window]
	return ok
}
