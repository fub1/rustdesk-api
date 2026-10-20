package database

import (
	"fmt"
	"strings"
	"testing"

	"github.com/lejianwen/rustdesk-api/v2/model"
)

func TestStrategyDefinitionsSchema280AndFutureReentry(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "upgrade279"}[legacy], func(t *testing.T) {
			db := openTestDB(t)
			if legacy {
				if err := Update(db, 279, seedTestData); err != nil {
					t.Fatal(err)
				}
				mustExec(t, db, "DROP TABLE strategy_definitions")
			}
			if err := Update(db, 280, seedTestData); err != nil {
				t.Fatal(err)
			}
			assertCurrentVersion(t, db, 280)
			names := []string{"Name", "name", "name ", "é", "e\u0301", "a\x00b", "a", "É"}
			for _, name := range names {
				if err := db.Create(&model.StrategyDefinition{Name: model.StrategyName(name), ConfigOptions: "{}"}).Error; err != nil {
					t.Fatal("distinct byte name rejected", err)
				}
			}
			if err := db.Create(&model.StrategyDefinition{Name: model.StrategyName("Name"), ConfigOptions: "{}"}).Error; err == nil {
				t.Fatal("exact duplicate accepted")
			}
			if err := db.Exec("INSERT INTO strategy_definitions(name,config_options) VALUES(NULL,'{}')").Error; err == nil {
				t.Fatal("NULL name accepted")
			}
			if err := db.Exec("INSERT INTO strategy_definitions(name,config_options) VALUES(?,NULL)", []byte("null-options")).Error; err == nil {
				t.Fatal("NULL config accepted")
			}
			for _, target := range []uint{280, 281} {
				if err := Update(db, target, seedTestData); err != nil {
					t.Fatal(err)
				}
			}
			var rows []model.StrategyDefinition
			if err := db.Order("id ASC").Find(&rows).Error; err != nil || len(rows) != len(names) {
				t.Fatal("future migration lost definitions", err)
			}
			for i, row := range rows {
				if string(row.Name) != names[i] || row.ConfigOptions != "{}" {
					t.Fatal("future migration changed exact name/config")
				}
			}
			if err := db.Exec("INSERT INTO strategy_definitions(name,config_options) VALUES('text-name','{}')").Error; err == nil {
				t.Fatal("SQLite accepted a TEXT name in the BLOB namespace")
			}
			columns, err := db.Migrator().ColumnTypes(&model.StrategyDefinition{})
			if err != nil || len(columns) != 3 {
				t.Fatal("definition schema must have exactly three columns", err)
			}
		})
	}
}

func TestStrategyDefinitionsMalformedExistingSchemaDoesNotAdvanceVersion(t *testing.T) {
	valid := "id INTEGER PRIMARY KEY AUTOINCREMENT, name BLOB NOT NULL CHECK(typeof(name)='blob'), config_options TEXT NOT NULL, UNIQUE(name)"
	for _, tc := range []struct {
		name, columns, index string
	}{
		{name: "text_name", columns: strings.Replace(valid, "name BLOB", "name TEXT", 1)},
		{name: "nullable_name", columns: strings.Replace(valid, "name BLOB NOT NULL", "name BLOB", 1)},
		{name: "nullable_config", columns: strings.Replace(valid, "config_options TEXT NOT NULL", "config_options TEXT", 1)},
		{name: "missing_config", columns: "id INTEGER PRIMARY KEY AUTOINCREMENT, name BLOB NOT NULL CHECK(typeof(name)='blob') UNIQUE"},
		{name: "extra_column", columns: strings.Replace(valid, ", UNIQUE(name)", ", unexpected TEXT, UNIQUE(name)", 1)},
		{name: "no_autoincrement", columns: strings.Replace(valid, " AUTOINCREMENT", "", 1)},
		{name: "no_binary_check", columns: strings.Replace(valid, " CHECK(typeof(name)='blob')", "", 1)},
		{name: "no_unique", columns: strings.Replace(valid, ", UNIQUE(name)", "", 1)},
		{name: "partial_unique", columns: strings.Replace(valid, ", UNIQUE(name)", "", 1), index: "CREATE UNIQUE INDEX partial_name ON strategy_definitions(name) WHERE id > 10"},
		{name: "composite_unique", columns: strings.Replace(valid, "UNIQUE(name)", "UNIQUE(name,config_options)", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			if err := Update(db, 279, seedTestData); err != nil {
				t.Fatal(err)
			}
			mustExec(t, db, "DROP TABLE strategy_definitions")
			mustExec(t, db, fmt.Sprintf("CREATE TABLE strategy_definitions (%s)", tc.columns))
			if tc.index != "" {
				mustExec(t, db, tc.index)
			}
			var before string
			if err := db.Raw("SELECT sql FROM sqlite_master WHERE type='table' AND name='strategy_definitions'").Scan(&before).Error; err != nil {
				t.Fatal(err)
			}
			if err := Update(db, 280, seedTestData); err == nil {
				t.Fatal("malformed existing schema acknowledged")
			}
			assertCurrentVersion(t, db, 279)
			var after string
			if err := db.Raw("SELECT sql FROM sqlite_master WHERE type='table' AND name='strategy_definitions'").Scan(&after).Error; err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatal("migration silently repaired unknown existing schema")
			}
		})
	}
}

func TestStrategyDefinitionVersionFailureRetries(t *testing.T) {
	db := openTestDB(t)
	if err := Update(db, 279, seedTestData); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "DROP TABLE strategy_definitions")
	mustExec(t, db, "CREATE TRIGGER reject_definition_version BEFORE INSERT ON versions BEGIN SELECT RAISE(ABORT,'version failure'); END")
	if err := Update(db, 280, seedTestData); err == nil {
		t.Fatal("version failure acknowledged")
	}
	assertCurrentVersion(t, db, 279)
	if db.Migrator().HasTable(&model.StrategyDefinition{}) {
		t.Fatal("SQLite schema committed before version")
	}
	mustExec(t, db, "DROP TRIGGER reject_definition_version")
	if err := Update(db, 280, seedTestData); err != nil {
		t.Fatal(err)
	}
	assertCurrentVersion(t, db, 280)
}
