package clientassign

import (
	"database/sql"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/gorm"
)

func assignAddressBook(tx *gorm.DB, owner uint, peer model.Peer, input Input) error {
	var collections []model.AddressBookCollection
	if err := exact(locked(tx), "name", input.AddressBookName.Value).Where("user_id = ?", owner).Order("id ASC").Limit(2).Find(&collections).Error; err != nil {
		return err
	}
	if len(collections) != 1 || collections[0].UserId != owner || collections[0].Name != input.AddressBookName.Value {
		return ErrInput
	}
	cid := collections[0].Id
	if cid == 0 {
		return ErrInput
	}
	tag := input.AddressBookTag.Value
	if !input.AddressBookTag.Present {
		tag = ""
	}
	if tag != "" {
		var labels []model.Tag
		if err := exact(locked(tx), "name", tag).Where("user_id = ? AND collection_id = ?", owner, cid).Order("id ASC").Find(&labels).Error; err != nil {
			return err
		}
		if len(labels) == 0 {
			if err := tx.Create(&model.Tag{UserId: owner, CollectionId: cid, Name: tag, Color: 0}).Error; err != nil {
				return err
			}
		}
	}
	// Read raw JSON only when it will be edited. AutoJson's permissive scanner
	// must never silently erase damaged memberships during an append.
	var rows []struct {
		RowID uint
		Tags  sql.NullString
	}
	if err := exact(locked(tx).Model(&model.AddressBook{}), "id", peer.Id).Where("user_id = ? AND collection_id = ?", owner, cid).Select("row_id, tags").Order("row_id ASC").Find(&rows).Error; err != nil {
		return err
	}
	fields := map[string]any{}
	for column, field := range map[string]OptionalString{"alias": input.AddressBookAlias, "note": input.AddressBookNote, "password": input.AddressBookPassword} {
		if field.Present {
			fields[column] = field.Value
		}
	}
	if len(rows) == 0 {
		labels := []string{}
		if tag != "" {
			labels = append(labels, tag)
		}
		encoded, _ := json.Marshal(labels)
		row := model.AddressBook{Id: peer.Id, UserId: owner, CollectionId: cid, Hostname: peer.Hostname, Username: peer.Username, Platform: bookPlatform(peer.Os), Tags: custom_types.AutoJson(encoded)}
		if input.AddressBookAlias.Present {
			row.Alias = input.AddressBookAlias.Value
		}
		if input.AddressBookNote.Present {
			row.Note = input.AddressBookNote.Value
		}
		if input.AddressBookPassword.Present {
			row.Password = input.AddressBookPassword.Value
		}
		return tx.Create(&row).Error
	}
	for _, row := range rows {
		values := map[string]any{}
		for column, value := range fields {
			values[column] = value
		}
		if tag != "" {
			if !row.Tags.Valid {
				return ErrUnavailable
			}
			updated, changed, err := appendBookTag(row.Tags.String, tag)
			if err != nil {
				return err
			}
			if changed {
				values["tags"] = updated
			}
		}
		if len(values) != 0 {
			if err := tx.Model(&model.AddressBook{}).Where("row_id = ? AND user_id = ? AND collection_id = ?", row.RowID, owner, cid).Updates(values).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func appendBookTag(raw, name string) (string, bool, error) {
	var items []json.RawMessage
	if !utf8.ValidString(raw) || json.Unmarshal([]byte(raw), &items) != nil || items == nil {
		return "", false, ErrUnavailable
	}
	result := make([]string, 0, len(items)+1)
	seen, changed := false, false
	for _, item := range items {
		var value string
		if string(item) == "null" || json.Unmarshal(item, &value) != nil {
			return "", false, ErrUnavailable
		}
		if value == name {
			if seen {
				changed = true
				continue
			}
			seen = true
		}
		result = append(result, value)
	}
	if !seen {
		result = append(result, name)
		changed = true
	}
	if !changed {
		return raw, false, nil
	}
	encoded, _ := json.Marshal(result)
	return string(encoded), true, nil
}

func bookPlatform(os string) string {
	value := strings.ToLower(os)
	for _, candidate := range []struct{ part, platform string }{{"android", "Android"}, {"windows", "Windows"}, {"linux", "Linux"}, {"mac", "Mac OS"}} {
		if strings.Contains(value, candidate.part) {
			return candidate.platform
		}
	}
	return ""
}
