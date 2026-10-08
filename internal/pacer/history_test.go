package pacer

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/history"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestManagementMetadataEditsHaveCASAndNoPublicResourceAlias(t *testing.T) {
	p := startPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"))
	defer p.Close()
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	p.history = store
	edit := func(path string, body any) pluginapi.ManagementResponse {
		raw, _ := json.Marshal(body)
		return p.manage(pluginapi.ManagementRequest{Method: "PATCH", Path: path, Body: raw})
	}
	path := "/v0/management/plugins/pacer/keys"
	hash := strings.Repeat("a", 64)
	add := map[string]any{"action": "add-key", "revision": 0, "hash": hash, "name": "Offline laptop"}
	if r := edit("/v0/resource/plugins/pacer/keys", add); r.StatusCode != 404 {
		t.Fatal("public metadata mutation exposed")
	}
	if r := edit(path, add); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	if r := edit(path, add); r.StatusCode != 409 {
		t.Fatal("stale revision accepted")
	}
	if r := edit(path, map[string]any{"action": "alias", "revision": 1, "reference": "key:" + hash, "name": "Bearer privatecredential"}); r.StatusCode != 400 {
		t.Fatal("secret-shaped alias accepted")
	}
	if r := edit(path, map[string]any{"action": "put-group", "revision": 1, "reference": "group:" + strings.Repeat("b", 64), "name": "Lab", "key_references": []string{"key:" + hash}}); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	var data struct {
		Keys   []history.Key   `json:"keys"`
		Groups []history.Group `json:"groups"`
	}
	json.Unmarshal(p.keysView().Body, &data)
	if len(data.Keys) != 1 || len(data.Groups) != 1 || len(data.Keys[0].Lineage) != 1 {
		t.Fatal(data)
	}
}
