package clientcompat

import (
	"context"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

// TagInput contains only client-owned values. Resource ownership always comes
// from the authenticated GUID scope, never a bound persistence model.
type TagInput struct {
	Name  StringField `json:"name"`
	Color *uint       `json:"color"`
}

type TagRenameInput struct {
	Old StringField `json:"old"`
	New StringField `json:"new"`
}

func (s *Store) AddTag(ctx context.Context, actor Actor, raw string, tag Tag) error {
	if tag.Name == "" {
		return ErrInput
	}
	return s.transaction(ctx, func(tx *gorm.DB) error {
		uid, cid, err := writeScope(tx, actor, raw, 2)
		if err != nil {
			return err
		}
		var existing []model.Tag
		if err := tagRows(tx, uid, cid, []string{tag.Name}).Find(&existing).Error; err != nil {
			return err
		}
		if len(existing) != 0 {
			return ErrInput
		}
		return tx.Create(&model.Tag{UserId: uid, CollectionId: cid, Name: tag.Name, Color: tag.Color}).Error
	})
}

func (s *Store) RenameTag(ctx context.Context, actor Actor, raw, oldName, newName string) error {
	if oldName == "" || newName == "" {
		return ErrInput
	}
	return s.transaction(ctx, func(tx *gorm.DB) error {
		uid, cid, err := writeScope(tx, actor, raw, 2)
		if err != nil {
			return err
		}
		var rows []model.Tag
		if err := tagRows(tx, uid, cid, []string{oldName, newName}).Find(&rows).Error; err != nil {
			return err
		}
		found := false
		for _, row := range rows {
			if row.Name == newName {
				return ErrInput
			}
			found = found || row.Name == oldName
		}
		if !found {
			return ErrNotFound
		}
		if err := tx.Model(&model.Tag{}).Where("user_id = ? AND collection_id = ? AND id IN ?", uid, cid, tagIDs(rows)).Update("name", newName).Error; err != nil {
			return err
		}
		return rewriteTagReferences(tx, uid, cid, oldName, newName, nil)
	})
}

func (s *Store) ColorTag(ctx context.Context, actor Actor, raw string, tag Tag) error {
	if tag.Name == "" {
		return ErrInput
	}
	return s.transaction(ctx, func(tx *gorm.DB) error {
		uid, cid, err := writeScope(tx, actor, raw, 2)
		if err != nil {
			return err
		}
		var rows []model.Tag
		if err := tagRows(tx, uid, cid, []string{tag.Name}).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return ErrNotFound
		}
		return tx.Model(&model.Tag{}).Where("user_id = ? AND collection_id = ? AND id IN ?", uid, cid, tagIDs(rows)).Update("color", tag.Color).Error
	})
}

func distinctKeys(keys []string) ([]string, error) {
	if len(keys) == 0 {
		return nil, ErrInput
	}
	result := make([]string, 0, len(keys))
	seen := map[string]bool{}
	for _, key := range keys {
		if key == "" {
			return nil, ErrInput
		}
		if !seen[key] {
			result = append(result, key)
			seen[key] = true
		}
	}
	return result, nil
}

// DeleteTags treats duplicate request names once. Missing names fail the whole
// request, and every historical duplicate row in this exact scope is removed.
func (s *Store) DeleteTags(ctx context.Context, actor Actor, raw string, names []string) error {
	keys, err := distinctKeys(names)
	if err != nil {
		return err
	}
	return s.transaction(ctx, func(tx *gorm.DB) error {
		uid, cid, err := writeScope(tx, actor, raw, 3)
		if err != nil {
			return err
		}
		removed := make(map[string]bool, len(keys))
		for _, name := range keys {
			var rows []model.Tag
			if err := tagRows(tx, uid, cid, []string{name}).Find(&rows).Error; err != nil {
				return err
			}
			if len(rows) == 0 {
				return ErrNotFound
			}
			if err := tx.Where("user_id = ? AND collection_id = ? AND id IN ?", uid, cid, tagIDs(rows)).Delete(&model.Tag{}).Error; err != nil {
				return err
			}
			removed[name] = true
		}
		return rewriteTagReferences(tx, uid, cid, "", "", removed)
	})
}

// DeletePeers uses the same ID scope as Patch: all historical duplicate rows
// for one business ID are affected. A retry after successful deletion fails
// with ErrNotFound; it never broadens the owner/collection scope.
func (s *Store) DeletePeers(ctx context.Context, actor Actor, raw string, ids []string) error {
	keys, err := distinctKeys(ids)
	if err != nil {
		return err
	}
	return s.transaction(ctx, func(tx *gorm.DB) error {
		uid, cid, err := writeScope(tx, actor, raw, 3)
		if err != nil {
			return err
		}
		for _, id := range keys {
			var rows []model.AddressBook
			if err := lockRows(tx).Where("user_id = ? AND collection_id = ? AND id = ?", uid, cid, id).Find(&rows).Error; err != nil {
				return err
			}
			if len(rows) == 0 {
				return ErrNotFound
			}
			if err := tx.Where("user_id = ? AND collection_id = ? AND id = ?", uid, cid, id).Delete(&model.AddressBook{}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
