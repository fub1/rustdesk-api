package clientcompat

import (
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func lockRows(tx *gorm.DB) *gorm.DB {
	if tx.Dialector.Name() != "sqlite" {
		return tx.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return tx
}

// Client writes serialize on the existing owner user row. Lock actor and owner
// in ascending ID order before any snapshot read; auth stores also lock users
// before sessions. SQLite relies on its transaction write lock (the production
// DSN uses _txlock=immediate); busy/snapshot conflicts fail the transaction.
// Management writers do not all participate in this lock protocol.
func writeActor(tx *gorm.DB, actor Actor, ownerID uint) (model.User, error) {
	if actor.ID == 0 {
		return model.User{}, ErrForbidden
	}
	ids := []uint{actor.ID}
	if ownerID != actor.ID {
		if ownerID < actor.ID {
			ids = []uint{ownerID, actor.ID}
		} else {
			ids = append(ids, ownerID)
		}
	}
	var user model.User
	for _, id := range ids {
		var row model.User
		if err := lockRows(tx).First(&row, id).Error; err != nil {
			return model.User{}, err
		}
		if row.Id == actor.ID {
			user = row
		}
	}
	if user.Status != model.COMMON_STATUS_ENABLE {
		return model.User{}, ErrForbidden
	}
	return user, nil
}

func writeScope(tx *gorm.DB, actor Actor, raw string, required int) (uint, uint, error) {
	_, ownerID, _, err := parseGuid(raw)
	if err != nil {
		return 0, 0, err
	}
	u, err := writeActor(tx, actor, ownerID)
	if err != nil {
		return 0, 0, err
	}
	return scope(tx, u, raw, required)
}

// Client tag names compare as exact strings, including case, accents and final
// spaces. MySQL's default text collation is not suitable for this comparison.
func tagRows(tx *gorm.DB, uid, cid uint, names []string) *gorm.DB {
	query := lockRows(tx).Where("user_id = ? AND collection_id = ?", uid, cid).Order("id ASC")
	switch tx.Dialector.Name() {
	case "mysql":
		return query.Where("BINARY name IN ?", names)
	case "sqlite":
		return query.Where("name COLLATE BINARY IN ?", names)
	default:
		return query.Where("name IN ?", names)
	}
}

func tagIDs(rows []model.Tag) []uint {
	ids := make([]uint, len(rows))
	for i, row := range rows {
		ids[i] = row.Id
	}
	return ids
}
