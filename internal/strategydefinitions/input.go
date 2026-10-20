// Package strategydefinitions owns explicit administrator CRUD for global
// named definitions. Device association is a separate, not-yet-enabled model.
package strategydefinitions

import (
	"bytes"
	"encoding/json"
	"io"
	"unicode/utf8"
)

type Input struct {
	ID            uint
	IDPresent     bool
	Name          string
	ConfigOptions map[string]string
}

type Definition struct {
	ID            uint              `json:"id"`
	Name          string            `json:"name"`
	ConfigOptions map[string]string `json:"config_options"`
}

type Page struct{ Current, Size int }

func (p Page) valid() bool {
	return p.Current > 0 && p.Size > 0 && p.Size <= 1000 && p.Current-1 <= int(^uint(0)>>1)/p.Size
}

func validName(name string) bool { return utf8.ValidString(name) && len(name) > 0 && len(name) <= 255 }

func (input Input) valid() bool {
	if !validName(input.Name) || input.ConfigOptions == nil {
		return false
	}
	for key, value := range input.ConfigOptions {
		if key == "" || !utf8.ValidString(key) || !utf8.ValidString(value) {
			return false
		}
	}
	return true
}

func Decode(data []byte) (Input, error) {
	var input Input
	if err := json.Unmarshal(data, &input); err != nil {
		return Input{}, ErrInput
	}
	return input, nil
}

// DecodeOptions shares the strict persisted-definition format with CLI and
// heartbeat consumers. Legacy direct-device policies keep their own contract.
func DecodeOptions(raw []byte) (map[string]string, error) { return parseOptions(raw) }

func (input *Input) UnmarshalJSON(data []byte) error {
	if !utf8.Valid(data) {
		return ErrInput
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return ErrInput
	}
	var value Input
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return ErrInput
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return ErrInput
		}
		seen[key] = true
		var raw json.RawMessage
		if d.Decode(&raw) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return ErrInput
		}
		switch key {
		case "id":
			if json.Unmarshal(raw, &value.ID) != nil {
				return ErrInput
			}
			value.IDPresent = true
		case "name":
			if json.Unmarshal(raw, &value.Name) != nil {
				return ErrInput
			}
		case "config_options":
			value.ConfigOptions, err = parseOptions(raw)
			if err != nil {
				return ErrInput
			}
		default:
			return ErrInput
		}
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return ErrInput
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF || !value.valid() {
		return ErrInput
	}
	*input = value
	return nil
}

func parseOptions(raw []byte) (map[string]string, error) {
	if !utf8.Valid(raw) {
		return nil, ErrInput
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return nil, ErrInput
	}
	values := map[string]string{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, ErrInput
		}
		key, ok := token.(string)
		if !ok || key == "" {
			return nil, ErrInput
		}
		if _, found := values[key]; found {
			return nil, ErrInput
		}
		var value *string
		if d.Decode(&value) != nil || value == nil {
			return nil, ErrInput
		}
		values[key] = *value
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return nil, ErrInput
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return nil, ErrInput
	}
	return values, nil
}
