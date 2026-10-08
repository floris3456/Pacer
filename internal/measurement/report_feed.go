package measurement

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// The trigger is compiled application schema, never imported SQL. A feed write
// failure aborts the revision INSERT and hence the enclosing PutRecord commit.
// Referencing immutable revision rows also journals older compatible writers.
const reportFeedSchema = `
CREATE TABLE measurement_report_meta (
 id INTEGER PRIMARY KEY CHECK(id=1), version INTEGER NOT NULL,
 feed_id TEXT NOT NULL, started INTEGER NOT NULL DEFAULT 0,
 high_event TEXT NOT NULL DEFAULT '', high_revision INTEGER NOT NULL DEFAULT 0,
 after_event TEXT NOT NULL DEFAULT '', after_revision INTEGER NOT NULL DEFAULT 0,
 complete INTEGER NOT NULL DEFAULT 0);
INSERT INTO measurement_report_meta(id,version,feed_id) VALUES(1,1,lower(hex(randomblob(32))));
CREATE TABLE measurement_report_feed (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 event_id TEXT NOT NULL, revision INTEGER NOT NULL, received_at INTEGER NOT NULL,
 UNIQUE(event_id,revision));
CREATE TRIGGER measurement_report_revision_insert AFTER INSERT ON measurement_record_revisions
BEGIN
 INSERT INTO measurement_report_feed(event_id,revision,received_at)
 VALUES(NEW.event_id,NEW.revision,NEW.received_at);
END;
`

func openReportFeed(ctx context.Context, c *sql.Conn) error {
	var exists int
	if err := c.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name='measurement_report_meta' AND type='table'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		_, err := c.ExecContext(ctx, reportFeedSchema)
		return err
	}
	var version int
	if err := c.QueryRowContext(ctx, `SELECT version FROM measurement_report_meta WHERE id=1`).Scan(&version); err != nil {
		return err
	}
	if version != 1 {
		return ErrInvalid
	}
	for _, name := range []string{"measurement_report_feed", "measurement_report_revision_insert"} {
		if err := c.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name=?`, name).Scan(&exists); err != nil {
			return err
		}
		if exists != 1 {
			return ErrConflict
		}
	}
	return nil
}

// ReportBudget bounds one operation, not total ledger size. Bytes counts source
// bodies plus raw evidence even though raw evidence is never exported.
type ReportBudget struct {
	Rows  int
	Bytes int
}

func (b ReportBudget) checked() (ReportBudget, error) {
	if b.Rows == 0 {
		b.Rows = 250
	}
	if b.Bytes == 0 {
		b.Bytes = 2 << 20
	}
	if b.Rows < 1 || b.Rows > 250 || b.Bytes < 1 || b.Bytes > 2<<20 {
		return b, ErrInvalid
	}
	return b, nil
}

type ReportFeedHead struct {
	ID                string `json:"id"`
	Through           int64  `json:"through,string"`
	BootstrapComplete bool   `json:"bootstrap_complete"`
}

type ReportFeedEntry struct {
	Sequence int64 `json:"sequence,string"`
	StoredRecord
	BodyDigest string `json:"body_digest"`
	RawDigest  string `json:"raw_digest"`
}

type ReportFeedPage struct {
	Entries []ReportFeedEntry `json:"entries"`
	Next    int64             `json:"next,string"`
	Done    bool              `json:"done"`
	Bytes   int               `json:"source_bytes"`
}

func (s *Store) ReportFeedHead(ctx context.Context) (h ReportFeedHead, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT feed_id,complete,coalesce((SELECT max(sequence) FROM measurement_report_feed),0) FROM measurement_report_meta WHERE id=1`).Scan(&h.ID, &h.BootstrapComplete, &h.Through)
	return
}

// BootstrapReportFeed adopts at most one bounded batch. The first invocation
// captures a finite indexed high key. Live INSERTs behind/ahead of that key are
// already journaled by the trigger. Adoption order is not historical receipt time.
func (s *Store) BootstrapReportFeed(ctx context.Context, budget ReportBudget) (bool, error) {
	b, err := budget.checked()
	if err != nil {
		return false, err
	}
	done := false
	err = s.write(ctx, func(tx *sql.Tx) error {
		var started bool
		var hi, after string
		var hr, ar int64
		if err := tx.QueryRowContext(ctx, `SELECT started,complete,high_event,high_revision,after_event,after_revision FROM measurement_report_meta WHERE id=1`).Scan(&started, &done, &hi, &hr, &after, &ar); err != nil {
			return err
		}
		if done {
			return nil
		}
		if !started {
			err := tx.QueryRowContext(ctx, `SELECT event_id,revision FROM measurement_record_revisions ORDER BY event_id DESC,revision DESC LIMIT 1`).Scan(&hi, &hr)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE measurement_report_meta SET started=1,high_event=?,high_revision=? WHERE id=1`, hi, hr); err != nil {
				return err
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT event_id,revision,received_at,length(body)+length(raw_usage) FROM measurement_record_revisions WHERE (event_id,revision)>(?,?) AND (event_id,revision)<=(?,?) ORDER BY event_id,revision LIMIT ?`, after, ar, hi, hr, b.Rows)
		if err != nil {
			return err
		}
		type ref struct {
			id            string
			rev, received int64
		}
		refs := []ref{}
		used := 0
		for rows.Next() {
			if err = ctx.Err(); err != nil {
				break
			}
			var r ref
			var n int
			if err = rows.Scan(&r.id, &r.rev, &r.received, &n); err != nil {
				break
			}
			if n > b.Bytes-used {
				if len(refs) == 0 {
					err = ErrLimit
				}
				break
			}
			used += n
			refs = append(refs, r)
		}
		rowErr := rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if rowErr != nil {
			return rowErr
		}
		for _, r := range refs {
			if err = ctx.Err(); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO measurement_report_feed(event_id,revision,received_at) VALUES(?,?,?) ON CONFLICT(event_id,revision) DO NOTHING`, r.id, r.rev, r.received); err != nil {
				return err
			}
			after, ar = r.id, r.rev
		}
		done = hi == "" || after == hi && ar == hr
		_, err = tx.ExecContext(ctx, `UPDATE measurement_report_meta SET after_event=?,after_revision=?,complete=? WHERE id=1`, after, ar, done)
		return err
	})
	return done, err
}

// ReadReportFeed reads one fixed head using an indexed keyset and closes its
// snapshot before returning. Consumers must select MAX(Revision), not last seq.
func (s *Store) ReadReportFeed(ctx context.Context, head ReportFeedHead, after int64, budget ReportBudget) (page ReportFeedPage, err error) {
	b, err := budget.checked()
	if err != nil {
		return page, err
	}
	if after < 0 || after > head.Through || !head.BootstrapComplete {
		return page, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	var id string
	var complete bool
	var maxSeq int64
	if err = tx.QueryRowContext(ctx, `SELECT feed_id,complete,coalesce((SELECT max(sequence) FROM measurement_report_feed),0) FROM measurement_report_meta WHERE id=1`).Scan(&id, &complete, &maxSeq); err != nil {
		return page, err
	}
	if id != head.ID || !complete || head.Through > maxSeq {
		return page, ErrConflict
	}
	page.Entries = []ReportFeedEntry{}
	page.Next = after
	rows, err := tx.QueryContext(ctx, `SELECT f.sequence,f.event_id,f.revision,f.received_at,r.body,r.raw_usage FROM measurement_report_feed f LEFT JOIN measurement_record_revisions r ON r.event_id=f.event_id AND r.revision=f.revision WHERE f.sequence>? AND f.sequence<=? ORDER BY f.sequence LIMIT ?`, after, head.Through, b.Rows)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			return page, err
		}
		var entry ReportFeedEntry
		var eid string
		var rev, received int64
		var body, raw []byte
		if err = rows.Scan(&entry.Sequence, &eid, &rev, &received, &body, &raw); err != nil {
			return page, err
		}
		n := len(body) + len(raw)
		if n > b.Bytes-page.Bytes {
			if len(page.Entries) == 0 {
				return page, ErrLimit
			}
			break
		}
		if len(body) == 0 || json.Unmarshal(body, &entry.Record) != nil || entry.EventID != eid || entry.Revision != rev {
			return page, ErrConflict
		}
		entry.ReceivedAt = stamp(received)
		entry.BodyDigest = digest(body)
		entry.RawDigest = digest(raw)
		page.Bytes += n
		page.Next = entry.Sequence
		page.Entries = append(page.Entries, entry)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	rows.Close()
	page.Done = page.Next == head.Through
	if !page.Done && len(page.Entries) == 0 {
		return page, ErrConflict
	}
	err = tx.Commit()
	return page, err
}
