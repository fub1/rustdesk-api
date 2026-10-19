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
		u, err := current(tx, actor)
		if err != nil {
			return err
		}
		uid, cid, err := scope(tx, u, raw, 2)
		if err != nil {
			return err
		}
		var existing []model.Tag
		if err := tx.Where("user_id = ? AND collection_id = ? AND name = ?", uid, cid, tag.Name).Find(&existing).Error; err != nil {
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
		u, err := current(tx, actor)
		if err != nil {
			return err
		}
		uid, cid, err := scope(tx, u, raw, 2)
		if err != nil {
			return err
		}
		var rows []model.Tag
		if err := tx.Where("user_id = ? AND collection_id = ? AND name IN ?", uid, cid, []string{oldName, newName}).Order("id ASC").Find(&rows).Error; err != nil {
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
		return tx.Model(&model.Tag{}).Where("user_id = ? AND collection_id = ? AND name = ?", uid, cid, oldName).Update("name", newName).Error
	})
}

func (s *Store) ColorTag(ctx context.Context, actor Actor, raw string, tag Tag) error {
	if tag.Name == "" {
		return ErrInput
	}
	return s.transaction(ctx, func(tx *gorm.DB) error {
		u, err := current(tx, actor)
		if err != nil {
			return err
		}
		uid, cid, err := scope(tx, u, raw, 2)
		if err != nil {
			return err
		}
		var rows []model.Tag
		if err := tx.Where("user_id = ? AND collection_id = ? AND name = ?", uid, cid, tag.Name).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return ErrNotFound
		}
		return tx.Model(&model.Tag{}).Where("user_id = ? AND collection_id = ? AND name = ?", uid, cid, tag.Name).Update("color", tag.Color).Error
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
		u, err := current(tx, actor)
		if err != nil {
			return err
		}
		uid, cid, err := scope(tx, u, raw, 3)
		if err != nil {
			return err
		}
		for _, name := range keys {
			var rows []model.Tag
			if err := tx.Where("user_id = ? AND collection_id = ? AND name = ?", uid, cid, name).Find(&rows).Error; err != nil {
				return err
			}
			if len(rows) == 0 {
				return ErrNotFound
			}
			if err := tx.Where("user_id = ? AND collection_id = ? AND name = ?", uid, cid, name).Delete(&model.Tag{}).Error; err != nil {
				return err
			}
		}
		return nil
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
		u, err := current(tx, actor)
		if err != nil {
			return err
		}
		uid, cid, err := scope(tx, u, raw, 3)
		if err != nil {
			return err
		}
		for _, id := range keys {
			var rows []model.AddressBook
			if err := tx.Where("user_id = ? AND collection_id = ? AND id = ?", uid, cid, id).Find(&rows).Error; err != nil {
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
