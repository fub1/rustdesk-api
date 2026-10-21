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
	if u.GroupId == 0 {
		return false, nil
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

// directoryUsers applies the same owner scope before counting and paging all
// three directory lists. The alias is a fixed internal SQL identifier.
func directoryUsers(query *gorm.DB, u model.User, shared, enabledOnly bool, alias string) *gorm.DB {
	query = query.Where(alias + ".id > 0")
	if u.IsAdmin == nil || !*u.IsAdmin {
		if shared {
			query = query.Where(alias+".group_id = ?", u.GroupId)
		} else {
			query = query.Where(alias+".id = ?", u.Id)
		}
		enabledOnly = true
	}
	if enabledOnly {
		query = query.Where(alias+".status = ?", model.COMMON_STATUS_ENABLE)
	}
	return query
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
		query := directoryUsers(tx.Model(&model.User{}), u, shared, enabledOnly, "users")
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
		query := tx.Table("peers AS p").Joins("JOIN users AS owner ON owner.id = p.user_id").Joins("LEFT JOIN device_groups AS dg ON dg.id = p.group_id AND dg.id > 0")
		query = directoryUsers(query, u, shared, enabledOnly, "owner")
		if err := query.Count(&total).Error; err != nil {
			return err
		}
		var stored []struct{ ID, Username, Hostname, OS, Owner, DeviceGroup, Note string }
		if err := query.Select("p.id, p.username, p.hostname, p.os, p.note, owner.username AS owner, COALESCE(dg.name, '') AS device_group").Order("p.row_id ASC").Offset(page.offset()).Limit(page.Size).Scan(&stored).Error; err != nil {
			return err
		}
		for _, v := range stored {
			rows = append(rows, GroupPeer{ID: v.ID, Info: HostInfo{Username: v.Username, OS: v.OS, DeviceName: v.Hostname}, UserName: v.Owner, DeviceGroupName: v.DeviceGroup, Note: v.Note})
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
		query := tx.Model(&model.DeviceGroup{})
		if u.IsAdmin == nil || !*u.IsAdmin {
			shared, err := visibleGroup(tx, u)
			if err != nil {
				return err
			}
			peers := tx.Table("peers AS p").Select("1").Joins("JOIN users AS owner ON owner.id = p.user_id").Where("p.group_id = device_groups.id")
			peers = directoryUsers(peers, u, shared, true, "owner")
			query = query.Where("device_groups.id > 0").Where("EXISTS (?)", peers)
		}
		if err := query.Count(&total).Error; err != nil {
			return err
		}
		return query.Select("name").Order("id ASC").Offset(page.offset()).Limit(page.Size).Find(&rows).Error
	})
	return total, rows, err
}
