package cpamp

// Additive, non-generated access to the extracted calculation implementation.
// Historical assumptions are deliberately unavailable at this boundary.
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

func ForwardValue(e Event, book ComparisonPriceBook) APIValue { return valueEvent(e, book) }
func ForwardAdd(a *APIValue, b APIValue)                      { a.add(b) }
func ForwardContains(c ProfileConfig, p Profile, account string, at int64) bool {
	return quotaProfileContains(c, p, account, at)
}
func (s *Service) ForwardCorrect(ctx context.Context, events []Event, book ComparisonPriceBook, through int64) error {
	if err := s.correctEventAccounting(ctx, events); err != nil {
		return err
	}
	// Same highest-revision precedence as enrichTokenEvidence, but bounded to the
	// requested page instead of materializing all evidence for an account.
	if book.Policy != profileValuePolicy || through == 0 {
		return nil
	}
	for i := range events {
		if err := ctx.Err(); err != nil {
			return err
		}
		var evidence TokenEvidence
		var raw []byte
		err := s.db.QueryRowContext(ctx, `SELECT revision,source_id,payload FROM analytics_token_evidence WHERE event_hash=? AND revision<=? ORDER BY revision DESC LIMIT 1`, events[i].Hash, through).Scan(&evidence.Revision, &evidence.SourceID, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &evidence.Details); err != nil {
			return err
		}
		events[i].TokenEvidence = &evidence
	}
	return nil
}
func (s *Service) ForwardBook(ctx context.Context, id string) (ComparisonPriceBook, error) {
	return s.comparisonPriceBook(ctx, id)
}
