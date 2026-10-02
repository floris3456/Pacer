package bridge

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/handoff"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// Omission preserves the existing behavior. Only an explicit false mutes delivery.
func (p *Plugin) configureNotifications(enabled *bool) {
	muted := enabled != nil && !*enabled
	previous := p.notificationsMuted.Swap(muted)
	if previous != muted {
		p.notificationLog("info", "Quota notification delivery changed; quota monitoring and logging remain active", map[string]any{
			"notifications_enabled": !muted,
		})
	}
}

// A threshold is logged once per event ID per plugin lifetime, not once per
// streamed token. Legacy IDs include account, window, quota cycle and severity;
// coordinated event IDs use the persisted global cursor. The bounded cache is
// intentionally separate from notice delivery and persistent quota state.
func (p *Plugin) logQuotaEvents(events []handoff.Event) {
	p.noticeLogMu.Lock()
	defer p.noticeLogMu.Unlock()
	for _, event := range events {
		if event.ID == "" || event.Text == "" || p.loggedNotices[event.ID] {
			continue
		}
		if p.loggedNotices == nil || len(p.loggedNotices) >= 4096 {
			p.loggedNotices = make(map[string]bool)
		}
		p.notificationLog("warning", event.Text, map[string]any{
			"quota_event": event.ID, "quota_kind": event.Kind,
			"notifications_enabled": !p.notificationsMuted.Load(),
		})
		p.loggedNotices[event.ID] = true
	}
}

func (p *Plugin) notificationLog(level, message string, fields map[string]any) {
	fields["plugin_id"] = "quota-handoff"
	// CPA's text formatter omits arbitrary structured fields. Keep the delivery
	// state in the message as well so the ordinary rotating log is self-contained.
	message = fmt.Sprintf("[quota-handoff] notifications_enabled=%v %s", fields["notifications_enabled"], message)
	record := map[string]any{"level": level, "message": message, "fields": fields}
	if p.Host != nil {
		if _, err := p.Host(pluginabi.MethodHostLog, record); err == nil {
			return
		}
	}
	// Logging must never prevent an admitted model response. Retain a structured
	// stderr record if the host logger is temporarily unavailable.
	data, _ := json.Marshal(record)
	log.Printf("quota-handoff: %s", data)
}
