package heartbeatstore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/internal/heartbeat"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestAtomicFlushFailureRetryAndNoTimeRegression(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "heartbeats.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	t.Cleanup(func() { pool.Close() })
	if err := db.AutoMigrate(&model.Peer{}); err != nil {
		t.Fatal(err)
	}
	a, b := model.Peer{Id: "a", LastOnlineTime: 10}, model.Peer{Id: "b", LastOnlineTime: 10}
	db.Create(&a)
	db.Create(&b)
	flush, _ := New(db)
	worker, _ := heartbeat.New(flush, heartbeat.Options{})
	_ = worker.Buffer(a.RowId, heartbeat.Entry{Time: 20, IP: "a-new"})
	_ = worker.Buffer(b.RowId, heartbeat.Entry{Time: 20, IP: "b-new"})
	db.Callback().Update().Before("gorm:update").Register("fault", func(tx *gorm.DB) {
		if tx.Statement.Table == "peers" {
			var calls int64
			tx.Session(&gorm.Session{NewDB: true}).Model(&model.Peer{}).Where("last_online_time = ?", 20).Count(&calls)
			if calls > 0 {
				tx.AddError(errors.New("second row failed"))
			}
		}
	})
	if err := worker.Flush(context.Background()); err == nil {
		t.Fatal("injected second update did not fail")
	}
	var got model.Peer
	db.First(&got, a.RowId)
	if got.LastOnlineTime != 10 || worker.Pending() != 2 {
		t.Fatal("partial heartbeat commit or lost batch")
	}
	db.Callback().Update().Remove("fault")
	if err := worker.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	db.First(&got, a.RowId)
	if got.LastOnlineTime != 20 {
		t.Fatal("retry did not persist")
	}
	if err := flush(context.Background(), map[uint]heartbeat.Entry{a.RowId: {Time: 15, IP: "older"}}); err != nil {
		t.Fatal(err)
	}
	db.First(&got, a.RowId)
	if got.LastOnlineTime != 20 || got.LastOnlineIp != "a-new" {
		t.Fatal("late observation overwrote current telemetry")
	}
}
