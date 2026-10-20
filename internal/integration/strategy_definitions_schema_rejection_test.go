package integration

import (
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/internal/database"
	"github.com/lejianwen/rustdesk-api/v2/model"
)

func TestStrategyDefinitionsRejectExistingInvalidSchema(t *testing.T) {
	forEngines(t, func(t *testing.T, f *fixture) {
		if err := database.Update(f.DB, 279, nil); err != nil {
			t.Fatal(err)
		}
		var shapes []string
		if f.Engine == "mysql" {
			shapes = []string{
				"CREATE TABLE strategy_definitions(id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,name VARBINARY(255) NOT NULL UNIQUE,config_options LONGTEXT NOT NULL) ENGINE=MyISAM",
				"CREATE TABLE strategy_definitions(id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,name VARBINARY(255) NOT NULL UNIQUE,config_options LONGTEXT NOT NULL) ENGINE=InnoDB",
				"CREATE TABLE strategy_definitions(id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,name VARBINARY(255) NOT NULL UNIQUE,config_options TEXT NOT NULL,PRIMARY KEY(id,name)) ENGINE=InnoDB",
			}
		} else {
			shapes = []string{
				"CREATE TABLE strategy_definitions(id BIGSERIAL PRIMARY KEY,name TEXT NOT NULL UNIQUE,config_options TEXT NOT NULL)",
				"CREATE TABLE strategy_definitions(id BIGSERIAL PRIMARY KEY,name BYTEA NOT NULL,config_options TEXT NOT NULL)",
				"CREATE TABLE strategy_definitions(id BIGSERIAL PRIMARY KEY,name BYTEA NOT NULL UNIQUE,config_options TEXT)",
			}
		}
		for _, ddl := range shapes {
			transactionExec(t, f.DB, "DROP TABLE strategy_definitions")
			transactionExec(t, f.DB, ddl)
			if err := database.Update(f.DB, 280, nil); err == nil {
				t.Fatal("existing invalid definition table was accepted")
			}
			var version model.Version
			if err := f.Reopen(t).Last(&version).Error; err != nil || version.Version != 279 {
				t.Fatal("invalid definition schema advanced version", err)
			}
		}
		// The invalid table is retained for explicit repair. Removing this owned
		// empty fixture table allows a clean retry, without inferring old data.
		transactionExec(t, f.DB, "DROP TABLE strategy_definitions")
		if err := database.Update(f.DB, 280, nil); err != nil {
			t.Fatal("clean retry failed", err)
		}
	})
}
