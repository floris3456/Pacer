package modelprice

import (
	"context"
	"database/sql"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp/model"
)

func Load(ctx context.Context, db *sql.DB) (map[string]model.ModelPrice, error) {
	return (&repository{db: db}).LoadAll(ctx)
}
