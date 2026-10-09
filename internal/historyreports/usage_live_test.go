package historyreports

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

const liveSource = "pacer-src"

func liveEvent(n int, account, credential, model string, at time.Time, input, output int64) ForwardEvent {
	id := fmt.Sprintf("native-%s-%d", account, n)
	return ForwardEvent{ID: id, Origin: "native", SourceID: liveSource, StartedAt: at, EndedAt: at.Add(time.Second), Final: true,
		Attribution: measurement.Attribution{Provider: "claude", AccountID: account, CredentialRef: credential, Model: model},
		Event: cpamp.Event{Hash: id, Account: account, Provider: "claude", Model: model, Started: at.UnixMilli(), Completed: at.Add(time.Second).UnixMilli(), Recorded: at.Add(time.Second).UnixMilli(), AccountingVersion: 2, Quality: "complete",
			Tokens: cpamp.Tokens{Input: input, Output: output, Total: input + output}}}
}

func liveReading(id int64, account string, seconds int64, reset, at time.Time, used float64) NativeReading {
	remaining := 100 - used
	return NativeReading{ID: id, Account: account, Provider: "claude", Scope: "all", Seconds: seconds, Remaining: &remaining, Reset: reset, At: at, Source: "header"}
}

func windowsByKey(q *cpamp.QuotaFacts) map[string]cpamp.FactWindow {
	out := map[string]cpamp.FactWindow{}
	for _, w := range q.Windows {
		out[w.Account+"|"+w.Base.ID] = w
	}
	return out
}

func sameJSON(t *testing.T, a, b any, label string) {
	t.Helper()
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if string(x) != string(y) {
		t.Fatalf("%s differs:\n%s\n%s", label, x, y)
	}
}
