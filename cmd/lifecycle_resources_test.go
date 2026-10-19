package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/internal/lifecycle"
	"github.com/spf13/cobra"
)

type closingCache struct{ closed bool }

func (*closingCache) Get(string, interface{}) error      { return nil }
func (*closingCache) Set(string, interface{}, int) error { return nil }
func (*closingCache) Gc() error                          { return nil }
func (c *closingCache) Close() error                     { c.closed = true; return nil }

func TestCommandClosesResourcesOnlyAfterCompleteDrain(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		t.Run(map[bool]string{false: "drained", true: "incomplete"}[incomplete], func(t *testing.T) {
			db := openBootDB(t, filepath.Join(t.TempDir(), "resources.db"))
			pool, _ := db.DB()
			previousCmd, previousDB, previousCache := rootCmd, global.DB, global.Cache
			cache := &closingCache{}
			global.DB, global.Cache = db, cache
			rootCmd = &cobra.Command{Use: "test-resources", RunE: func(*cobra.Command, []string) error {
				if incomplete {
					return errors.Join(lifecycle.ErrIncompleteShutdown, context.DeadlineExceeded)
				}
				return nil
			}}
			rootCmd.SetArgs(nil)
			t.Cleanup(func() { pool.Close(); rootCmd, global.DB, global.Cache = previousCmd, previousDB, previousCache })
			err := execute(context.Background())
			if incomplete {
				if !errors.Is(err, lifecycle.ErrIncompleteShutdown) || cache.closed || global.DB != db || pool.Ping() != nil {
					t.Fatal("incomplete drain closed or reset resources still in use")
				}
			} else {
				if err != nil || !cache.closed || global.DB != nil || global.Cache != nil || pool.Ping() == nil {
					t.Fatal("normal drain did not close owned resources")
				}
			}
		})
	}
}
