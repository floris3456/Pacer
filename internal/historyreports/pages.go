package historyreports

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
)

type WindowQuery struct {
	Query
	Kind   string `json:"kind,omitempty"`  // empty, 5h, weekly
	State  string `json:"state,omitempty"` // completed (default), current, all
	Limit  int    `json:"limit"`           // default 25, maximum 100
	Cursor string `json:"cursor,omitempty"`
}
type WindowPage struct {
	Pin        Pin      `json:"pin"`
	Windows    []Window `json:"windows"`
	Total      int      `json:"total"`
	NextCursor string   `json:"next_cursor,omitempty"`
}
type pageCursor struct {
	Binding string
	Reset   int64
	ID      string
}

func (r *Reader) sign(b []byte) []byte {
	h := hmac.New(sha256.New, r.meta.CursorKey)
	h.Write(b)
	return h.Sum(nil)
}
func (r *Reader) cursor(c pageCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString(r.sign(b))
}
func (r *Reader) parseCursor(s, binding string) (pageCursor, error) {
	var c pageCursor
	if len(s) > 2048 {
		return c, ErrCursor
	}
	parts := strings.Split(s, ".")
	if len(parts) != 2 {
		return c, ErrCursor
	}
	b, e := base64.RawURLEncoding.DecodeString(parts[0])
	if e != nil {
		return c, ErrCursor
	}
	mac, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil || !hmac.Equal(mac, r.sign(b)) || json.Unmarshal(b, &c) != nil || c.Binding != binding {
		return c, ErrCursor
	}
	return c, nil
}

func (r *Reader) ListWindows(ctx context.Context, q WindowQuery) (WindowPage, error) {
	if q.Limit == 0 {
		q.Limit = 25
	}
	if q.State == "" {
		q.State = "completed"
	}
	if q.Limit < 1 || q.Limit > 100 || (q.Kind != "" && q.Kind != "5h" && q.Kind != "weekly") || (q.State != "completed" && q.State != "current" && q.State != "all") {
		return WindowPage{}, ErrInput
	}
	ctx, cancel := bounded(ctx)
	defer cancel()
	f, e := r.filter(ctx, q.Query)
	if e != nil {
		return WindowPage{}, e
	}
	if f.Profile == "all" {
		return WindowPage{}, ErrInput
	}
	bind, _ := json.Marshal(struct {
		Version, Source, Kind, State string
		Pin                          Pin
		Limit                        int
	}{Version, r.meta.Boundary.SnapshotSHA256, q.Kind, q.State, f, q.Limit})
	binding := hash(string(bind))
	var after pageCursor
	if q.Cursor != "" {
		after, e = r.parseCursor(q.Cursor, binding)
		if e != nil {
			return WindowPage{}, e
		}
	}
	w, e := r.service.Workspace(ctx, f)
	if e != nil {
		return WindowPage{}, domainError(ctx, e)
	}
	windows := w.Windows
	if f.Profile == "unassigned" {
		windows = w.Unassigned.Windows
	} else if f.Profile != "" {
		windows = nil
		for _, p := range w.Profiles {
			if p.ID == f.Profile {
				windows = p.Windows
				break
			}
		}
	}
	matched := []Window{}
	for _, win := range windows {
		if q.Kind != "" && win.Kind != q.Kind {
			continue
		}
		if q.State == "current" && win.Status != "current" {
			continue
		}
		if q.State == "completed" && (win.Status != "closed" || win.Valuation != nil && (win.Valuation.Completion.Status != "closed" || win.Valuation.Completion.Latest > f.At)) {
			continue
		}
		matched = append(matched, win)
	}
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].Reset != matched[j].Reset {
			return matched[i].Reset > matched[j].Reset
		}
		return matched[i].ID < matched[j].ID
	})
	page := WindowPage{Pin: w.Filter, Total: len(matched), Windows: []Window{}}
	start := 0
	if q.Cursor != "" {
		found := false
		for i, win := range matched {
			if win.Reset == after.Reset && win.ID == after.ID {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			return WindowPage{}, ErrCursor
		}
	}
	end := min(start+q.Limit, len(matched))
	page.Windows = append(page.Windows, matched[start:end]...)
	if end < len(matched) {
		last := matched[end-1]
		page.NextCursor = r.cursor(pageCursor{Binding: binding, Reset: last.Reset, ID: last.ID})
	}
	return page, nil
}
