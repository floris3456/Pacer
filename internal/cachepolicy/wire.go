package cachepolicy

import (
	"bytes"
	"encoding/json"
	"io"
)

const (
	CapabilityHeader = "X-Pacer-Cache-Capability"
	MaxCommandBytes  = 16 * 1024
)

// DecodeCommand enforces the exact versioned JSON contract. Credentials and
// trusted evidence have no JSON fields and cannot be supplied in this body.
func DecodeCommand(r io.Reader) (Command, error) {
	var cmd Command
	b, err := io.ReadAll(io.LimitReader(r, MaxCommandBytes+1))
	if err != nil || len(b) > MaxCommandBytes {
		return cmd, invalid("Cache policy JSON exceeds the body limit or could not be read.")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&cmd); err != nil {
		return Command{}, invalid("Invalid or unknown cache policy JSON fields.")
	}
	if d.Decode(new(any)) != io.EOF {
		return Command{}, invalid("Only one JSON object is allowed.")
	}
	if err = validateCommand(cmd); err != nil {
		return Command{}, err
	}
	if !validScope(cmd.Scope) {
		return Command{}, invalid("Invalid scope identifiers.")
	}
	return cmd, nil
}
