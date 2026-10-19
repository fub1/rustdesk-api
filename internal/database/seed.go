package database

import (
	"fmt"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

// SeedInitialData uses the migration transaction, never the global services.
func SeedInitialData(db *gorm.DB, defaultName, shareName, adminHash string) error {
	group := &model.Group{Name: defaultName, Type: model.GroupTypeDefault}
	if err := db.Create(group).Error; err != nil {
		return fmt.Errorf("create default group: %w", err)
	}
	shared := &model.Group{Name: shareName, Type: model.GroupTypeShare}
	if err := db.Create(shared).Error; err != nil {
		return fmt.Errorf("create shared group: %w", err)
	}
	isAdmin := true
	admin := &model.User{
		Username: "admin", Nickname: "Admin", Password: adminHash,
		Status: model.COMMON_STATUS_ENABLE, IsAdmin: &isAdmin, GroupId: group.Id,
	}
	if err := db.Create(admin).Error; err != nil {
		return fmt.Errorf("create initial admin: %w", err)
	}
	return nil
}
