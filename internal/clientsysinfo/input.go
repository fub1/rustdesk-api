// Package clientsysinfo owns the existing anonymous sysinfo report transaction.
// Device strings remain report metadata, not proof of ownership or authority.
package clientsysinfo

import (
	"bytes"
	"encoding/json"
	"strings"
)

type OptionalString struct {
	Present bool
	Value   string
}

func (f *OptionalString) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return ErrInput
	}
	if err := json.Unmarshal(data, &f.Value); err != nil {
		return ErrInput
	}
	f.Present = true
	return nil
}

// Input is a whitelist, independent of the Peer persistence model. The named
// preset fields remain recognized so direct Store callers also fail closed.
// Every decoded preset-* field is checked, including unknown future options.
type Input struct {
	ID                     string         `json:"id"`
	CPU                    OptionalString `json:"cpu"`
	Hostname               OptionalString `json:"hostname"`
	Memory                 OptionalString `json:"memory"`
	OS                     OptionalString `json:"os"`
	Username               OptionalString `json:"username"`
	UUID                   OptionalString `json:"uuid"`
	Version                OptionalString `json:"version"`
	PresetAddressBookName  OptionalString `json:"preset-address-book-name"`
	PresetAddressBookAlias OptionalString `json:"preset-address-book-alias"`
	managementPreset       bool
}

func (input *Input) UnmarshalJSON(data []byte) error {
	// A plain Go string accepts JSON null without changing a preceding value.
	// Decode ID through OptionalString too, including duplicate-key/null input.
	type plain Input
	var value plain
	decoded := struct {
		*plain
		ID OptionalString `json:"id"`
	}{plain: &value}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return ErrInput
	}
	if !decoded.ID.Present || decoded.ID.Value == "" {
		return ErrInput
	}
	// Walk every occurrence rather than a map: a duplicate empty value must
	// not hide an earlier management request. Match case like encoding/json.
	decoder := json.NewDecoder(bytes.NewReader(data))
	if _, err := decoder.Token(); err != nil {
		return ErrInput
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return ErrInput
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return ErrInput
		}
		if strings.HasPrefix(strings.ToLower(key.(string)), "preset-") {
			var preset OptionalString
			if err := json.Unmarshal(raw, &preset); err != nil {
				return ErrInput
			}
			value.managementPreset = value.managementPreset || preset.Value != ""
		}
	}
	*input = Input(value)
	input.ID = decoded.ID.Value
	return nil
}

func (input Input) hasManagementPreset() bool {
	return input.managementPreset || input.PresetAddressBookName.Value != "" || input.PresetAddressBookAlias.Value != ""
}

func (input Input) values() map[string]any {
	// The fixed 1.5.0 client omits username when no active user is available.
	// That omission is an empty snapshot, not a request to retain a stale user.
	values := map[string]any{"username": input.Username.Value}
	for column, field := range map[string]OptionalString{"cpu": input.CPU, "hostname": input.Hostname, "memory": input.Memory, "os": input.OS, "uuid": input.UUID, "version": input.Version} {
		if field.Present {
			values[column] = field.Value
		}
	}
	return values
}
