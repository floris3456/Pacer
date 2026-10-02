package bridge

import (
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/adaptive"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/handoff"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShadowSchedulerDelegatesAndRecordsActualSelection(t *testing.T) {
	p := New(nil)
	var err error
	p.Engine, err = handoff.Open(handoff.Config{StatePath: filepath.Join(t.TempDir(), "state.sqlite"), Adaptive: adaptive.Config{Mode: "shadow"}, Credentials: []handoff.Credential{{ID: "host-choice", Index: "1", Provider: "claude", Account: "small", Alias: "Small"}}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	pick, err := p.Handle(pluginabi.MethodSchedulerPick, []byte(`{}`))
	if err != nil || pick.(pluginapi.SchedulerPickResponse).Handled {
		t.Fatalf("shadow took scheduler: %+v %v", pick, err)
	}
	before, _ := json.Marshal(pluginapi.RequestInterceptRequest{RequestID: "request", Model: "light", Body: []byte(`{"input":"dummy"}`), Headers: http.Header{}})
	if _, err = p.Handle(pluginabi.MethodRequestInterceptBefore, before); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(pluginapi.RequestInterceptRequest{RequestID: "request", Model: "light", Metadata: map[string]any{"selected_auth_id": "host-choice"}})
	response, err := p.Handle(pluginabi.MethodRequestInterceptAfter, after)
	if err != nil || response.(pluginapi.RequestInterceptResponse).Terminate {
		t.Fatalf("shadow refused missing quota: %+v %v", response, err)
	}
	if p.requests["request"].Selected.Auth != "host-choice" {
		t.Fatal("actual selection not observed")
	}
	// A request already running when the plugin was loaded has no tracked state.
	after, _ = json.Marshal(pluginapi.RequestInterceptRequest{RequestID: "pre-existing", Metadata: map[string]any{"selected_auth_id": "host-choice"}})
	response, err = p.Handle(pluginabi.MethodRequestInterceptAfter, after)
	if err != nil || response.(pluginapi.RequestInterceptResponse).Terminate {
		t.Fatal("hot load interrupted an existing request")
	}
}

func TestBackendPrivateTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 48)), 0600); err != nil {
		t.Fatal(err)
	}
	c := adaptive.Config{Mode: "shadow", URL: "http://10.0.0.10:18317", TokenFile: path}
	if err := validateBackend(c); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if validateBackend(c) == nil {
		t.Fatal("accepted public token file")
	}
}
