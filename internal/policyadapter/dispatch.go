package policyadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/cachepolicy"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const MaxRPCBytes = 64 * 1024

// Dispatch accepts only trusted CPA native RPC payloads, not public HTTP bodies.
// The parent retains responsibility for registration and all other plugin RPCs.
func (a *Adapter) Dispatch(ctx context.Context, method string, raw []byte) (any, error) {
	switch method {
	case pluginabi.MethodCachePolicyHandle:
		var req pluginapi.CachePolicyRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		if req.Method == "POST" {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(raw, &fields)
			if len(fields["operation"]) > cachepolicy.MaxCommandBytes {
				return nil, reject(413, "body_too_large", "Policy operation exceeds 16 KiB.")
			}
			var operation map[string]json.RawMessage
			_ = json.Unmarshal(fields["operation"], &operation)
			for _, name := range []string{"protocol", "operation", "installation_id", "session_id", "provider_route", "context_epoch", "expected_revision", "ttl", "operation_id"} {
				if operation[name] == nil {
					return nil, reject(400, "invalid", "Missing required policy operation fields.")
				}
			}
		}
		return a.HandleCachePolicy(ctx, req)
	case pluginabi.MethodCachePolicyResolve:
		var req pluginapi.CachePolicyResolveRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		return a.ResolveCachePolicy(ctx, req)
	case pluginabi.MethodCachePolicyAttempt:
		var req pluginapi.CachePolicyAttemptRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		response, err := a.CachePolicyAttempt(ctx, req)
		if err != nil {
			return nil, err
		}
		if req.Operation == "begin" {
			return response, nil
		}
		return struct{}{}, nil
	case pluginabi.MethodRequestComplete:
		// CPA rpc_schema.go wraps the PascalCase completion with this optional
		// native callback token. It is not caller identity or a policy capability.
		var req struct {
			pluginapi.RequestCompletion
			HostCallbackID string `json:"host_callback_id,omitempty"`
		}
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		if err := a.HandleRequestComplete(ctx, req.RequestCompletion); err != nil {
			return nil, err
		}
		return struct{}{}, nil
	default:
		return nil, reject(400, "invalid", "Unsupported policy adapter RPC.")
	}
}

func decode(raw []byte, out any) error {
	if len(raw) == 0 || len(raw) > MaxRPCBytes {
		return reject(400, "invalid", "Invalid native policy payload size.")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := exactValue(d, reflect.TypeOf(out).Elem()); err != nil {
		return reject(400, "invalid", "Invalid, duplicate, case-aliased or unknown native policy fields.")
	}
	if _, err := d.Token(); err != io.EOF {
		return reject(400, "invalid", "Exactly one native policy JSON object is required.")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return reject(400, "invalid", "Invalid native policy field types.")
	}
	return nil
}

// Token validation prevents encoding/json's duplicate-key and case-insensitive
// field behavior. Metadata maps remain extensible; no caller identity is read from them.
func exactValue(d *json.Decoder, t reflect.Type) error {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t != nil && t.Kind() == reflect.Interface {
		t = nil
	}
	tok, err := d.Token()
	if err != nil {
		return err
	}
	bad := func() error { return io.ErrUnexpectedEOF }
	if tok == nil {
		if t != nil && t.Kind() != reflect.Map && t.Kind() != reflect.Slice {
			return bad()
		}
		return nil
	}
	switch tok {
	case json.Delim('{'):
		fields := make(map[string]reflect.Type)
		strict := t != nil && t.Kind() == reflect.Struct
		if strict {
			objectFields(t, fields)
		}
		seen := make(map[string]bool)
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return bad()
			}
			seen[name] = true
			child := fields[name]
			if strict && child == nil {
				return bad()
			}
			if t != nil && t.Kind() == reflect.Map {
				child = t.Elem()
			}
			if err = exactValue(d, child); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return bad()
		}
	case json.Delim('['):
		var child reflect.Type
		if t != nil && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
			child = t.Elem()
		}
		for d.More() {
			if err := exactValue(d, child); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return bad()
		}
	case json.Delim('}'), json.Delim(']'):
		return bad()
	}
	return nil
}

func objectFields(t reflect.Type, fields map[string]reflect.Type) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "-" || f.PkgPath != "" {
			continue
		}
		if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
			objectFields(f.Type, fields)
			continue
		}
		if name == "" {
			name = f.Name
		}
		fields[name] = f.Type
	}
}
