package pacer

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func (p *Plugin) quotaAPI(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	index := req.Query.Get("auth_index")
	if req.Method == "POST" {
		if len(req.Body) > 4096 {
			return resource(413, map[string]string{"error": "body_too_large"})
		}
		var body struct {
			Index string `json:"auth_index"`
		}
		d := json.NewDecoder(bytes.NewReader(req.Body))
		d.DisallowUnknownFields()
		if d.Decode(&body) != nil || d.Decode(new(any)) != io.EOF {
			return resource(400, map[string]string{"error": "invalid_quota_request"})
		}
		index = body.Index
	}
	if index == "" || len(index) > 512 {
		return resource(400, map[string]string{"error": "invalid_auth_index"})
	}
	p.mu.Lock()
	var selected credential
	for _, c := range p.creds {
		if c.Index == index {
			selected = c
			break
		}
	}
	p.mu.Unlock()
	if selected.ID == "" {
		return resource(404, map[string]string{"error": "quota_credential_unavailable"})
	}
	// Attempt windows are a GET-only, read-only extension. POST reconcile never
	// reads these parameters, so its behaviour is unchanged.
	var window *attemptWindow
	if req.Method == "GET" {
		w, code := parseAttemptWindow(req.Query)
		if code != "" {
			return resource(400, map[string]string{"error": code})
		}
		window = w
	}
	if req.Method == "POST" {
		if err := p.observer.Queue(selected.Account, observation.Endpoint(selected.Provider), observation.ReasonManual, time.Time{}); err != nil {
			return resource(503, map[string]string{"error": "quota_schedule_unavailable"})
		}
	}
	windows, err := p.observer.ReadWindows(selected.Account)
	if err != nil {
		return resource(503, map[string]string{"error": "quota_state_unavailable"})
	}
	if windows == nil {
		windows = []observation.Window{}
	}
	job, _, err := p.observer.Status(selected.Account)
	if err != nil {
		return resource(503, map[string]string{"error": "quota_schedule_unavailable"})
	}
	body := map[string]any{"provider": selected.Provider, "now": p.now(), "windows": windows, "reconciliation": map[string]any{"next": job.Next, "blocked": job.Blocked, "reason": job.Reason, "state": job.State}}
	if window != nil {
		attempts, err := p.observer.Attempts(selected.Account, window.after, window.limit)
		if err != nil {
			return resource(503, map[string]string{"error": "quota_attempts_unavailable"})
		}
		// Always a JSON array, never null, so clients can page without nil checks.
		views := make([]quotaAttemptView, 0, len(attempts))
		for _, a := range attempts {
			views = append(views, quotaAttemptView{ID: a.ID, Endpoint: a.Endpoint, Reason: a.Reason, Started: a.Started, Finished: a.Finished, Status: a.Status})
		}
		body["attempts"] = views
		// A full page may have more rows; clients stop once a page is short.
		body["attempts_more"] = len(attempts) == window.limit
	}
	return resource(200, body)
}

const (
	attemptLimitDefault = 1000
	attemptLimitMax     = 1000
)

var (
	// Cursors are non-negative decimal IDs with no leading zeros. The digit cap
	// keeps the value within int64 range before ParseInt runs.
	attemptCursorPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,18})$`)
	attemptLimitPattern  = regexp.MustCompile(`^[1-9][0-9]{0,3}$`)
)

// attemptWindow is a validated page request: attempts with ID > after, at most limit.
type attemptWindow struct {
	after int64
	limit int
}

// parseAttemptWindow validates the optional attempt window. It returns nil with
// no error code when attempts_after is absent, which keeps the response shape
// unchanged. Otherwise it returns an error code for a 400 response.
func parseAttemptWindow(q url.Values) (*attemptWindow, string) {
	cursors, hasCursor := q["attempts_after"]
	limits, hasLimit := q["attempts_limit"]
	if !hasCursor {
		if hasLimit {
			return nil, "invalid_attempt_cursor"
		}
		return nil, ""
	}
	if len(cursors) != 1 || !attemptCursorPattern.MatchString(cursors[0]) {
		return nil, "invalid_attempt_cursor"
	}
	after, err := strconv.ParseInt(cursors[0], 10, 64)
	if err != nil {
		return nil, "invalid_attempt_cursor"
	}
	w := &attemptWindow{after: after, limit: attemptLimitDefault}
	if hasLimit {
		if len(limits) != 1 || !attemptLimitPattern.MatchString(limits[0]) {
			return nil, "invalid_attempt_limit"
		}
		// The pattern caps the value at 9999, so Atoi cannot overflow here.
		n, err := strconv.Atoi(limits[0])
		if err != nil || n > attemptLimitMax {
			return nil, "invalid_attempt_limit"
		}
		w.limit = n
	}
	return w, ""
}

// quotaAttemptView is the only attempt shape the management API exposes. It
// omits the account, retry hints and any credential or token material.
type quotaAttemptView struct {
	ID       int64     `json:"id"`
	Endpoint string    `json:"endpoint"`
	Reason   string    `json:"reason"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Status   int       `json:"status"`
}
