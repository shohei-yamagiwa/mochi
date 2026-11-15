package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/google/jsonschema-go/jsonschema"
)

// Validator infers a JSON Schema from a Go type T and validates JSON payloads against it.
type Validator[T any] struct {
	schema   *jsonschema.Schema
	resolved *jsonschema.Resolved
}

// NewValidator creates a Validator for type T. An optional customize callback may refine the inferred schema
// (for example, adding enum values or numeric bounds) before resolution.
func NewValidator[T any](customize func(*jsonschema.Schema)) (*Validator[T], error) {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		return nil, fmt.Errorf("infer schema for %T: %w", *new(T), err)
	}
	if customize != nil {
		customize(s)
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("resolve schema for %T: %w", *new(T), err)
	}
	return &Validator[T]{
		schema:   s,
		resolved: resolved,
	}, nil
}

// Schema returns the inferred JSON Schema.
func (v *Validator[T]) Schema() *jsonschema.Schema {
	return v.schema
}

// SchemaJSON returns the pretty-printed JSON representation of the inferred schema.
func (v *Validator[T]) SchemaJSON() (json.RawMessage, error) {
	data, err := json.MarshalIndent(v.schema, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal schema: %w", err)
	}
	return data, nil
}

// ValidateJSON validates raw JSON bytes against the resolved schema and decodes them into T.
func (v *Validator[T]) ValidateJSON(raw []byte) (*T, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))

	var instance any
	if err := dec.Decode(&instance); err != nil {
		return nil, fmt.Errorf("decode json for validation: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing data after json document")
	}

	if err := v.resolved.Validate(instance); err != nil {
		return nil, fmt.Errorf("schema validation failed: %w", err)
	}

	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("unmarshal validated payload into %T: %w", out, err)
	}
	return &out, nil
}

// ValidateValue validates an in-memory Go value against the resolved schema by round-tripping through JSON.
func (v *Validator[T]) ValidateValue(val *T) error {
	if val == nil {
		return fmt.Errorf("cannot validate nil %T", *new(T))
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return fmt.Errorf("marshal %T for validation: %w", *val, err)
	}
	_, err = v.ValidateJSON(raw)
	return err
}
