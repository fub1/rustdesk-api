package clientcompat

import (
	"database/sql"
	"encoding/json"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

type tagReference struct {
	RowID uint `gorm:"column:row_id"`
	Tags  sql.NullString
}

// Only the tags column is written: device metadata, notes, extension columns
// and timestamps remain untouched. Read raw strings instead of AutoJson's
// permissive scanner, and visit every physical row in the exact book scope.
func rewriteTagReferences(tx *gorm.DB, uid, cid uint, renameFrom, renameTo string, removed map[string]bool) error {
	var rows []tagReference
	if err := lockRows(tx).Model(&model.AddressBook{}).Select("row_id, tags").Where("user_id = ? AND collection_id = ?", uid, cid).Order("row_id ASC").Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		if !row.Tags.Valid {
			return ErrUnavailable
		}
		before, err := strictTags([]byte(row.Tags.String))
		if err != nil {
			return ErrUnavailable
		}
		if renameFrom != "" {
			participates := false
			for _, name := range before {
				participates = participates || name == renameFrom
			}
			if !participates {
				continue
			}
		}
		after := make([]string, 0, len(before))
		changed, seenTarget := false, false
		for _, name := range before {
			if removed[name] {
				changed = true
				continue
			}
			if renameFrom != "" && name == renameFrom {
				name, changed = renameTo, true
			}
			if renameFrom != "" && name == renameTo {
				if seenTarget {
					changed = true
					continue
				}
				seenTarget = true
			}
			after = append(after, name)
		}
		if !changed {
			continue
		}
		encoded, err := json.Marshal(after)
		if err != nil {
			return err
		}
		query := tx.Model(&model.AddressBook{}).Where("row_id = ? AND user_id = ? AND collection_id = ?", row.RowID, uid, cid)
		switch tx.Dialector.Name() {
		case "mysql":
			query = query.Where("BINARY tags = BINARY ?", row.Tags.String)
		case "sqlite":
			query = query.Where("tags = ? COLLATE BINARY", row.Tags.String)
		default:
			query = query.Where("tags = ?", row.Tags.String)
		}
		result := query.UpdateColumn("tags", string(encoded))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrUnavailable
		}
	}
	return nil
}
