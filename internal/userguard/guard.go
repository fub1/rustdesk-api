// Package userguard serializes account role/status removals without a sentinel row.
package userguard

import (
	"errors"
	"sort"

	"github.com/lejianwen/rustdesk-api/v2/internal/collectionguard"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrLastEnabledAdmin = errors.New("the last enabled administrator cannot be removed")

func Rows(tx *gorm.DB) *gorm.DB {
	if tx.Dialector.Name() != "sqlite" {
		return tx.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return tx
}

// LockUsers must be the transaction's first row-lock phase. Every role/status
// mutation uses this protocol; new candidate drift never causes a late user lock.
// Missing requested rows remain absent so the caller can distinguish actor/target.
func LockUsers(tx *gorm.DB, requested ...uint) (map[uint]model.User, error) {
	var candidates []uint
	if err := tx.Model(&model.User{}).Where("is_admin = ?", true).Order("id ASC").Pluck("id", &candidates).Error; err != nil {
		return nil, err
	}
	seen := map[uint]bool{}
	ids := make([]uint, 0, len(requested)+len(candidates))
	for _, id := range append(candidates, requested...) {
		if id != 0 && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	locked := make(map[uint]model.User, len(ids))
	for _, id := range ids {
		var row model.User
		if err := Rows(tx).First(&row, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			return nil, err
		}
		locked[id] = row
	}
	var current []uint
	if err := tx.Model(&model.User{}).Where("is_admin = ?", true).Order("id ASC").Pluck("id", &current).Error; err != nil {
		return nil, err
	}
	for _, id := range current {
		if _, ok := locked[id]; !ok {
			return nil, collectionguard.ErrUnavailable
		}
	}
	return locked, nil
}

func EnabledAdmin(user model.User) bool {
	return user.IsAdmin != nil && *user.IsAdmin && user.Status == model.COMMON_STATUS_ENABLE
}

// CheckRemoval evaluates the locked current facts and the effective post-patch
// target. Existing zero-enabled-admin data is not silently repaired.
func CheckRemoval(locked map[uint]model.User, target model.User, after *model.User) error {
	if !EnabledAdmin(target) || (after != nil && EnabledAdmin(*after)) {
		return nil
	}
	for id, row := range locked {
		if id != target.Id && EnabledAdmin(row) {
			return nil
		}
	}
	return ErrLastEnabledAdmin
}

// Patch preserves the old GORM struct Updates nonzero semantics without accepting
// ORM metadata or a password through the account metadata route.
func Patch(current model.User, in model.User) (model.User, map[string]any) {
	next := current
	values := map[string]any{}
	if in.Username != "" {
		next.Username = in.Username
		values["username"] = in.Username
	}
	if in.Email != "" {
		next.Email = in.Email
		values["email"] = in.Email
	}
	if in.Nickname != "" {
		next.Nickname = in.Nickname
		values["nickname"] = in.Nickname
	}
	if in.Avatar != "" {
		next.Avatar = in.Avatar
		values["avatar"] = in.Avatar
	}
	if in.Remark != "" {
		next.Remark = in.Remark
		values["remark"] = in.Remark
	}
	if in.GroupId != 0 {
		next.GroupId = in.GroupId
		values["group_id"] = in.GroupId
	}
	if in.Status != 0 {
		next.Status = in.Status
		values["status"] = in.Status
	}
	if in.IsAdmin != nil {
		value := *in.IsAdmin
		next.IsAdmin = &value
		values["is_admin"] = value
	}
	return next, values
}
