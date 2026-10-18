package tools

import (
	"bytes"
	"encoding/json"
	"errors"
)

// errArgumentsMismatch is what DecodeArguments returns; its text is written for the model.
var errArgumentsMismatch = errors.New("the arguments do not match the tool's schema")

// DecodeArguments decodes a call's arguments into the tool's input struct. Empty arguments decode
// as an empty object. A field the struct does not declare is rejected, because every tool schema
// forbids additional properties and a dropped field would be a silent misunderstanding.
func DecodeArguments(raw json.RawMessage, into any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return errArgumentsMismatch
	}
	return nil
}
