// Package clientassign owns the administrator-authorized CLI assignment of an
// already registered device. Registered ID/UUID strings are not device proof.
package clientassign

import (
	"bytes"
	"encoding/json"
	"io"
)

type OptionalString struct {
	Present bool
	Value   string
}

func (f *OptionalString) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) || json.Unmarshal(data, &f.Value) != nil {
		return ErrInput
	}
	f.Present = true
	return nil
}

type Input struct {
	ID, UUID                                                    string
	UserName, DeviceGroupName, DeviceName, DeviceUsername, Note OptionalString
}

// Decode an exact whitelist, rejecting duplicate/unknown keys and null values.
// In particular no unsupported assignment is silently acknowledged or applied
// partially alongside the supported fields.
func (input *Input) UnmarshalJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return ErrInput
	}
	var value Input
	var id, uuid OptionalString
	fields := map[string]*OptionalString{"id": &id, "uuid": &uuid, "user_name": &value.UserName, "device_group_name": &value.DeviceGroupName, "device_name": &value.DeviceName, "device_username": &value.DeviceUsername, "note": &value.Note}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return ErrInput
		}
		name, ok := token.(string)
		field := fields[name]
		if !ok || field == nil || seen[name] || d.Decode(field) != nil {
			return ErrInput
		}
		seen[name] = true
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return ErrInput
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return ErrInput
	}
	value.ID, value.UUID = id.Value, uuid.Value
	if !value.valid() {
		return ErrInput
	}
	*input = value
	return nil
}

func (input Input) valid() bool {
	return input.ID != "" && input.UUID != "" &&
		(!input.UserName.Present || input.UserName.Value != "") &&
		(!input.DeviceGroupName.Present || input.DeviceGroupName.Value != "") &&
		(input.UserName.Present || input.DeviceGroupName.Present || input.DeviceName.Present || input.DeviceUsername.Present || input.Note.Present)
}
