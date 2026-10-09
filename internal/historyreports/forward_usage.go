package historyreports

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// UsageNative is the forward store's share of the continuous usage record:
// native request rows (every revision, in sequence order) whose request
// started at or after the cutoff, and the origin certificates of the recorded
// cutover (none without a cutover: native accounts keep their own references).
// Consumers keep the highest revision of each event ID.
type UsageNative struct {
	Events  []ForwardEvent
	Origins ForwardOrigins
	Through int64 // last sequence scanned
	More    bool  // the page was full: read again after Through
}

// UsageNative scans at most limit rows after sequence afterSeq.
func (s *ForwardStore) UsageNative(ctx context.Context, from time.Time, afterSeq int64, limit int) (UsageNative, error) {
	out := UsageNative{Events: []ForwardEvent{}, Through: afterSeq}
	if limit <= 0 || limit > 10000 || afterSeq < 0 {
		return out, ErrInput
	}
	var rec forwardCutoverRecord
	switch err := s.objectNamed(ctx, "cutover", "1", &rec); {
	case err == nil:
		if err = s.object(ctx, "origins", rec.OriginDigest, &out.Origins); err != nil {
			return out, err
		}
	case errors.Is(err, sql.ErrNoRows):
	default:
		return out, err
	}
	rows, err := s.reader.QueryContext(ctx, `SELECT sequence,started,body FROM forward_native WHERE sequence>? ORDER BY sequence LIMIT ?`, afterSeq, limit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var seq, started int64
		var body []byte
		if err = rows.Scan(&seq, &started, &body); err != nil {
			return out, err
		}
		n++
		out.Through = seq
		if started < from.UnixNano() {
			continue
		}
		var e ForwardEvent
		if json.Unmarshal(body, &e) != nil {
			return out, ErrSource
		}
		out.Events = append(out.Events, e)
	}
	out.More = n == limit
	return out, rows.Err()
}
