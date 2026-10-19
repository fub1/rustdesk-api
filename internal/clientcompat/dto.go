package clientcompat

import (
	"database/sql"
	"encoding/json"
	"strconv"

	"github.com/lejianwen/rustdesk-api/v2/model"
)

type Page struct{ Current, Size int }

func (p Page) valid() bool {
	return p.Current > 0 && p.Size > 0 && p.Size <= 1000 && (p.Current-1) <= int(^uint(0)>>1)/p.Size
}
func (p Page) offset() int { return (p.Current - 1) * p.Size }

type Peer struct {
	ID               string   `json:"id"`
	Hash             string   `json:"hash"`
	Password         string   `json:"password"`
	Username         string   `json:"username"`
	Hostname         string   `json:"hostname"`
	Platform         string   `json:"platform"`
	Alias            string   `json:"alias"`
	Note             string   `json:"note"`
	Tags             []string `json:"tags"`
	ForceAlwaysRelay string   `json:"forceAlwaysRelay"`
	RdpPort          string   `json:"rdpPort"`
	RdpUsername      string   `json:"rdpUsername"`
	LoginName        string   `json:"loginName"`
	SameServer       bool     `json:"same_server"`
}

// The outer raw field overrides the embedded AutoJson scanner for this read
// projection. AutoJson intentionally normalizes malformed storage to [], which
// would otherwise turn a corrupt read into a destructive successful snapshot.
type peerRecord struct {
	model.AddressBook
	RawTags sql.NullString `gorm:"column:tags"`
}

func (peerRecord) TableName() string { return "address_books" }
func strictTags(data []byte) ([]string, error) {
	var items []json.RawMessage
	if json.Unmarshal(data, &items) != nil || items == nil {
		return nil, ErrInput
	}
	tags := make([]string, 0, len(items))
	for _, item := range items {
		var value StringField
		if value.UnmarshalJSON(item) != nil {
			return nil, ErrInput
		}
		tags = append(tags, value.Value)
	}
	return tags, nil
}
func peerDTO(record peerRecord) (Peer, error) {
	if !record.RawTags.Valid {
		return Peer{}, ErrUnavailable
	}
	tags, err := strictTags([]byte(record.RawTags.String))
	if err != nil {
		return Peer{}, ErrUnavailable
	}
	row := record.AddressBook
	return Peer{ID: row.Id, Hash: row.Hash, Password: row.Password, Username: row.Username, Hostname: row.Hostname, Platform: row.Platform, Alias: row.Alias, Note: row.Note, Tags: tags, ForceAlwaysRelay: strconv.FormatBool(row.ForceAlwaysRelay), RdpPort: row.RdpPort, RdpUsername: row.RdpUsername, LoginName: row.LoginName, SameServer: row.SameServer}, nil
}

type Profile struct {
	GUID  string `json:"guid"`
	Name  string `json:"name"`
	Owner string `json:"owner"`
	Note  string `json:"note"`
	Rule  int    `json:"rule"`
}
type Tag struct {
	Name  string `json:"name"`
	Color uint   `json:"color"`
}
type User struct {
	Name        string         `json:"name"`
	DisplayName string         `json:"display_name"`
	Avatar      string         `json:"avatar"`
	Email       string         `json:"email"`
	Note        string         `json:"note"`
	IsAdmin     bool           `json:"is_admin"`
	Status      int            `json:"status"`
	Info        map[string]any `json:"info"`
}
type DeviceGroup struct {
	Name string `json:"name"`
}
type HostInfo struct {
	Username   string `json:"username"`
	OS         string `json:"os"`
	DeviceName string `json:"device_name"`
}
type GroupPeer struct {
	ID              string   `json:"id"`
	Info            HostInfo `json:"info"`
	Status          int      `json:"status"`
	User            string   `json:"user"`
	UserName        string   `json:"user_name"`
	DeviceGroupName string   `json:"device_group_name"`
	Note            string   `json:"note"`
}

// Optional fields reject JSON null: only omission preserves a value, while an
// explicit empty string/list clears it.
type StringField struct {
	Present bool
	Value   string
}

func (f *StringField) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return ErrInput
	}
	if err := json.Unmarshal(b, &f.Value); err != nil {
		return ErrInput
	}
	f.Present = true
	return nil
}

type TagsField struct {
	Present bool
	Value   []string
}

func (f *TagsField) UnmarshalJSON(b []byte) error {
	value, err := strictTags(b)
	if err != nil {
		return ErrInput
	}
	f.Value = value
	f.Present = true
	return nil
}

type Patch struct {
	ID       string      `json:"id"`
	Alias    StringField `json:"alias"`
	Hash     StringField `json:"hash"`
	Password StringField `json:"password"`
	Username StringField `json:"username"`
	Hostname StringField `json:"hostname"`
	Platform StringField `json:"platform"`
	Tags     TagsField   `json:"tags"`
	Note     StringField `json:"note"`
}
type RelayFlag struct {
	Present bool
	Value   bool
}

func (f *RelayFlag) UnmarshalJSON(b []byte) error {
	var text string
	if json.Unmarshal(b, &text) == nil {
		if text != "true" && text != "false" {
			return ErrInput
		}
		f.Present, f.Value = true, text == "true"
		return nil
	}
	var value bool
	if string(b) == "null" || json.Unmarshal(b, &value) != nil {
		return ErrInput
	}
	f.Present, f.Value = true, value
	return nil
}

type BoolField struct {
	Present bool
	Value   bool
}

func (f *BoolField) UnmarshalJSON(b []byte) error {
	// The native peer DTO may send same_server:null for an unknown server.
	// Treat that as no assertion; explicit false remains a present value.
	if string(b) == "null" {
		return nil
	}
	if err := json.Unmarshal(b, &f.Value); err != nil {
		return ErrInput
	}
	f.Present = true
	return nil
}

// PeerInput deliberately has no row/user/collection identifiers. Ownership is
// supplied by the authenticated resource scope, never by the document.
type PeerInput struct {
	ID               string      `json:"id"`
	Hash             StringField `json:"hash"`
	Password         StringField `json:"password"`
	Username         StringField `json:"username"`
	Hostname         StringField `json:"hostname"`
	Platform         StringField `json:"platform"`
	Alias            StringField `json:"alias"`
	Tags             TagsField   `json:"tags"`
	ForceAlwaysRelay RelayFlag   `json:"forceAlwaysRelay"`
	RdpPort          StringField `json:"rdpPort"`
	RdpUsername      StringField `json:"rdpUsername"`
	LoginName        StringField `json:"loginName"`
	SameServer       BoolField   `json:"same_server"`
	Note             StringField `json:"note"`
}
type Document struct {
	Peers     []PeerInput `json:"peers"`
	Tags      []string    `json:"tags"`
	TagColors StringField `json:"tag_colors"`
}
