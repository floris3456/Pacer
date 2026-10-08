package historyreports

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

// AdvanceForward releases the source snapshot before taking the destination
// writer. A failure here cannot undo or acknowledge the already committed usage.
func (s *ForwardStore) AdvanceForward(ctx context.Context, feed *measurement.Store, head measurement.ReportFeedHead, budget measurement.ReportBudget) (bool, error) {
	if feed == nil {
		return false, ErrInput
	}
	status, err := s.Status(ctx)
	if err != nil {
		return false, err
	}
	if status.FeedID != "" && status.FeedID != head.ID {
		return false, ErrForwardConflict
	}
	page, err := feed.ReadReportFeed(ctx, head, status.Through, budget)
	if err != nil {
		return false, err
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		var id string
		var n int64
		if e := tx.QueryRowContext(ctx, `SELECT feed_id,through_seq FROM forward_meta WHERE id=1`).Scan(&id, &n); e != nil {
			return e
		}
		if n != status.Through || id != status.FeedID {
			return ErrForwardConflict
		}
		for _, entry := range page.Entries {
			if e := ctx.Err(); e != nil {
				return e
			}
			f := convertForward(entry)
			body, e := json.Marshal(f)
			if e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, `INSERT INTO forward_native(event_id,revision,sequence,sort_key,physical,started,account,provider,credential,body) VALUES(?,?,?,?,?,?,?,?,?,?)`, f.ID, f.Revision, f.Sequence, "native-event:"+f.ID, f.PhysicalKey, f.StartedAt.UnixNano(), ForwardAccountRef(f.SourceID, f.Event.Provider, f.Event.Account), f.Event.Provider, f.Attribution.CredentialRef, body); e != nil {
				return e
			}
		}
		_, e := tx.ExecContext(ctx, `UPDATE forward_meta SET feed_id=?,through_seq=?,bootstrap_through=? WHERE id=1`, head.ID, page.Next, head.BootstrapThrough)
		return e
	})
	return page.Done, err
}

type ForwardBaselinePage struct {
	Events []ForwardEvent
	Next   string
	Done   bool
	Bytes  int
}

// ReadBaselineEvents keysets on the frozen table's existing primary key. No new
// index is built on the archive. Unadmitted rows advance the cursor, not totals.
func (r *Reader) ReadBaselineEvents(ctx context.Context, after string, budget measurement.ReportBudget) (page ForwardBaselinePage, err error) {
	rowsLimit, bytesLimit := budget.Rows, budget.Bytes
	if rowsLimit == 0 {
		rowsLimit = 250
	}
	if bytesLimit == 0 {
		bytesLimit = 2 << 20
	}
	if rowsLimit < 1 || rowsLimit > 250 || bytesLimit < 1 || bytesLimit > 2<<20 {
		return page, ErrInput
	}
	book, err := r.service.ForwardBook(ctx, r.meta.Pin.ValuationRule)
	if err != nil {
		return page, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT event_hash,event_id,payload FROM analytics_events WHERE event_hash>? ORDER BY event_hash LIMIT ?`, after, rowsLimit)
	if err != nil {
		return page, err
	}
	page.Events = []ForwardEvent{}
	page.Next = after
	events := []cpamp.Event{}
	digests := []string{}
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			break
		}
		var key string
		var id int64
		var raw []byte
		if err = rows.Scan(&key, &id, &raw); err != nil {
			break
		}
		if len(raw) > bytesLimit-page.Bytes {
			if page.Next == after {
				err = ErrLimit
			}
			break
		}
		page.Next = key
		page.Bytes += len(raw)
		if id > r.meta.Pin.Through {
			continue
		}
		var e cpamp.Event
		if json.Unmarshal(raw, &e) != nil || e.Hash != key || e.ID != id {
			err = ErrSource
			break
		}
		events = append(events, e)
		digests = append(digests, hash(string(raw)))
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	if rowErr != nil {
		return page, rowErr
	}
	if err = r.service.ForwardCorrect(ctx, events, book, r.meta.Pin.TokenEvidenceThrough); err != nil {
		return page, err
	}
	for i, e := range events {
		if e.Started <= 0 || e.Started > int64(^uint64(0)>>1)/int64(time.Millisecond) {
			return page, ErrSource
		}
		e.QuotaHeaders = nil
		e.Metadata = nil
		e.AuthFile = ""
		e.AuthIndex = ""
		e.Value = nil
		f := ForwardEvent{ID: "cpamp-event:" + forwardDigest([]string{r.meta.Boundary.LineageSHA256, e.Hash}), Origin: "baseline", SourceID: "cpamp:" + r.meta.Boundary.LineageSHA256, AttemptID: e.RequestID, Revision: 1, StartedAt: time.UnixMilli(e.Started).UTC(), EndedAt: time.UnixMilli(e.Completed).UTC(), ReceivedAt: time.UnixMilli(e.Recorded).UTC(), StartBasis: "cpamp_recorded_ms", Final: true, Accounting: "cpamp-frozen", Quality: e.Quality, Event: e, BodyDigest: digests[i], Issues: []string{}}
		f.RawInputBasis = "frozen_cpamp_raw_input"
		f.Issues = forwardBaselineIssues(e)
		page.Events = append(page.Events, f)
	}
	var next string
	err = r.db.QueryRowContext(ctx, `SELECT event_hash FROM analytics_events WHERE event_hash>? ORDER BY event_hash LIMIT 1`, page.Next).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		page.Done = true
		err = nil
	}
	return page, err
}

// PrepareForwardBase is trusted, offline, resumable preparation. It never alters
// the archive or derives a new historical assumption cohort from the current time.
func (s *ForwardStore) PrepareForwardBase(ctx context.Context, r *Reader, budget measurement.ReportBudget) (string, error) {
	if r == nil {
		return "", ErrInput
	}
	file, err := os.Open(r.path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	buffer := make([]byte, 64<<10)
	for {
		if err = ctx.Err(); err != nil {
			break
		}
		var n int
		n, err = file.Read(buffer)
		if n > 0 {
			_, _ = h.Write(buffer[:n])
		}
		if err == io.EOF {
			err = nil
			break
		}
		if err != nil {
			break
		}
	}
	file.Close()
	if err != nil {
		return "", err
	}
	base := ForwardBase{Boundary: r.meta.Boundary, Pin: r.meta.Pin, ArtifactDigest: hex.EncodeToString(h.Sum(nil))}
	body, _ := json.Marshal(base)
	d := hash(string(body))
	err = s.write(ctx, func(tx *sql.Tx) error {
		var old string
		e := tx.QueryRowContext(ctx, `SELECT digest FROM forward_base_meta WHERE id=1`).Scan(&old)
		if e == nil {
			if old != d {
				return ErrForwardConflict
			}
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO forward_base_meta(id,digest,body) VALUES(1,?,?)`, d, body)
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
		var done bool
		if err = s.reader.QueryRowContext(ctx, `SELECT after_hash,complete FROM forward_base_meta WHERE id=1`).Scan(&after, &done); err != nil {
			return "", err
		}
		if done {
			return d, nil
		}
		page, e := r.ReadBaselineEvents(ctx, after, budget)
		if e != nil {
			return "", e
		}
		err = s.write(ctx, func(tx *sql.Tx) error {
			var current string
			if e := tx.QueryRowContext(ctx, `SELECT after_hash FROM forward_base_meta WHERE id=1`).Scan(&current); e != nil {
				return e
			}
			if current != after {
				return ErrForwardConflict
			}
			for _, f := range page.Events {
				if e := ctx.Err(); e != nil {
					return e
				}
				raw, e := json.Marshal(f)
				if e != nil {
					return e
				}
				if _, e = tx.ExecContext(ctx, `INSERT INTO forward_base_events(id,event_hash,account,provider,started,body) VALUES(?,?,?,?,?,?)`, f.ID, f.Event.Hash, f.Event.Account, f.Event.Provider, f.StartedAt.UnixNano(), raw); e != nil {
					return e
				}
			}
			_, e := tx.ExecContext(ctx, `UPDATE forward_base_meta SET after_hash=?,event_count=event_count+?,complete=? WHERE id=1`, page.Next, len(page.Events), page.Done)
			return e
		})
		if err != nil {
			return "", err
		}
	}
}

func (r *Reader) ForwardMetadata(ctx context.Context) (ForwardComparisonBook, ForwardProfileConfig, error) {
	b, err := r.service.ForwardBook(ctx, r.meta.Pin.ValuationRule)
	if err != nil {
		return b, ForwardProfileConfig{}, err
	}
	rev := r.meta.Pin.Revision
	if rev == 0 {
		rev = -1
	}
	c, err := r.service.ProfileConfig(ctx, rev)
	return b, c, err
}
