package clientsysinfo

import (
	"errors"
	"strings"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/gorm"
)

// legacyPreset preserves the two existing preset options and their legacy
// owner=1/cleanup behavior. Only error/context/transaction plumbing is changed.
// PRE01/02 still own admission, ownership and the unsupported preset options.
// All operations use the caller's transaction; no legacy global helper is used.
func legacyPreset(tx *gorm.DB, rowID uint, input Input) error {
	alias := input.PresetAddressBookAlias.Value
	if alias != "" {
		if err := tx.Model(&model.Peer{}).Where("row_id = ?", rowID).Update("alias", alias).Error; err != nil {
			return err
		}
	}
	if input.PresetAddressBookName.Value == "" {
		return nil
	}
	var collection model.AddressBookCollection
	if err := tx.Where("name = ? AND user_id = 1", input.PresetAddressBookName.Value).First(&collection).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	var existing model.AddressBook
	err := tx.Where("id = ? AND collection_id = ?", input.ID, collection.Id).First(&existing).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if err == nil {
		values := map[string]any{}
		for column, value := range map[string]string{"alias": alias, "hostname": input.Hostname.Value, "username": input.Username.Value} {
			if value != "" {
				values[column] = value
			}
		}
		if len(values) != 0 {
			if err := tx.Model(&model.AddressBook{}).Where("row_id = ?", existing.RowId).Updates(values).Error; err != nil {
				return err
			}
		}
	} else {
		row := model.AddressBook{Id: input.ID, Alias: alias, Hostname: input.Hostname.Value, Username: input.Username.Value, Platform: legacyPlatform(input.OS.Value), Tags: custom_types.AutoJson("[]"), UserId: 1, CollectionId: collection.Id}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		if err := tx.Where("id = ? AND user_id = 1 AND collection_id != ?", input.ID, collection.Id).Delete(&model.AddressBook{}).Error; err != nil {
			return err
		}
	}
	// This legacy preset branch intentionally retains its original empty-value
	// behavior; the basic report whitelist cannot modify these management keys.
	return tx.Model(&model.Peer{}).Where("row_id = ?", rowID).Updates(map[string]any{"ab_id": collection.Id, "alias": alias, "username": input.Username.Value, "hostname": input.Hostname.Value}).Error
}

func legacyPlatform(os string) string {
	for _, entry := range []struct {
		parts []string
		name  string
	}{
		{[]string{"Android", "android"}, "Android"},
		{[]string{"Windows", "windows"}, "Windows"},
		{[]string{"Linux", "linux"}, "Linux"},
		{[]string{"mac", "Mac"}, "Mac OS"},
	} {
		for _, part := range entry.parts {
			if strings.Contains(os, part) {
				return entry.name
			}
		}
	}
	return ""
}
