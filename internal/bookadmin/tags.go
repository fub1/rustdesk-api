package bookadmin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

func tagScope(row model.Tag) bookScope { return bookScope{row.UserId, row.CollectionId} }
func tagRows(tx *gorm.DB, scope bookScope, names []string) *gorm.DB {
	q := rowLock(tx).Where("user_id = ? AND collection_id = ?", scope.OwnerID, scope.CollectionID).Order("id ASC")
	switch tx.Dialector.Name() {
	case "mysql":
		return q.Where("BINARY name IN ?", names)
	case "sqlite":
		return q.Where("name COLLATE BINARY IN ?", names)
	default:
		return q.Where("name IN ?", names)
	}
}
func tagIDs(rows []model.Tag) []uint {
	ids := make([]uint, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.Id)
	}
	return ids
}
func lockedTag(tx *gorm.DB, id uint, candidate model.Tag) (model.Tag, error) {
	var row model.Tag
	if err := rowLock(tx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return row, ErrNotFound
		}
		return row, err
	}
	if !sameScope(tagScope(candidate), tagScope(row)) {
		return row, ErrInput
	}
	return row, nil
}
func (s *Store) CreateTag(ctx context.Context, actor auth.Principal, mode Mode, input TagInput) error {
	if input.ID != 0 || input.Name == "" {
		return ErrInput
	}
	scope, err := s.createScope(ctx, actor, mode, input.UserID, input.CollectionID)
	if err != nil {
		return err
	}
	return s.run(ctx, actor, mode, writePlan{Scopes: []bookScope{scope}}, func(tx *gorm.DB, _ lockedWrite) error {
		// Existing owner0 books may be maintained, but acquire no new children.
		if scope.OwnerID == 0 {
			return ErrInput
		}
		return tx.Create(&model.Tag{UserId: scope.OwnerID, CollectionId: scope.CollectionID, Name: input.Name, Color: input.Color}).Error
	})
}
func (s *Store) UpdateTag(ctx context.Context, actor auth.Principal, mode Mode, input TagInput) error {
	if input.ID == 0 || input.Name == "" {
		return ErrInput
	}
	var candidate model.Tag
	if err := s.candidate(ctx, actor, &candidate, input.ID, "name"); err != nil {
		return err
	}
	if candidate.Id == 0 {
		return s.missing(ctx, actor, mode)
	}
	return s.run(ctx, actor, mode, writePlan{Scopes: []bookScope{tagScope(candidate)}}, func(tx *gorm.DB, _ lockedWrite) error {
		scope := tagScope(candidate)
		if err := fixedScope(input.UserID, input.CollectionID, scope); err != nil {
			return err
		}
		if candidate.Name == input.Name {
			row, err := lockedTag(tx, input.ID, candidate)
			if err != nil {
				return err
			}
			if row.Name != candidate.Name {
				return ErrInput
			}
			return tx.Model(&row).Update("color", input.Color).Error
		}
		rows, row, err := lockedTagGroup(tx, scope, candidate, input.Name)
		if err != nil {
			return err
		}
		var old []model.Tag
		for _, other := range rows {
			if other.Name == input.Name {
				return ErrInput
			}
			if other.Name == row.Name {
				old = append(old, other)
			}
		}
		if err := tx.Model(&model.Tag{}).Where("user_id = ? AND collection_id = ? AND id IN ?", scope.OwnerID, scope.CollectionID, tagIDs(old)).Update("name", input.Name).Error; err != nil {
			return err
		}
		// An ID update controls only that row's color, even when renaming every
		// historical duplicate definition of the same exact name.
		if err := tx.Model(&model.Tag{}).Where("id = ?", row.Id).Update("color", input.Color).Error; err != nil {
			return err
		}
		return rewriteTagReferences(tx, scope, row.Name, input.Name, false)
	})
}
func (s *Store) DeleteTag(ctx context.Context, actor auth.Principal, mode Mode, input TagInput) error {
	if input.ID == 0 {
		return ErrInput
	}
	var candidate model.Tag
	if err := s.candidate(ctx, actor, &candidate, input.ID, "name"); err != nil {
		return err
	}
	if candidate.Id == 0 {
		return s.missing(ctx, actor, mode)
	}
	return s.run(ctx, actor, mode, writePlan{Scopes: []bookScope{tagScope(candidate)}}, func(tx *gorm.DB, _ lockedWrite) error {
		scope := tagScope(candidate)
		if err := fixedScope(input.UserID, input.CollectionID, scope); err != nil {
			return err
		}
		rows, row, err := lockedTagGroup(tx, scope, candidate, candidate.Name)
		if err != nil {
			return err
		}
		var old []model.Tag
		for _, other := range rows {
			if other.Name == row.Name {
				old = append(old, other)
			}
		}
		if err := tx.Where("user_id = ? AND collection_id = ? AND id IN ?", scope.OwnerID, scope.CollectionID, tagIDs(old)).Delete(&model.Tag{}).Error; err != nil {
			return err
		}
		return rewriteTagReferences(tx, scope, row.Name, "", true)
	})
}

func lockedTagGroup(tx *gorm.DB, scope bookScope, candidate model.Tag, target string) ([]model.Tag, model.Tag, error) {
	q := tagRows(tx, scope, []string{candidate.Name, target})
	if scope.OwnerID == 0 && scope.CollectionID == 0 {
		// There is no owner/parent anchor for this historical scope. Lock every
		// extant definition in one ascending order; new owner0 children are
		// prohibited. Distinct old names targeting one absent name serialize.
		q = rowLock(tx).Where("user_id = 0 AND collection_id = 0").Order("id ASC")
	}
	var rows []model.Tag
	if err := q.Find(&rows).Error; err != nil {
		return nil, model.Tag{}, err
	}
	for _, row := range rows {
		if row.Id == candidate.Id {
			if !sameScope(tagScope(row), scope) || row.Name != candidate.Name {
				return nil, row, ErrInput
			}
			return rows, row, nil
		}
	}
	return nil, model.Tag{}, ErrNotFound
}

type tagReference struct {
	RowID uint `gorm:"column:row_id"`
	Tags  sql.NullString
}

func decodeTags(raw string) ([]string, error) {
	var items []json.RawMessage
	if json.Unmarshal([]byte(raw), &items) != nil || items == nil {
		return nil, ErrUnavailable
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		var name string
		if string(item) == "null" || json.Unmarshal(item, &name) != nil {
			return nil, ErrUnavailable
		}
		result = append(result, name)
	}
	return result, nil
}

// Narrow tags-only writes preserve every other entry column and timestamp.
// The raw reader avoids AutoJson silently turning corrupt storage into [].
func rewriteTagReferences(tx *gorm.DB, scope bookScope, from, to string, remove bool) error {
	var rows []tagReference
	if err := rowLock(tx).Model(&model.AddressBook{}).Select("row_id,tags").Where("user_id = ? AND collection_id = ?", scope.OwnerID, scope.CollectionID).Order("row_id ASC").Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		if !row.Tags.Valid {
			return ErrUnavailable
		}
		before, err := decodeTags(row.Tags.String)
		if err != nil {
			return err
		}
		participates := false
		for _, name := range before {
			participates = participates || name == from
		}
		if !participates {
			continue
		}
		after := make([]string, 0, len(before))
		seen := false
		for _, name := range before {
			if name == from {
				if remove {
					continue
				}
				name = to
			}
			if !remove && name == to {
				if seen {
					continue
				}
				seen = true
			}
			after = append(after, name)
		}
		encoded, err := json.Marshal(after)
		if err != nil {
			return err
		}
		q := tx.Model(&model.AddressBook{}).Where("row_id = ? AND user_id = ? AND collection_id = ?", row.RowID, scope.OwnerID, scope.CollectionID)
		switch tx.Dialector.Name() {
		case "mysql":
			q = q.Where("BINARY tags = BINARY ?", row.Tags.String)
		case "sqlite":
			q = q.Where("tags = ? COLLATE BINARY", row.Tags.String)
		default:
			q = q.Where("tags = ?", row.Tags.String)
		}
		result := q.UpdateColumn("tags", string(encoded))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrUnavailable
		}
	}
	return nil
}
