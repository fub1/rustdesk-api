// Package clientcompat implements the selected RustDesk client contracts with
// explicit database/context inputs. It owns no legacy globals or schema.
package clientcompat

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

var (
	ErrInput       = errors.New("invalid client input")
	ErrForbidden   = errors.New("resource access denied")
	ErrNotFound    = errors.New("resource not found")
	ErrUnavailable = errors.New("client storage unavailable")
)

type Actor struct{ ID uint }
type Store struct{ db *gorm.DB }

func New(db *gorm.DB) (*Store, error) {
	if db == nil || db.Error != nil {
		return nil, ErrUnavailable
	}
	return &Store{db: db}, nil
}
func classify(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, ErrInput, ErrForbidden, ErrNotFound} {
		if errors.Is(err, known) {
			return known
		}
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return ErrUnavailable
}
func (s *Store) transaction(ctx context.Context, fn func(*gorm.DB) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return classify(s.db.WithContext(ctx).Transaction(fn))
}
func current(tx *gorm.DB, actor Actor) (model.User, error) {
	var u model.User
	if actor.ID == 0 {
		return u, ErrForbidden
	}
	if err := tx.First(&u, actor.ID).Error; err != nil {
		return u, err
	}
	if u.Status != model.COMMON_STATUS_ENABLE {
		return u, ErrForbidden
	}
	return u, nil
}
func parseGuid(guid string) (uint, uint, uint, error) {
	parts := strings.Split(guid, "-")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, 0, 0, ErrInput
	}
	var ids [3]uint
	for i, part := range parts {
		v, err := strconv.ParseUint(part, 10, strconv.IntSize)
		if err != nil || part == "" {
			return 0, 0, 0, ErrInput
		}
		ids[i] = uint(v)
	}
	if ids[0] == 0 || ids[1] == 0 {
		return 0, 0, 0, ErrInput
	}
	return ids[0], ids[1], ids[2], nil
}
func guid(gid, uid, cid uint) string {
	return strconv.FormatUint(uint64(gid), 10) + "-" + strconv.FormatUint(uint64(uid), 10) + "-" + strconv.FormatUint(uint64(cid), 10)
}
func scope(tx *gorm.DB, actor model.User, raw string, required int) (uint, uint, error) {
	gid, uid, cid, err := parseGuid(raw)
	if err != nil {
		return 0, 0, err
	}
	var owner model.User
	if err := tx.First(&owner, uid).Error; err != nil {
		return 0, 0, err
	}
	if owner.GroupId != gid {
		return 0, 0, ErrInput
	}
	if cid == 0 {
		if actor.Id != uid {
			return 0, 0, ErrForbidden
		}
		return uid, cid, nil
	}
	var collection model.AddressBookCollection
	if err := tx.First(&collection, cid).Error; err != nil {
		return 0, 0, err
	}
	if collection.UserId != uid {
		return 0, 0, ErrInput
	}
	if actor.Id == uid {
		return uid, cid, nil
	}
	var rule int
	err = tx.Model(&model.AddressBookCollectionRule{}).Where("collection_id = ? AND user_id = ? AND rule BETWEEN 1 AND 3", cid, uid).
		Where("(type = ? AND to_id = ?) OR (type = ? AND to_id = ?)", model.ShareAddressBookRuleTypePersonal, actor.Id, model.ShareAddressBookRuleTypeGroup, actor.GroupId).
		Select("COALESCE(MAX(rule), 0)").Scan(&rule).Error
	if err != nil {
		return 0, 0, err
	}
	if rule < required {
		return 0, 0, ErrForbidden
	}
	return uid, cid, nil
}
