package queuev1

import (
	"bytes"
	"encoding/json"
)

// Absence is backward-compatible; an explicitly supplied deadline must be a
// canonical string, not null/empty (which would otherwise erase its meaning).
func (r *SendRequest) UnmarshalJSON(data []byte) error {
	type plain SendRequest
	var value plain
	wire := struct {
		*plain
		ValidUntil json.RawMessage `json:"validUntil"`
	}{plain: &value}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return ErrInvalid
	}
	if wire.ValidUntil != nil {
		if err := json.Unmarshal(wire.ValidUntil, &value.ValidUntil); err != nil || value.ValidUntil == "" || !canonicalDeadline(value.ValidUntil) {
			return ErrInvalid
		}
	}
	*r = SendRequest(value)
	return nil
}
