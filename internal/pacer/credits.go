package pacer

import (
	"database/sql"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
)

func openCredits(statePath string, now func() time.Time) (*sql.DB, *observation.CreditStore) {
	if statePath == "" {
		return nil, nil
	}
	db := openOwnerOnlySQLite(statePath + ".credits.sqlite")
	if db == nil {
		return nil, nil
	}
	credits, err := observation.OpenCredits(db, now)
	if err != nil {
		db.Close()
		return nil, nil
	}
	return db, credits
}

func (p *Plugin) readCredits(account string) observation.Credits {
	// The pointer is immutable after configure. Close may close its database;
	// unavailable optional evidence must not hide ordinary quota windows.
	if p.credits != nil {
		if credits, err := p.credits.ReadCredits(account); err == nil {
			return credits
		}
	}
	return observation.Credits{State: "unknown", Source: "api"}
}
