// Package clientsysinfo owns the existing anonymous sysinfo report transaction.
// Device strings remain report metadata, not proof of ownership or authority.
package clientsysinfo

import "encoding/json"

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

// Input is a whitelist, independent of the Peer persistence model. Only the
// two already consumed preset options are retained here; other client options
// do not become supported merely because the JSON decoder tolerates them.
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
	*input = Input(value)
	input.ID = decoded.ID.Value
	return nil
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
