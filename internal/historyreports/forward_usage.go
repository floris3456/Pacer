package historyreports

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// UsageNative is the forward store's share of the continuous usage record:
// the latest revision of every native request that started at or after From,
// in start order, and the origin certificates of the recorded cutover (none
// when no cutover is recorded: native accounts then keep their own references).
type UsageNative struct {
	Events  []ForwardEvent
	Origins ForwardOrigins
	Through int64 // highest native sequence read
}

// UsageNative reads at most limit native requests (ErrLimit beyond it).
func (s *ForwardStore) UsageNative(ctx context.Context, from time.Time, limit int) (UsageNative, error) {
	out := UsageNative{Events: []ForwardEvent{}}
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
	rows, err := s.reader.QueryContext(ctx, `SELECT sequence,body FROM forward_native n WHERE n.started>=? AND n.revision=(SELECT max(x.revision) FROM forward_native x WHERE x.event_id=n.event_id) ORDER BY n.started,n.sort_key LIMIT ?`, from.UnixNano(), limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var body []byte
		if err = rows.Scan(&seq, &body); err != nil {
			return out, err
		}
		var e ForwardEvent
		if json.Unmarshal(body, &e) != nil {
			return out, ErrSource
		}
		if len(out.Events) == limit {
			return out, ErrLimit
		}
		out.Events = append(out.Events, e)
		out.Through = max(out.Through, seq)
	}
	return out, rows.Err()
}
