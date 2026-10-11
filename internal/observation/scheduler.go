package observation

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"time"
)

const (
	AccountMinGap = 15 * time.Minute
	// EndpointMinGap spaces reads of one provider endpoint across accounts.
	EndpointMinGap = 30 * time.Second
	// IdleInterval is the longest a meter goes unread: an idle account, use
	// outside CPA and a silent early reset show up within this time.
	IdleInterval = 30 * time.Minute
	// ActivityQuiet is how long after its last request an account whose meter
	// only a direct read reveals is read again.
	ActivityQuiet  = 5 * time.Minute
	MaxBackoff     = 6 * time.Hour
	ReasonSeed     = "seed"
	ReasonPartial  = "partial"
	ReasonDeadline = "deadline"
	ReasonIdle     = "idle"
	ReasonManual   = "manual"
	ReasonActivity = "activity"
)

// directOnly names providers whose ordinary responses carry no quota: only a
// direct read of their meter gives the balance and the reset.
func directOnly(provider string) bool {
	switch providerName(provider) {
	case "antigravity", "opencode-go":
		return true
	}
	return false
}

var ErrStaleJob = errors.New("observation: stale or unowned job token")

// Job is both a scheduler snapshot and (when acquired) a capability to Finish.
// Next includes all pacing/backoff floors. Inflight and suspended jobs have no
// finite next start; Blocked explains why. Due is the underlying requested time.
type Job struct {
	Account           string    `json:"account"`
	Endpoint          string    `json:"endpoint"`
	Reason            string    `json:"reason"`
	Due               time.Time `json:"due"`
	State             string    `json:"state"`
	Token             string    `json:"token,omitempty"`
	Started           time.Time `json:"started"`
	Joined            int64     `json:"joined"`
	Cancelled         int64     `json:"cancelled"`
	Next              time.Time `json:"next"`
	Blocked           string    `json:"blocked,omitempty"`
	AccountNotBefore  time.Time `json:"account_not_before"`
	EndpointNotBefore time.Time `json:"endpoint_not_before"`
	Failures          int       `json:"failures"`
	// Activity is the last request served by a direct-only account.
	Activity time.Time `json:"activity"`
}

type budget struct {
	last, notBefore time.Time
	failures        int
	suspended       bool
}

// Endpoint returns a shared budget identity, not a URL. Unknown providers need
// an explicitly registered adapter and its own stable endpoint budget key.
func Endpoint(provider string) string {
	switch providerName(provider) {
	case "claude":
		return "claude:usage"
	case "codex":
		return "codex:usage"
	case "antigravity":
		return "antigravity:quota"
	case "opencode-go":
		return "opencode-go:usage"
	default:
		return ""
	}
}

func endpointKey(endpoint string) (string, error) {
	endpoint = strings.ToLower(strings.TrimSpace(endpoint))
	if known := Endpoint(endpoint); known != "" {
		endpoint = known
	}
	if endpoint == "" || strings.ContainsAny(endpoint, "/?#\r\n\t ") {
		return "", errors.New("observation: endpoint must be a shared budget ID, never a URL")
	}
	return endpoint, nil
}

// jitter is stable for an input event. Repeated requests/restarts cannot keep
// resampling an earlier slot. Jitter never shortens a minimum/provider deadline.
func jitter(key string, at time.Time, min, max time.Duration) time.Duration {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key + at.UTC().Format(time.RFC3339Nano)))
	return min + time.Duration(h.Sum64()%uint64(max-min+1))
}

func queueDue(account, endpoint, reason string, due, now time.Time) time.Time {
	if due.IsZero() {
		due = now
	}
	switch reason {
	case ReasonSeed:
		return due.Add(jitter(account+endpoint+reason, due, time.Minute, 5*time.Minute))
	case ReasonDeadline:
		return due.Add(jitter(account+endpoint+reason, due, 30*time.Second, 90*time.Second))
	case ReasonIdle:
		return due.Add(IdleInterval)
	case ReasonActivity:
		return due.Add(ActivityQuiet)
	default:
		return due
	}
}

func getJob(c *sql.Conn, account string) (Job, bool, error) {
	var j Job
	var due, started, activity int64
	err := c.QueryRowContext(context.Background(), "SELECT account,endpoint,reason,due,state,token,started,joined,cancelled,activity FROM observation_jobs WHERE account=?", account).Scan(&j.Account, &j.Endpoint, &j.Reason, &due, &j.State, &j.Token, &started, &j.Joined, &j.Cancelled, &activity)
	if errors.Is(err, sql.ErrNoRows) {
		return j, false, nil
	}
	j.Due, j.Started, j.Activity = fromNS(due), fromNS(started), fromNS(activity)
	return j, err == nil, err
}

func getBudget(c *sql.Conn, kind, key string) (budget, error) {
	var b budget
	var last, notBefore int64
	err := c.QueryRowContext(context.Background(), "SELECT last_start,not_before,failures,suspended FROM observation_budgets WHERE kind=? AND key=?", kind, key).Scan(&last, &notBefore, &b.failures, &b.suspended)
	if errors.Is(err, sql.ErrNoRows) {
		return b, nil
	}
	b.last, b.notBefore = fromNS(last), fromNS(notBefore)
	return b, err
}

func putBudget(c *sql.Conn, kind, key string, b budget) error {
	_, err := c.ExecContext(context.Background(), `INSERT INTO observation_budgets(kind,key,last_start,not_before,failures,suspended) VALUES(?,?,?,?,?,?)
 ON CONFLICT(kind,key) DO UPDATE SET last_start=excluded.last_start,not_before=excluded.not_before,failures=excluded.failures,suspended=excluded.suspended`, kind, key, ns(b.last), ns(b.notBefore), b.failures, b.suspended)
	return err
}

// coverage requires Claude's known shared 5h AND 7d fields (explicit null counts).
// Codex legitimately has just a weekly primary. Scoped-only evidence cannot
// accidentally stand in for the main budget. A direct-only provider (no quota in
// its responses) is covered by its last read while that is fresh; a request it
// served after that read asks for one more read ActivityQuiet after the last
// such request. Unknown adapters are never claimed complete.
func coverage(windows []Window, now, activity time.Time) (bool, time.Time, string) {
	return coverageInterval(windows, now, activity, IdleInterval)
}

func coverageInterval(windows []Window, now, activity time.Time, interval time.Duration) (bool, time.Time, string) {
	if len(windows) == 0 {
		return false, time.Time{}, ReasonPartial
	}
	claude5, claude7, codex, direct := false, false, false, false
	var next, read time.Time
	reason := ReasonIdle
	for _, w := range windows {
		fresh := w.FreshComplete(now, interval)
		if !fresh && directOnly(w.Provider) {
			// A bucket with no running window may carry a balance and no reset.
			fresh = w.State(now) == "partial" && w.Remaining != nil && w.Reset.IsZero() && !w.Usage.At.After(now) && now.Sub(w.Usage.At) < interval
		}
		if !fresh {
			return false, time.Time{}, ReasonPartial
		}
		switch {
		case w.Provider == "claude":
			claude5 = claude5 || (w.Scope == "all" && w.Seconds == 18000)
			claude7 = claude7 || (w.Scope == "all" && w.Seconds == 604800)
		case w.Provider == "codex":
			codex = codex || w.Scope == "all"
		case directOnly(w.Provider):
			direct = true
		default:
			return false, time.Time{}, ReasonPartial
		}
		at := w.Usage.At
		if w.NotApplicable || w.Dormant {
			at = w.StatusEvidence.At
		}
		if at.After(read) {
			read = at
		}
		candidate := at.Add(interval)
		if next.IsZero() || candidate.Before(next) {
			next = candidate
			reason = ReasonIdle
		}
		if !w.Reset.IsZero() {
			deadline := w.Reset.Add(jitter(w.Key(), w.Reset, 30*time.Second, 90*time.Second))
			if deadline.Before(next) {
				next = deadline
				reason = ReasonDeadline
			}
		}
	}
	if direct && activity.After(read) {
		if quiet := activity.Add(ActivityQuiet); quiet.Before(next) {
			next, reason = quiet, ReasonActivity
		}
	}
	return (claude5 && claude7) || codex || direct, next, reason
}

// reconcileJob cancels satisfied work, retaining an idle/deadline wake-up. This
// is metadata only: continuous fresh passive signals keep postponing direct I/O.
func (s *Store) reconcileJob(c *sql.Conn, account string, now time.Time) error {
	j, exists, err := getJob(c, account)
	if err != nil || !exists || j.State == "inflight" {
		return err
	}
	windows, err := readWindows(c, account)
	if err != nil {
		return err
	}
	interval, err := pollingInterval(c, account)
	if err != nil {
		return err
	}
	complete, next, reason := coverageInterval(windows, now, j.Activity, interval)
	if complete {
		cancelled := j.Cancelled
		if j.State == "queued" {
			cancelled++
		}
		_, err = c.ExecContext(context.Background(), "UPDATE observation_jobs SET state='waiting',reason=?,due=?,cancelled=? WHERE account=?", reason, ns(next), cancelled, account)
		return err
	}
	// Manual callers cannot bypass the grace period just by changing the reason.
	due, reason := j.Due, j.Reason
	if j.State == "waiting" {
		due = now
		if now.Before(j.Due) && j.Reason != ReasonDeadline {
			reason = ReasonPartial
		}
	}
	for _, w := range windows {
		if !w.Reset.IsZero() && !w.Reset.After(now) {
			deadline := w.Reset.Add(jitter(w.Key(), w.Reset, 30*time.Second, 90*time.Second))
			if deadline.After(due) {
				due, reason = deadline, ReasonDeadline
			}
		}
	}
	if j.State == "waiting" || !due.Equal(j.Due) {
		_, err = c.ExecContext(context.Background(), "UPDATE observation_jobs SET state='queued',reason=?,due=? WHERE account=?", reason, ns(due), account)
	}
	return err
}

// Queue registers/coalesces one account's work. account is an opaque VERIFIED
// provider account ID supplied by the bridge, or an unmerged credential ID.
// due is the base event time: seed=startup, deadline=reported reset, idle=last
// evidence; the scheduler adds policy jitter/interval for those reasons.
func (s *Store) Queue(account, endpoint, reason string, due time.Time) error {
	if account == "" {
		return errors.New("observation: account required")
	}
	var err error
	endpoint, err = endpointKey(endpoint)
	if err != nil {
		return err
	}
	if reason == "" {
		reason = ReasonManual
	}
	return s.write(func(c *sql.Conn) error {
		now := s.clock().UTC()
		j, exists, err := getJob(c, account)
		if err != nil {
			return err
		}
		if exists && j.Endpoint != endpoint {
			return errors.New("observation: account already uses a different endpoint budget")
		}
		requested := queueDue(account, endpoint, reason, due, now)
		if !exists {
			_, err = c.ExecContext(context.Background(), "INSERT INTO observation_jobs(account,endpoint,reason,due,state) VALUES(?,?,?,?,'queued')", account, endpoint, reason, ns(requested))
		} else if j.State == "inflight" {
			_, err = c.ExecContext(context.Background(), "UPDATE observation_jobs SET joined=joined+1 WHERE account=?", account)
		} else {
			// Joining a seed cannot erase its initial jitter. More specific
			// missing-field work may advance the requested time, never budgets.
			if j.State == "queued" && (j.Due.Before(requested) || j.Reason == reason || j.Reason == ReasonSeed) {
				requested = j.Due
				reason = j.Reason
			}
			_, err = c.ExecContext(context.Background(), "UPDATE observation_jobs SET joined=joined+1,state='queued',reason=?,due=? WHERE account=?", reason, ns(requested), account)
		}
		if err != nil {
			return err
		}
		return s.reconcileJob(c, account, now)
	})
}

// Activity notes a request that a direct-only account served at the given time.
// Its meter is read once ActivityQuiet after the last such request, never
// sooner than the account and endpoint floors allow. Other providers report
// quota with their responses, so for them this is a no-op.
func (s *Store) Activity(account, provider string, at time.Time) error {
	if account == "" {
		return errors.New("observation: account required")
	}
	if !directOnly(provider) {
		return nil
	}
	endpoint, err := endpointKey(provider)
	if err != nil {
		return err
	}
	return s.write(func(c *sql.Conn) error {
		now := s.clock().UTC()
		if at.IsZero() || at.After(now) {
			at = now
		}
		j, exists, err := getJob(c, account)
		if err != nil {
			return err
		}
		if exists && j.Endpoint != endpoint {
			return errors.New("observation: account already uses a different endpoint budget")
		}
		if !exists {
			_, err = c.ExecContext(context.Background(), "INSERT INTO observation_jobs(account,endpoint,reason,due,state,activity) VALUES(?,?,?,?,'queued',?)", account, endpoint, ReasonActivity, ns(queueDue(account, endpoint, ReasonActivity, at, now)), ns(at))
		} else if at.After(j.Activity) {
			_, err = c.ExecContext(context.Background(), "UPDATE observation_jobs SET activity=? WHERE account=?", ns(at), account)
		}
		if err != nil {
			return err
		}
		return s.reconcileJob(c, account, now)
	})
}

func ready(c *sql.Conn, j Job) (Job, error) {
	a, err := getBudget(c, "account", j.Account)
	if err != nil {
		return j, err
	}
	e, err := getBudget(c, "endpoint", j.Endpoint)
	if err != nil {
		return j, err
	}
	j.AccountNotBefore, j.EndpointNotBefore, j.Failures = a.notBefore, e.notBefore, a.failures
	if j.State == "inflight" {
		j.Blocked = "account-inflight"
		return j, nil
	}
	if a.suspended {
		j.Blocked = "authentication"
		return j, nil
	}
	var inflight int
	if err := c.QueryRowContext(context.Background(), "SELECT count(*) FROM observation_jobs WHERE endpoint=? AND state='inflight'", j.Endpoint).Scan(&inflight); err != nil {
		return j, err
	}
	if inflight != 0 {
		j.Blocked = "endpoint-inflight"
		return j, nil
	}
	j.Next = j.Due
	floors := []struct {
		at  time.Time
		why string
	}{{a.notBefore, "account-backoff"}, {e.notBefore, "endpoint-backoff"}}
	if !a.last.IsZero() {
		floors = append(floors, struct {
			at  time.Time
			why string
		}{a.last.Add(AccountMinGap), "account-spacing"})
	}
	if !e.last.IsZero() {
		floors = append(floors, struct {
			at  time.Time
			why string
		}{e.last.Add(EndpointMinGap + jitter(j.Endpoint, e.last, 0, 5*time.Second)), "endpoint-spacing"})
	}
	for _, floor := range floors {
		if floor.at.After(j.Next) {
			j.Next, j.Blocked = floor.at, floor.why
		}
	}
	return j, nil
}

func (s *Store) Status(account string) (Job, bool, error) {
	var j Job
	var exists bool
	err := s.write(func(c *sql.Conn) error {
		var err error
		j, exists, err = getJob(c, account)
		if err != nil || !exists {
			return err
		}
		j, err = ready(c, j)
		return err
	})
	return j, exists, err
}

// Acquire atomically checks the injected clock, freshness, both request budgets,
// and both inflight gates. nil means no start is permitted; consult Status.
// Only a nonnil returned job authorizes the bridge to call its host quota API.
func (s *Store) Acquire(account string) (*Job, error) {
	var acquired *Job
	err := s.write(func(c *sql.Conn) error {
		now := s.clock().UTC()
		if err := s.reconcileJob(c, account, now); err != nil {
			return err
		}
		j, exists, err := getJob(c, account)
		if err != nil || !exists {
			return err
		}
		j, err = ready(c, j)
		if err != nil {
			return err
		}
		if j.Next.IsZero() || now.Before(j.Next) || j.State == "inflight" {
			return nil
		}
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			return err
		}
		j.Token, j.Started, j.State, j.Blocked = hex.EncodeToString(buf), now, "inflight", ""
		if _, err := c.ExecContext(context.Background(), "UPDATE observation_jobs SET state='inflight',token=?,started=? WHERE account=?", j.Token, ns(now), account); err != nil {
			return err
		}
		for _, pair := range [][2]string{{"account", account}, {"endpoint", j.Endpoint}} {
			b, err := getBudget(c, pair[0], pair[1])
			if err != nil {
				return err
			}
			b.last = now
			if err := putBudget(c, pair[0], pair[1], b); err != nil {
				return err
			}
		}
		if _, err := c.ExecContext(context.Background(), "INSERT INTO observation_attempts(token,account,endpoint,reason,started) VALUES(?,?,?,?,?)", j.Token, account, j.Endpoint, j.Reason, ns(now)); err != nil {
			return err
		}
		acquired = &j
		return nil
	})
	return acquired, err
}

// Start is an alias for Acquire; neither method enqueues or performs network I/O.
func (s *Store) Start(account string) (*Job, error) { return s.Acquire(account) }

// RetryAfter accepts delta seconds (including fractional seconds) and HTTP-date.
// It does not cap a provider's deadline. Invalid/past dates are not usable advice.
func RetryAfter(h http.Header, now time.Time) (time.Time, bool) {
	raw := headersLower(h)["retry-after"]
	if t := relative(raw, now); !t.IsZero() {
		return t, true
	}
	if t, err := http.ParseTime(raw); err == nil && !t.Before(now) {
		return t.UTC(), true
	}
	return time.Time{}, false
}

func fallback(key string, failures int, now time.Time) time.Time {
	d := AccountMinGap
	for i := 1; i < failures && d < MaxBackoff; i++ {
		d *= 2
	}
	if d >= MaxBackoff {
		return now.Add(MaxBackoff)
	}
	d += jitter(key, now, 0, d/10)
	if d > MaxBackoff {
		d = MaxBackoff
	}
	return now.Add(d)
}

// Finish records the actual analytics status/headers. Call Observe with parsed
// quota evidence before Finish. A 2xx without usable data does not imply capacity.
// status=0 denotes transport failure or an explicitly recovered interrupted call.
func (s *Store) Finish(job Job, status int, headers http.Header) error {
	return s.write(func(c *sql.Conn) error {
		now := s.clock().UTC()
		j, exists, err := getJob(c, job.Account)
		if err != nil {
			return err
		}
		if !exists || j.State != "inflight" || j.Token == "" || j.Token != job.Token || j.Endpoint != job.Endpoint {
			return ErrStaleJob
		}
		a, err := getBudget(c, "account", j.Account)
		if err != nil {
			return err
		}
		e, err := getBudget(c, "endpoint", j.Endpoint)
		if err != nil {
			return err
		}
		success := status >= 200 && status < 300
		due := now.Add(AccountMinGap)
		reason := ReasonPartial
		if success {
			a.failures, e.failures = 0, 0
			windows, err := readWindows(c, j.Account)
			if err != nil {
				return err
			}
			if len(windows) == 0 {
				due, reason = now.Add(IdleInterval), ReasonIdle
			}
		} else if status == http.StatusUnauthorized || status == http.StatusForbidden {
			a.suspended = true
		} else {
			a.failures++
			e.failures++
			due = latest(fallback(j.Account, a.failures, now), fallback(j.Endpoint, e.failures, now))
			if retry, ok := RetryAfter(headers, now); ok {
				due = latest(due, retry)
			}
			a.notBefore, e.notBefore = latest(a.notBefore, due), latest(e.notBefore, due)
		}
		if err := putBudget(c, "account", j.Account, a); err != nil {
			return err
		}
		if err := putBudget(c, "endpoint", j.Endpoint, e); err != nil {
			return err
		}
		if _, err := c.ExecContext(context.Background(), "UPDATE observation_attempts SET finished=?,status=?,retry_after=? WHERE token=?", ns(now), status, headersLower(headers)["retry-after"], j.Token); err != nil {
			return err
		}
		if _, err := c.ExecContext(context.Background(), "UPDATE observation_jobs SET state='queued',token='',due=?,reason=? WHERE account=?", ns(due), reason, j.Account); err != nil {
			return err
		}
		return s.reconcileJob(c, j.Account, now)
	})
}

// Resume is for explicit credential recovery, not UI refresh. Backoff and start
// floors survive recovery. The bridge must first establish valid credentials.
func (s *Store) Resume(account string) error {
	return s.write(func(c *sql.Conn) error {
		_, err := c.ExecContext(context.Background(), "UPDATE observation_budgets SET suspended=0 WHERE kind='account' AND key=?", account)
		return err
	})
}

// RecoverInterrupted must be called ONLY after the owner knows an old process's
// transport has stopped. Open never expires an inflight lease and risks overlap.
// Persisted tokens fence late Finish calls after recovery/new acquisition.
func (s *Store) RecoverInterrupted(job Job) error { return s.Finish(job, 0, nil) }

// Jobs lists cached scheduler state. Polling it performs no upstream work.
func (s *Store) Jobs() ([]Job, error) {
	var out []Job
	err := s.write(func(c *sql.Conn) error {
		rows, err := c.QueryContext(context.Background(), "SELECT account FROM observation_jobs ORDER BY due,account")
		if err != nil {
			return err
		}
		var accounts []string
		for rows.Next() {
			var account string
			if err := rows.Scan(&account); err != nil {
				rows.Close()
				return err
			}
			accounts = append(accounts, account)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, account := range accounts {
			j, _, err := getJob(c, account)
			if err != nil {
				return err
			}
			j, err = ready(c, j)
			if err != nil {
				return err
			}
			out = append(out, j)
		}
		return nil
	})
	return out, err
}

func (j Job) String() string {
	return fmt.Sprintf("%s %s %s next=%s blocked=%s", j.Account, j.Endpoint, j.State, j.Next.Format(time.RFC3339Nano), j.Blocked)
}
