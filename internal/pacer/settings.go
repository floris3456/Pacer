package pacer

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/routing"
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
	return nil
}

// Settings are the routing controls of the Pacer page. Each is seeded by
// CPA's YAML (or a built-in default) and may be changed on the page; a page
// change lasts until the seed of that setting changes, like the mode.
type Settings struct {
	// CompactOnSwitch: a forced move first asks clients that can compact to do
	// so (409 quota_compact_required), so the new account receives a summary.
	CompactOnSwitch bool `json:"compact_on_switch"`
	// CompactionModels writes that summary: the first, then each next one if
	// the previous fails. CPA model names; empty leaves it to the client.
	CompactionModels []string `json:"compaction_models"`
	// Band: accounts within this % of the best pace count as equally good, so
	// cold chats stay instead of moving for a small gain.
	Band float64 `json:"band_percent"`
	// NewChatMinimum: % left of the 5-hour window an account needs to take a
	// new or moved chat.
	NewChatMinimum float64 `json:"new_chat_minimum_percent"`
	// ShortFloor, LongFloor: % left at or below which an account serves no
	// chat, not even a warm one (a forced move).
	ShortFloor float64 `json:"short_floor_percent"`
	LongFloor  float64 `json:"long_floor_percent"`
	// WarmMinutes: how long after its last request a chat counts as warm when
	// Pacer has no cache evidence for it.
	WarmMinutes int `json:"warm_minutes"`
	// TransientRetries: how often a warm chat's request is refused for a retry
	// when its account is briefly unavailable (an upstream error) although its
	// meters are fine, before the chat is moved. 0 moves at once.
	TransientRetries int `json:"transient_retries"`
	// ForceWarning: points above a floor from which clients show FORCE USE,
	// the account a forced move will soon send the chat to.
	ForceWarning float64 `json:"force_warning_percent"`
	// Providers routed by pace.
	Providers []string `json:"providers"`
	// PausedAccounts take no new or moved chats; their warm chats stay.
	PausedAccounts []string `json:"paused_accounts"`
}

// Limits of the settings, shared with the page.
const (
	maxCompactionModels = 8
	maxPausedAccounts   = 64
	transientWindow     = 2 * time.Minute
)

var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+\- ]{0,127}$`)

func defaultSettings() Settings {
	return Settings{CompactionModels: []string{}, Band: 90, NewChatMinimum: 15, ShortFloor: 2, LongFloor: 1, WarmMinutes: 60, TransientRetries: 3, ForceWarning: 5, Providers: []string{"claude"}, PausedAccounts: []string{}}
}

// seedSettings: the YAML values where CPA's YAML has them, else the defaults.
func seedSettings(cfg Config) Settings {
	s := defaultSettings()
	s.CompactOnSwitch = cfg.CompactOnSwitch
	if len(cfg.CompactionModels) > 0 {
		s.CompactionModels = append([]string{}, cfg.CompactionModels...)
	}
	if len(cfg.Routing.Providers) > 0 {
		s.Providers = append([]string{}, cfg.Routing.Providers...)
	}
	return s
}

func (s Settings) validate() error {
	in := func(v, lo, hi float64) bool { return v >= lo && v <= hi && v == v }
	switch {
	case len(s.CompactionModels) > maxCompactionModels:
		return fmt.Errorf("at most %d compaction models", maxCompactionModels)
	case !in(s.Band, 50, 100):
		return errors.New("band is 50 to 100 %")
	case !in(s.NewChatMinimum, 0, 100):
		return errors.New("new-chat minimum is 0 to 100 %")
	case !in(s.ShortFloor, 0, 50) || !in(s.LongFloor, 0, 50):
		return errors.New("floors are 0 to 50 %")
	case s.WarmMinutes < 0 || s.WarmMinutes > 24*60:
		return errors.New("warm time is 0 to 1440 minutes")
	case s.TransientRetries < 0 || s.TransientRetries > 10:
		return errors.New("retries are 0 to 10")
	case !in(s.ForceWarning, 0, 50):
		return errors.New("force warning is 0 to 50 %")
	case len(s.Providers) == 0:
		return errors.New("route at least one provider")
	case len(s.PausedAccounts) > maxPausedAccounts:
		return errors.New("too many paused accounts")
	}
	seen := map[string]bool{}
	for _, m := range s.CompactionModels {
		if !modelName.MatchString(m) || seen[m] {
			return fmt.Errorf("compaction model %q is invalid or repeated", m)
		}
		seen[m] = true
	}
	seen = map[string]bool{}
	for _, v := range s.Providers {
		if (v != "claude" && v != "codex") || seen[v] {
			return errors.New("providers are claude and codex")
		}
		seen[v] = true
	}
	for _, a := range s.PausedAccounts {
		if a == "" || len(a) > 256 {
			return errors.New("paused account is invalid")
		}
	}
	return nil
}

func (s Settings) routing() routing.Config {
	c := routing.Default()
	c.Band, c.NewShort, c.ShortFloor, c.LongFloor = s.Band/100, s.NewChatMinimum, s.ShortFloor, s.LongFloor
	return c
}

func (s Settings) warmFor() time.Duration { return time.Duration(s.WarmMinutes) * time.Minute }

func (s Settings) paused(account string) bool {
	for _, a := range s.PausedAccounts {
		if a == account {
			return true
		}
	}
	return false
}

// settingFields maps each setting's JSON name to its value, so each is seeded
// and overridden on its own.
func settingFields(s Settings) (map[string]json.RawMessage, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	fields := map[string]json.RawMessage{}
	return fields, json.Unmarshal(raw, &fields)
}

// reconcileSettings applies a page change of a setting until its seed changes.
func reconcileSettings(db *sql.DB, seed Settings) (Settings, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS pacer_settings (name TEXT PRIMARY KEY, configured TEXT NOT NULL, effective TEXT NOT NULL)`); err != nil {
		return Settings{}, err
	}
	seeds, err := settingFields(seed)
	if err != nil {
		return Settings{}, err
	}
	effective := map[string]json.RawMessage{}
	for name, value := range seeds {
		var configured, current string
		err := db.QueryRow(`SELECT configured,effective FROM pacer_settings WHERE name=?`, name).Scan(&configured, &current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Settings{}, err
		}
		if errors.Is(err, sql.ErrNoRows) || configured != string(value) {
			if _, err := db.Exec(`INSERT INTO pacer_settings(name,configured,effective) VALUES(?,?,?) ON CONFLICT(name) DO UPDATE SET configured=excluded.configured,effective=excluded.effective`, name, string(value), string(value)); err != nil {
				return Settings{}, err
			}
			current = string(value)
		}
		effective[name] = json.RawMessage(current)
	}
	raw, _ := json.Marshal(effective)
	s := defaultSettings()
	if err := json.Unmarshal(raw, &s); err != nil || s.validate() != nil {
		return Settings{}, errors.New("persisted Pacer settings are invalid")
	}
	return s, nil
}

// decodeSettingsPatch applies a partial JSON object of settings to current.
func decodeSettingsPatch(body []byte, current Settings) (Settings, error) {
	if len(body) > 16<<10 {
		return current, errors.New("body too large")
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(body, &patch); err != nil || len(patch) == 0 {
		return current, errors.New("send a JSON object of settings")
	}
	fields, err := settingFields(current)
	if err != nil {
		return current, err
	}
	for name, value := range patch {
		if _, ok := fields[name]; !ok {
			return current, fmt.Errorf("unknown setting %q", name)
		}
		fields[name] = value
	}
	raw, _ := json.Marshal(fields)
	next := defaultSettings()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&next); err != nil || decoder.Decode(new(any)) != io.EOF {
		return current, errors.New("a setting has the wrong type")
	}
	if next.CompactionModels == nil {
		next.CompactionModels = []string{}
	}
	if next.PausedAccounts == nil {
		next.PausedAccounts = []string{}
	}
	return next, next.validate()
}

// setSettings stores the effective settings and applies them at once.
func (p *Plugin) setSettings(s Settings) error {
	fields, err := settingFields(s)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for name, value := range fields {
		if _, err := tx.Exec(`UPDATE pacer_settings SET effective=? WHERE name=?`, string(value), name); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	p.applySettingsLocked(s)
	return nil
}

func (p *Plugin) applySettingsLocked(s Settings) {
	p.settings = s
	p.cfg.CompactOnSwitch = s.CompactOnSwitch
	p.cfg.Routing.Providers = append([]string{}, s.Providers...)
}

// settingsView is the page's view: the settings, their seeds, and which were
// changed on the page.
func (p *Plugin) settingsViewLocked() map[string]any {
	seed := seedSettings(p.seedCfg)
	effective, _ := settingFields(p.settings)
	seeds, _ := settingFields(seed)
	changed := []string{}
	for name, value := range effective {
		if !bytes.Equal(value, seeds[name]) {
			changed = append(changed, name)
		}
	}
	return map[string]any{"settings": p.settings, "seed": seed, "changed": changed, "limits": map[string]any{"compaction_models": maxCompactionModels}}
}
