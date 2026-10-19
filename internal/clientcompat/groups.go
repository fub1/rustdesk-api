package clientcompat

import (
	"context"
	"strings"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

func containsOS(value, part string) bool { return strings.Contains(strings.ToLower(value), part) }
func visibleGroup(tx *gorm.DB, u model.User) (bool, error) {
	if u.IsAdmin != nil && *u.IsAdmin {
		return true, nil
	}
	var group model.Group
	if err := tx.First(&group, u.GroupId).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return false, nil
		}
		return false, err
	}
	return group.Type == model.GroupTypeShare, nil
}
func (s *Store) Users(ctx context.Context, actor Actor, page Page, enabledOnly bool) (int64, []User, error) {
	rows := []User{}
	var total int64
	if !page.valid() {
		return 0, rows, ErrInput
	}
	err := s.transaction(ctx, func(tx *gorm.DB) error {
		u, err := current(tx, actor)
		if err != nil {
			return err
		}
		shared, err := visibleGroup(tx, u)
		if err != nil {
			return err
		}
		query := tx.Model(&model.User{})
		if shared {
			query = query.Where("group_id = ?", u.GroupId)
		} else {
			query = query.Where("id = ?", u.Id)
		}
		if enabledOnly {
			query = query.Where("status = ?", model.COMMON_STATUS_ENABLE)
		}
		if err := query.Count(&total).Error; err != nil {
			return err
		}
		var stored []model.User
		if err := query.Order("id ASC").Offset(page.offset()).Limit(page.Size).Find(&stored).Error; err != nil {
			return err
		}
		for _, v := range stored {
			status := 0
			if v.Status == model.COMMON_STATUS_ENABLE {
				status = 1
			}
			rows = append(rows, User{Name: v.Username, DisplayName: v.Nickname, Avatar: v.Avatar, Email: v.Email, IsAdmin: v.IsAdmin != nil && *v.IsAdmin, Status: status, Info: map[string]any{}})
		}
		return nil
	})
	return total, rows, err
}
func (s *Store) GroupPeers(ctx context.Context, actor Actor, page Page, enabledOnly bool) (int64, []GroupPeer, error) {
	rows := []GroupPeer{}
	var total int64
	if !page.valid() {
		return 0, rows, ErrInput
	}
	err := s.transaction(ctx, func(tx *gorm.DB) error {
		u, err := current(tx, actor)
		if err != nil {
			return err
		}
		shared, err := visibleGroup(tx, u)
		if err != nil {
			return err
		}
		query := tx.Table("peers AS p").Joins("JOIN users AS owner ON owner.id = p.user_id").Joins("LEFT JOIN device_groups AS dg ON dg.id = p.group_id")
		if shared {
			query = query.Where("owner.group_id = ?", u.GroupId)
		} else {
			query = query.Where("owner.id = ?", u.Id)
		}
		if enabledOnly {
			query = query.Where("owner.status = ?", model.COMMON_STATUS_ENABLE)
		}
		if err := query.Count(&total).Error; err != nil {
			return err
		}
		var stored []struct{ ID, Username, Hostname, OS, Owner, DeviceGroup string }
		if err := query.Select("p.id, p.username, p.hostname, p.os, owner.username AS owner, COALESCE(dg.name, '') AS device_group").Order("p.row_id ASC").Offset(page.offset()).Limit(page.Size).Scan(&stored).Error; err != nil {
			return err
		}
		for _, v := range stored {
			rows = append(rows, GroupPeer{ID: v.ID, Info: HostInfo{Username: v.Username, OS: v.OS, DeviceName: v.Hostname}, UserName: v.Owner, DeviceGroupName: v.DeviceGroup})
		}
		return nil
	})
	return total, rows, err
}
func (s *Store) DeviceGroups(ctx context.Context, actor Actor, page Page) (int64, []DeviceGroup, error) {
	rows := []DeviceGroup{}
	var total int64
	if !page.valid() {
		return 0, rows, ErrInput
	}
	err := s.transaction(ctx, func(tx *gorm.DB) error {
		u, err := current(tx, actor)
		if err != nil {
			return err
		}
		if u.IsAdmin == nil || !*u.IsAdmin {
			return ErrForbidden
		}
		query := tx.Model(&model.DeviceGroup{})
		if err := query.Count(&total).Error; err != nil {
			return err
		}
		return query.Select("name").Order("id ASC").Offset(page.offset()).Limit(page.Size).Find(&rows).Error
	})
	return total, rows, err
}
