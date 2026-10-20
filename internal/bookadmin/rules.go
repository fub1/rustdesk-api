package bookadmin

import (
	"context"
	"errors"

	"github.com/lejianwen/rustdesk-api/v2/internal/auth"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

func ruleScope(row model.AddressBookCollectionRule) bookScope {
	return bookScope{row.UserId, row.CollectionId}
}
func ruleValues(input RuleInput) error {
	if input.ToID == 0 || input.Rule < 1 || input.Rule > 3 || (input.Type != model.ShareAddressBookRuleTypePersonal && input.Type != model.ShareAddressBookRuleTypeGroup) {
		return ErrInput
	}
	return nil
}
func rulePlan(scope bookScope, input RuleInput) writePlan {
	plan := writePlan{Scopes: []bookScope{scope}}
	if input.Type == model.ShareAddressBookRuleTypePersonal {
		plan.UserIDs = []uint{input.ToID}
	}
	return plan
}
func validateRule(tx *gorm.DB, scope bookScope, input RuleInput, excludeID uint) error {
	if input.Type == model.ShareAddressBookRuleTypePersonal {
		if input.ToID == scope.OwnerID {
			return ErrInput
		}
		// This target user was already locked with all other user identities.
		var user model.User
		if err := tx.First(&user, input.ToID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrInput
			}
			return err
		}
	} else {
		var group model.Group
		if err := tx.First(&group, input.ToID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrInput
			}
			return err
		}
	}
	var count int64
	if err := tx.Model(&model.AddressBookCollectionRule{}).Where("user_id = ? AND collection_id = ? AND type = ? AND to_id = ? AND id <> ?", scope.OwnerID, scope.CollectionID, input.Type, input.ToID, excludeID).Count(&count).Error; err != nil {
		return err
	}
	if count != 0 {
		return ErrInput
	}
	return nil
}
func (s *Store) CreateRule(ctx context.Context, actor auth.Principal, mode Mode, input RuleInput) error {
	if input.ID != 0 || input.CollectionID == 0 {
		return ErrInput
	}
	if err := ruleValues(input); err != nil {
		return err
	}
	scope, err := s.createScope(ctx, actor, mode, input.UserID, input.CollectionID)
	if err != nil {
		return err
	}
	return s.run(ctx, actor, mode, rulePlan(scope, input), func(tx *gorm.DB, _ lockedWrite) error {
		if scope.OwnerID == 0 {
			return ErrInput
		}
		if err := validateRule(tx, scope, input, 0); err != nil {
			return err
		}
		return tx.Create(&model.AddressBookCollectionRule{UserId: scope.OwnerID, CollectionId: scope.CollectionID, ToId: input.ToID, Type: input.Type, Rule: input.Rule}).Error
	})
}
func lockedRule(tx *gorm.DB, id uint, candidate model.AddressBookCollectionRule) (model.AddressBookCollectionRule, error) {
	var row model.AddressBookCollectionRule
	if err := rowLock(tx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return row, ErrNotFound
		}
		return row, err
	}
	if !sameScope(ruleScope(candidate), ruleScope(row)) {
		return row, ErrInput
	}
	return row, nil
}
func (s *Store) UpdateRule(ctx context.Context, actor auth.Principal, mode Mode, input RuleInput) error {
	if input.ID == 0 {
		return ErrInput
	}
	if err := ruleValues(input); err != nil {
		return err
	}
	var candidate model.AddressBookCollectionRule
	if err := s.candidate(ctx, actor, &candidate, input.ID); err != nil {
		return err
	}
	if candidate.Id == 0 {
		return s.missing(ctx, actor, mode)
	}
	return s.run(ctx, actor, mode, rulePlan(ruleScope(candidate), input), func(tx *gorm.DB, _ lockedWrite) error {
		row, err := lockedRule(tx, input.ID, candidate)
		if err != nil {
			return err
		}
		scope := ruleScope(row)
		if scope.CollectionID == 0 {
			return ErrInput
		}
		if err := fixedScope(input.UserID, input.CollectionID, scope); err != nil {
			return err
		}
		if err := validateRule(tx, scope, input, row.Id); err != nil {
			return err
		}
		return tx.Model(&row).Updates(map[string]any{"type": input.Type, "to_id": input.ToID, "rule": input.Rule}).Error
	})
}
func (s *Store) DeleteRule(ctx context.Context, actor auth.Principal, mode Mode, input RuleInput) error {
	if input.ID == 0 {
		return ErrInput
	}
	var candidate model.AddressBookCollectionRule
	if err := s.candidate(ctx, actor, &candidate, input.ID); err != nil {
		return err
	}
	if candidate.Id == 0 {
		return s.missing(ctx, actor, mode)
	}
	return s.run(ctx, actor, mode, writePlan{Scopes: []bookScope{ruleScope(candidate)}}, func(tx *gorm.DB, _ lockedWrite) error {
		row, err := lockedRule(tx, input.ID, candidate)
		if err != nil {
			return err
		}
		if err := fixedScope(input.UserID, input.CollectionID, ruleScope(row)); err != nil {
			return err
		}
		return tx.Delete(&row).Error
	})
}
