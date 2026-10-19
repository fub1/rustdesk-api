package heartbeatstore

import (
	"context"
	"errors"
	"sort"

	"github.com/lejianwen/rustdesk-api/v2/internal/heartbeat"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func New(db *gorm.DB) (heartbeat.Flush, error) {
	if db == nil || db.Error != nil {
		return nil, errors.New("heartbeat database is required")
	}
	owned := db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
	return func(ctx context.Context, batch map[uint]heartbeat.Entry) error {
		ids := make([]uint, 0, len(batch))
		for id := range batch {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		return owned.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			for _, id := range ids {
				entry := batch[id]
				if err := tx.Model(&model.Peer{}).Where("row_id = ? AND last_online_time <= ?", id, entry.Time).Updates(map[string]interface{}{"last_online_time": entry.Time, "last_online_ip": entry.IP}).Error; err != nil {
					return err
				}
			}
			return nil
		})
	}, nil
}
