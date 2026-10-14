package pacer

import (
	"database/sql"
	"errors"
)

// routingMode normalizes a configured or persisted mode: off or on. The former
// dry-run mode never routed, so it reads as off. Anything else is invalid ("").
func routingMode(mode string) string {
	switch mode {
	case "", "off", "dry-run":
		return "off"
	case "on":
		return "on"
	}
	return ""
}

// CPA's YAML seeds the control, and an explicit YAML mode change takes priority.
// A management change survives unrelated hot reloads and process restarts.
func reconcileMode(db *sql.DB, configured string) (string, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS pacer_routing_settings (id INTEGER PRIMARY KEY CHECK(id=1), configured TEXT NOT NULL, effective TEXT NOT NULL)`); err != nil {
		return "", err
	}
	var previous, effective string
	err := db.QueryRow(`SELECT configured,effective FROM pacer_routing_settings WHERE id=1`).Scan(&previous, &effective)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if errors.Is(err, sql.ErrNoRows) || configured != routingMode(previous) {
		_, err = db.Exec(`INSERT INTO pacer_routing_settings(id,configured,effective) VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET configured=excluded.configured,effective=excluded.effective`, configured, configured)
		return configured, err
	}
	mode := routingMode(effective)
	if mode == "" {
		return "", errors.New("persisted routing mode is invalid")
	}
	return mode, nil
}

func (p *Plugin) setMode(mode string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if mode != "off" && mode != "on" {
		return errors.New("routing mode is off or on")
	}
	if _, err := p.db.Exec(`UPDATE pacer_routing_settings SET effective=? WHERE id=1`, mode); err != nil {
		return err
	}
	p.cfg.Routing.Mode = mode
	p.dryRunMarkLocked()
	return nil
}
