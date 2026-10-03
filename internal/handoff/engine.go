package handoff

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/adaptive"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/quota"
	"sort"
	"strings"
	"sync"
	"time"
)

type Credential struct {
	ID       string `yaml:"auth_id" json:"id"`
	Index    string `yaml:"auth_index" json:"index"`
	Provider string `yaml:"provider" json:"provider"`
	Account  string `yaml:"account" json:"account"`
	Alias    string `yaml:"alias" json:"alias"`
	Rank     int    `yaml:"rank" json:"rank"`
}
type Config struct {
	UsageAuthURL string          `yaml:"usage_auth_url"`
	Adaptive     adaptive.Config `yaml:"adaptive"`
	StatePath    string          `yaml:"state_path"`
	Credentials  []Credential    `yaml:"credentials"`
	MaxAge       int64           `yaml:"max_age_seconds"`
	// DiscoverCredentials follows CPA's enabled Claude/Codex auths; configured
	// credentials then only override alias, rank and account seed.
	DiscoverCredentials bool `yaml:"discover_credentials"`
}
type Claim struct {
	Route        string `json:"route"`
	Protocol     int    `json:"protocol"`
	Installation string `json:"installation"`
	Session      string `json:"session"`
	Request      string `json:"request"`
	Kind         string `json:"kind"`
	Capability   string `json:"capability"` // SHA-256 digest; the raw capability never enters CPA inference logs.
	Auto         bool   `json:"auto"`
	Fresh        bool   `json:"fresh"`
	Portable     bool   `json:"portable"`
	Checkpoint   string `json:"checkpoint,omitempty"`
	Generation   int    `json:"generation"`
	Intent       string `json:"intent,omitempty"`
}

func (c Claim) Valid() bool {
	if c.Protocol != 1 || c.Installation == "" || c.Session == "" || c.Route == "" || c.Request == "" || len(c.Capability) != 64 {
		return false
	}
	for _, s := range []string{c.Installation, c.Session, c.Request, c.Route, c.Checkpoint, c.Intent} {
		if len(s) > 256 {
			return false
		}
	}
	if _, err := hex.DecodeString(c.Capability); err != nil {
		return false
	}
	return c.Kind == "primary" || c.Kind == "compaction" || c.Kind == "title" || c.Kind == "generate"
}

type Account struct {
	Samples map[string]quota.Window `json:"samples,omitempty"`
	Windows map[string]quota.Window `json:"windows"`
}
type Event struct {
	Cursor     int64  `json:"cursor"`
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Text       string `json:"text"`
	Generation int    `json:"generation"`
}
type Binding struct {
	UsageAuth    string                      `json:"usage_auth,omitempty"`
	UsageAt      int64                       `json:"usage_at,omitempty"`
	SummaryReady bool                        `json:"summary_ready,omitempty"`
	Expected     adaptive.Request            `json:"expected"`
	AdaptiveSeen map[string]bool             `json:"adaptive_seen,omitempty"`
	Limits       map[string]int64            `json:"adaptive_limits,omitempty"`
	Departed     map[string]map[string]int64 `json:"departed,omitempty"`
	Route        string                      `json:"route"`
	PrimaryUsed  bool                        `json:"primary_used"`
	QuotaHeld    bool                        `json:"quota_held"`
	Key          string                      `json:"key"`
	Capability   string                      `json:"capability"`
	Provider     string                      `json:"provider"`
	Model        string                      `json:"model"`
	Account      string                      `json:"account"`
	Auth         string                      `json:"auth"`
	Alias        string                      `json:"alias"`
	Generation   int                         `json:"generation"`
	Intent       string                      `json:"intent"`
	Consumed     string                      `json:"consumed"`
	Auto         bool                        `json:"auto"`
	Last         int64                       `json:"last"`
	Reserve      int                         `json:"reserve"`
	Events       []Event                     `json:"events"`
	Seen         map[string]bool             `json:"seen"`
}
type State struct {
	Adaptive   *adaptive.State     `json:"adaptive,omitempty"`
	Identities map[string]string   `json:"identities"`
	Accounts   map[string]Account  `json:"accounts"`
	Bindings   map[string]*Binding `json:"bindings"`
	Cursor     int64               `json:"cursor"`
}
type Flight struct {
	Admission                 string
	Key, Account, Auth, Alias string
	Generation                int
	Kind                      string
}
type Engine struct {
	usageStarted map[string]bool
	pending      *adaptive.Request
	pendingKey   string
	mu           sync.Mutex
	cfg          Config
	static       []Credential
	db           *sql.DB
	state        State
	now          func() time.Time
	broken       error
	flights      map[string]Flight
}
type Refusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (r *Refusal) Error() string           { return r.Code + ": " + r.Message }
func refuse(code, message string) *Refusal { return &Refusal{code, message} }

type Snapshot struct {
	Mode        string         `json:"mode,omitempty"`
	PrimaryUsed bool           `json:"primary_used"`
	Consumed    string         `json:"consumed"`
	Protocol    int            `json:"protocol"`
	Cursor      int64          `json:"cursor"`
	Generation  int            `json:"generation"`
	Alias       string         `json:"alias"`
	Intent      string         `json:"intent"`
	Available   bool           `json:"available"`
	Windows     []quota.Window `json:"windows"`
	Events      []Event        `json:"events"`
}

func Open(cfg Config, now func() time.Time) (*Engine, error) {
	cfg.Adaptive.Defaults()
	if cfg.Adaptive.Mode != "off" && !cfg.Adaptive.Enabled() {
		return nil, errors.New("adaptive mode must be off, shadow or enforce")
	}
	if !cfg.Adaptive.Valid() {
		return nil, errors.New("invalid adaptive reserve settings")
	}
	if cfg.StatePath == "" || (len(cfg.Credentials) == 0 && !cfg.DiscoverCredentials) {
		return nil, errors.New("state_path and credentials (or discover_credentials) are required")
	}
	if cfg.MaxAge == 0 {
		cfg.MaxAge = 90
	}
	ids := map[string]bool{}
	for _, c := range cfg.Credentials {
		if c.ID == "" || c.Index == "" || c.Account == "" || c.Alias == "" || (c.Provider != "claude" && c.Provider != "codex") || ids[c.ID] {
			return nil, errors.New("credentials need unique auth_id, auth_index, provider, stable account and alias")
		}
		ids[c.ID] = true
	}
	db, state, err := openStore(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	for i, c := range cfg.Credentials {
		if known := state.Identities[c.ID]; known != "" {
			cfg.Credentials[i].Account = known
		}
	}
	if state.Adaptive == nil {
		state.Adaptive = adaptive.NewState()
	}
	for _, ticket := range state.Adaptive.Tickets {
		if ticket.Completed == 0 {
			ticket.Orphan = true
		}
	}
	static := append([]Credential(nil), cfg.Credentials...)
	return &Engine{cfg: cfg, static: static, db: db, state: state, now: now, flights: map[string]Flight{}}, nil
}
func (e *Engine) Close() error    { e.mu.Lock(); defer e.mu.Unlock(); return e.db.Close() }
func Digest(s string) string      { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func account(c Credential) string { return c.Provider + ":" + c.Account }
func (e *Engine) Credentials() []Credential {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Credential(nil), e.cfg.Credentials...)
}
func (e *Engine) credential(id string) (Credential, bool) {
	for _, c := range e.cfg.Credentials {
		if c.ID == id {
			return c, true
		}
	}
	return Credential{}, false
}

func (e *Engine) Observe(auth string, windows []quota.Window) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.broken != nil {
		return e.broken
	}
	c, ok := e.credential(auth)
	if !ok {
		return nil
	}
	key := account(c)
	a := e.state.Accounts[key]
	if a.Windows == nil {
		a.Windows = map[string]quota.Window{}
	}
	confirmed := false
	for _, w := range windows {
		if w.Valid() {
			if w.Source == "api" {
				if a.Samples == nil {
					a.Samples = map[string]quota.Window{}
				}
				if w.Observed >= a.Samples[w.Key()].Observed {
					a.Samples[w.Key()] = w
				}
			}
			confirmed = confirmed || w.Observed >= a.Windows[w.Key()].Observed
			a.Windows[w.Key()] = quota.Merge(a.Windows[w.Key()], w)
		}
	}
	e.state.Accounts[key] = a
	e.trimAdaptive(key)
	for _, b := range e.state.Bindings {
		if b.Account == key {
			e.reconcile(b, confirmed)
		}
	}
	return e.save()
}
func (e *Engine) windows(key, model string) []quota.Window {
	result := []quota.Window{}
	for _, w := range e.state.Accounts[key].Windows {
		if w.Applies(model) || w.Scope == "oauth" {
			result = append(result, w)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key() < result[j].Key() })
	return result
}
func (e *Engine) eligible(c Credential, model string, legacy bool) bool {
	windows := e.windows(account(c), model)
	// Both shared windows must be known. An absent or stale provider value isn't capacity.
	shared := 0
	for _, w := range windows {
		if (w.Held && !e.adaptiveWindow(c, model, w)) || (!w.NotApplicable && w.Remaining <= 0) || e.now().Unix()-w.Observed > e.cfg.MaxAge || (w.Reset > 0 && e.now().Unix() >= w.Reset) || (legacy && w.Reclaimed) {
			return false
		}
		if w.Scope == "all" {
			shared++
		}
	}
	return shared == 2 && e.adaptiveEligible(c, model)
}
func (e *Engine) emit(b *Binding, kind, text string) {
	e.state.Cursor++
	b.Events = append(b.Events, Event{e.state.Cursor, fmt.Sprintf("quota-%d", e.state.Cursor), kind, text, b.Generation})
	if len(b.Events) > 128 {
		b.Events = b.Events[len(b.Events)-128:]
	}
}
func (e *Engine) reconcile(b *Binding, confirmed bool) {
	if e.cfg.Adaptive.Shadow() {
		e.reconcileShadow(b)
		return
	}
	if e.reconcileAdaptive(b) {
		return
	}
	messages := []string{}
	urgent := false
	if b.Seen == nil {
		b.Seen = map[string]bool{}
	}
	// Retain deduplication across handoffs, bounded to each account's current cycles.
	valid := map[string]bool{}
	for account, a := range e.state.Accounts {
		for _, w := range a.Windows {
			for _, level := range []string{"warning", "handoff"} {
				valid[fmt.Sprintf("%s:%s:%d:%s", account, w.Key(), w.Cycle, level)] = true
			}
		}
	}
	for id := range b.Seen {
		if !valid[id] {
			delete(b.Seen, id)
		}
	}
	for _, w := range e.windows(b.Account, b.Model) {
		if w.NotApplicable {
			continue
		}
		if w.Held {
			urgent = true
		}
		if !w.Warned && !w.Held {
			continue
		}
		level := "warning"
		if w.Held {
			level = "handoff"
		}
		id := fmt.Sprintf("%s:%s:%d:%s", b.Account, w.Key(), w.Cycle, level)
		if b.Seen[id] {
			continue
		}
		b.Seen[id] = true
		label := "five-hour"
		threshold := 10
		if w.Seconds == 604800 {
			label = "seven-day"
			threshold = 5
		}
		text := fmt.Sprintf("Credential %s has %.1f%% of its %s allowance remaining (resets %s).", b.Alias, w.Remaining, label, time.Unix(w.Reset, 0).UTC().Format(time.RFC3339))
		if !w.Held {
			text += fmt.Sprintf(" At %d%%, another notice will arrive and this chat will compact if automatic compaction is enabled, then switch credentials.", threshold)
		}
		messages = append(messages, text)
	}
	if urgent && b.Intent == "" {
		b.Intent = Digest(fmt.Sprintf("%s:%d:%d", b.Key, b.Generation, e.state.Cursor))
		b.Reserve = 0
	}
	if urgent {
		b.QuotaHeld = true
	} else if b.Intent != "" && (b.QuotaHeld || confirmed) {
		// A chat with no replacement may still be on its original account when
		// that account resets or its stale evidence becomes fresh. Resuming it is
		// not a switch to another account. Empty usage samples prove no recovery.
		if c, ok := e.credential(b.Auth); ok && account(c) == b.Account && e.eligible(c, b.Model, false) {
			b.Intent = ""
			b.Reserve = 0
			b.QuotaHeld = false
			e.emit(b, "recovered", fmt.Sprintf("Credential %s has provider-confirmed quota again. The next request will recheck availability on this chat's existing binding.", b.Alias))
		}
	}
	if len(messages) > 0 {
		kind := "warning"
		if urgent {
			kind = "handoff"
			if b.Auto {
				messages = append(messages, "The current request will finish, then a portable compaction and credential handoff are required.")
			} else {
				messages = append(messages, "Automatic compaction is off. The credential will change at the next request boundary with portable context.")
			}
		}
		e.emit(b, kind, strings.Join(messages, " "))
	}
}
func (e *Engine) choose(candidates []string, provider, model, exclude string, legacy bool) *Credential {
	choices := []Credential{}
	for _, id := range candidates {
		c, ok := e.credential(id)
		if ok && c.Provider == provider && account(c) != exclude && e.eligible(c, model, legacy) {
			choices = append(choices, c)
		}
	}
	score := func(c Credential) (bool, float64, int) {
		warned := false
		weekly := 100.0
		allocations := 0
		for _, w := range e.windows(account(c), model) {
			warned = warned || (w.Warned && !e.adaptiveWindow(c, model, w))
			if w.Seconds == 604800 && w.Remaining < weekly {
				weekly = w.Remaining
			}
		}
		for _, b := range e.state.Bindings {
			if b.Account == account(c) && e.now().Unix()-b.Last < 300 {
				allocations++
			}
		}
		return warned, weekly, allocations
	}
	sort.Slice(choices, func(i, j int) bool {
		a, b := choices[i], choices[j]
		wa, qa, na := score(a)
		wb, qb, nb := score(b)
		if wa != wb {
			return !wa
		}
		if a.Rank != b.Rank {
			return a.Rank < b.Rank
		}
		if qa != qb {
			return qa > qb
		}
		if na != nb {
			return na < nb
		}
		return a.ID < b.ID
	})
	if len(choices) == 0 {
		return nil
	}
	return &choices[0]
}

// Pick runs under one lock: the binding, one-use checkpoint and admission record
// advance together before any upstream execution. In-flight work retains its auth.
func (e *Engine) pick(caller, provider, model string, candidates []string, claim *Claim) (Flight, error) {
	if e.broken != nil {
		return Flight{}, refuse("quota_state_unavailable", "Quota state could not be persisted")
	}
	if claim == nil {
		// Unintegrated clients accept cache loss: provider-confirmed reset
		// capacity is reusable without a chat/checkpoint handshake.
		c := e.choose(candidates, provider, model, "", false)
		if c == nil {
			return Flight{}, refuse("quota_no_capacity", "No verified credential capacity is available")
		}
		return Flight{Auth: c.ID, Account: account(*c), Alias: c.Alias}, nil
	}
	q := *claim
	if caller == "" || !q.Valid() {
		return Flight{}, refuse("quota_protocol_invalid", "Invalid quota coordination identity")
	}
	key := Digest(caller + "\x00" + q.Installation + "\x00" + q.Session + "\x00" + provider + "\x00" + q.Route)
	admission := key + ":" + q.Request
	if f, ok := e.flights[admission]; ok {
		return f, nil
	}
	b := e.state.Bindings[key]
	if b != nil && b.Capability != q.Capability {
		return Flight{}, refuse("quota_identity_conflict", "This chat is already registered with another capability")
	}
	if q.Kind == "primary" && (b == nil || !b.PrimaryUsed) && !q.Portable {
		return Flight{}, refuse("quota_checkpoint_required", "The first primary request requires portable context, even if auxiliary work already registered this chat")
	}
	if b == nil {
		c := e.choose(candidates, provider, model, "", !q.Fresh && q.Checkpoint == "")
		if c == nil {
			return Flight{}, refuse("quota_no_capacity", "No compatible credential has verified quota above the reserve")
		}
		b = &Binding{Route: q.Route, Key: key, Capability: q.Capability, Provider: provider, Model: model, Auth: c.ID, Account: account(*c), Alias: c.Alias, Generation: 1, Seen: map[string]bool{}}
		e.state.Bindings[key] = b
	} else if q.Kind == "primary" && q.Generation != b.Generation {
		return Flight{}, refuse("quota_generation_changed", "Refresh the chat binding before continuing")
	}
	b.Auto = q.Auto
	b.Last = e.now().Unix()
	if e.pending != nil && q.Kind != "compaction" {
		b.Expected = *e.pending
	}
	b.Model = model
	e.reconcile(b, false)
	current, currentOK := e.credential(b.Auth)
	present := false
	for _, id := range candidates {
		if id == b.Auth {
			present = true
		}
	}
	if !present {
		for _, id := range candidates {
			if alias, ok := e.credential(id); ok && account(alias) == b.Account {
				b.Auth = alias.ID
				current = alias
				currentOK = true
				present = true
				break
			}
		}
	}
	b.Model = model
	available := currentOK && present && account(current) == b.Account && e.eligible(current, model, false)
	boundary := q.Portable && ((q.Checkpoint != "" && q.Checkpoint != b.Consumed) || (q.Fresh && !b.PrimaryUsed))
	if q.Kind == "compaction" {
		// Each chat may compact immediately, including alongside other chats on
		// this account. Keep its binding until its own checkpoint is acknowledged.
		safe := currentOK && present && account(current) == b.Account
		for _, w := range e.windows(b.Account, model) {
			if (!w.NotApplicable && w.Remaining <= 0) || e.now().Unix()-w.Observed > e.cfg.MaxAge {
				safe = false
			}
		}
		if safe && !e.adaptiveEligible(current, model) {
			safe = false
		}
		if b.Intent != "" && b.Reserve >= 4 {
			return Flight{}, refuse("quota_compaction_failed", "Compaction reserve attempts exhausted; resume manually after reviewing capacity")
		}
		c := &current
		if !safe {
			c = e.choose(candidates, provider, model, b.Account, false)
		}
		if c == nil {
			return Flight{}, refuse("quota_no_capacity", "No credential can complete the portable summary")
		}
		if b.Intent != "" {
			b.Reserve++
		}
		f := Flight{admission, key, account(*c), c.ID, c.Alias, b.Generation, q.Kind}
		e.flights[admission] = f
		if err := e.save(); err != nil {
			return Flight{}, err
		}
		return f, nil
	}
	if q.Kind != "primary" && !available {
		return Flight{}, refuse("quota_auxiliary_paused", "Optional work paused while credential capacity is low")
	}
	if q.Kind == "primary" {
		if !available && currentOK && present && e.adaptiveWaitable(b) {
			return Flight{}, refuse("quota_budget_wait", "Waiting for running requests to settle their shared quota reservations")
		}
		need := b.Intent != "" || !available
		if need && b.Intent == "" {
			b.Intent = Digest(fmt.Sprintf("%s:%d:unavailable", key, b.Generation))
			e.emit(b, "handoff", "The bound credential is unavailable; a safe handoff is required.")
		}
		if need && (!q.Portable || (q.Auto && !boundary) || q.Intent != b.Intent) {
			if err := e.save(); err != nil {
				return Flight{}, err
			}
			return Flight{}, refuse("quota_checkpoint_required", "Credential handoff requires a fresh portable checkpoint or an explicit auto-off boundary")
		}
		// A fresh flag is only meaningful on the first binding; it cannot be reused.
		if need || boundary {
			exclude := ""
			if need {
				exclude = b.Account
			}
			next := e.choose(candidates, provider, model, exclude, false)
			if next == nil && need {
				e.emit(b, "paused", "No compatible replacement credential is available. Generation is paused until capacity returns.")
				_ = e.save()
				return Flight{}, refuse("quota_no_capacity", "No compatible replacement credential is available")
			}
			if next != nil && account(*next) != b.Account {
				for _, f := range e.flights {
					if f.Key == key {
						return Flight{}, refuse("quota_request_active", "Another request for this chat must finish before handoff")
					}
				}
				old := b.Alias
				e.rememberDeparture(b)
				b.Auth = next.ID
				b.Account = account(*next)
				b.Alias = next.Alias
				b.Generation++
				b.Intent = ""
				b.QuotaHeld = false
				b.Reserve = 0
				e.emit(b, "switched", fmt.Sprintf("Switched this chat from %s to %s.", old, b.Alias))
				e.reconcile(b, false)
			}
		}
		b.PrimaryUsed = true
		b.SummaryReady = false
		if q.Checkpoint != "" {
			b.Consumed = q.Checkpoint
		}
	}
	f := Flight{admission, key, b.Account, b.Auth, b.Alias, b.Generation, q.Kind}
	e.flights[admission] = f
	if err := e.save(); err != nil {
		return Flight{}, err
	}
	return f, nil
}
func (e *Engine) Complete(request string, succeeded ...bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.usageStarted, request)
	delete(e.flights, request)
	success := len(succeeded) == 0 || succeeded[0]
	e.completeAdaptive(request, success)
}
func (e *Engine) Snapshot(capability string, after int64, model string) (Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.broken != nil {
		return Snapshot{}, e.broken
	}
	hash := Digest(capability)
	// A capability is isolated per session and route; model selects only within it.
	for _, b := range e.state.Bindings {
		if b.Capability != hash || b.Route != model {
			continue
		}
		if e.cfg.Adaptive.Enabled() {
			b.Last = e.now().Unix()
			e.reconcile(b, false)
			if err := e.save(); err != nil {
				return Snapshot{}, err
			}
		}
		events := []Event{}
		for _, v := range b.Events {
			if v.Cursor > after {
				events = append(events, v)
			}
		}
		candidates := []string{}
		for _, c := range e.cfg.Credentials {
			candidates = append(candidates, c.ID)
		}
		previousRequest, previousKey := e.pending, e.pendingKey
		if e.cfg.Adaptive.Enforce() {
			r := b.Expected
			e.pending = &r
			e.pendingKey = b.Key
		}
		available := e.choose(candidates, b.Provider, b.Model, b.Account, false) != nil
		e.pending, e.pendingKey = previousRequest, previousKey
		return Snapshot{e.cfg.Adaptive.Mode, b.PrimaryUsed, b.Consumed, 1, e.state.Cursor, b.Generation, b.Alias, b.Intent, available, e.windows(b.Account, b.Model), events}, nil
	}
	return Snapshot{}, refuse("quota_unknown_session", "Session capability is not registered")
}
func (e *Engine) MarshalState() ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return json.Marshal(e.state)
}

func (e *Engine) Credential(id string) (Credential, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.credential(id)
}
