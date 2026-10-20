package database

import (
	"fmt"
	"gorm.io/gorm"
	"strings"
	"unicode"
)

// Keep this table out of ordinary AutoMigrate. Converting a populated MySQL
// VARBINARY name back to a case-insensitive VARCHAR before fixing its type can
// reject distinct valid names on a later upgrade.
func migrateStrategyDefinitions(db *gorm.DB) error {
	var ddl string
	switch db.Dialector.Name() {
	case "mysql":
		ddl = `CREATE TABLE IF NOT EXISTS strategy_definitions (
id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
name VARBINARY(255) NOT NULL,
config_options LONGTEXT NOT NULL,
PRIMARY KEY (id), UNIQUE KEY strategy_definitions_name_key (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`
	case "postgres":
		ddl = `CREATE TABLE IF NOT EXISTS strategy_definitions (
id BIGSERIAL PRIMARY KEY,
name BYTEA NOT NULL,
config_options TEXT NOT NULL,
CONSTRAINT strategy_definitions_name_key UNIQUE (name)
)`
	case "sqlite":
		ddl = `CREATE TABLE IF NOT EXISTS strategy_definitions (
id INTEGER PRIMARY KEY AUTOINCREMENT,
name BLOB NOT NULL CHECK (typeof(name) = 'blob'),
config_options TEXT NOT NULL,
CONSTRAINT strategy_definitions_name_key UNIQUE (name)
)`
	default:
		return fmt.Errorf("unsupported strategy definition database dialect %q", db.Dialector.Name())
	}
	if err := db.Exec(ddl).Error; err != nil {
		return fmt.Errorf("migrate strategy definitions: %w", err)
	}
	return validateStrategyDefinitions(db)
}

func invalidDefinitionSchema() error {
	return fmt.Errorf("invalid existing strategy definitions schema")
}

// Metadata reads return checked errors. CREATE IF NOT EXISTS must not bless a
// pre-existing nullable/text-name/no-unique table as a completed migration.
func validateStrategyDefinitions(db *gorm.DB) error {
	switch db.Dialector.Name() {
	case "mysql":
		var engine string
		if err := db.Raw(`SELECT ENGINE FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='strategy_definitions'`).Scan(&engine).Error; err != nil {
			return err
		}
		if !strings.EqualFold(engine, "InnoDB") {
			return invalidDefinitionSchema()
		}
		var columns []struct{ Name, DataType, ColumnType, Nullable, ColumnKey, Extra string }
		if err := db.Raw(`SELECT COLUMN_NAME AS name, DATA_TYPE AS data_type, COLUMN_TYPE AS column_type, IS_NULLABLE AS nullable, COLUMN_KEY AS column_key, EXTRA AS extra FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='strategy_definitions' ORDER BY ordinal_position`).Scan(&columns).Error; err != nil {
			return err
		}
		if len(columns) != 3 {
			return invalidDefinitionSchema()
		}
		seen := map[string]bool{}
		for _, column := range columns {
			if column.Nullable != "NO" || seen[column.Name] {
				return invalidDefinitionSchema()
			}
			seen[column.Name] = true
			switch column.Name {
			case "id":
				if strings.ToLower(column.ColumnType) != "bigint unsigned" || column.ColumnKey != "PRI" || !strings.Contains(column.Extra, "auto_increment") {
					return invalidDefinitionSchema()
				}
			case "name":
				if strings.ToLower(column.ColumnType) != "varbinary(255)" {
					return invalidDefinitionSchema()
				}
			case "config_options":
				if column.DataType != "text" && column.DataType != "longtext" {
					return invalidDefinitionSchema()
				}
			default:
				return invalidDefinitionSchema()
			}
		}
		var indexes []struct {
			Name, ColumnName string
			NonUnique, Seq   int
			Prefix           *int
		}
		if err := db.Raw(`SELECT INDEX_NAME AS name, COLUMN_NAME AS column_name, NON_UNIQUE AS non_unique, SEQ_IN_INDEX AS seq, SUB_PART AS prefix FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='strategy_definitions' ORDER BY index_name,seq_in_index`).Scan(&indexes).Error; err != nil {
			return err
		}
		groups := map[string][]int{}
		for i, index := range indexes {
			groups[index.Name] = append(groups[index.Name], i)
		}
		primary := groups["PRIMARY"]
		if len(primary) != 1 || indexes[primary[0]].ColumnName != "id" || indexes[primary[0]].NonUnique != 0 || indexes[primary[0]].Prefix != nil {
			return invalidDefinitionSchema()
		}
		for _, positions := range groups {
			if len(positions) == 1 {
				index := indexes[positions[0]]
				if index.NonUnique == 0 && index.ColumnName == "name" && index.Prefix == nil {
					return nil
				}
			}
		}
		return invalidDefinitionSchema()
	case "postgres":
		var columns []struct {
			Name, DataType string
			NotNull        bool
			DefaultExpr    *string
		}
		if err := db.Raw(`SELECT a.attname AS name,pg_catalog.format_type(a.atttypid,a.atttypmod) AS data_type,a.attnotnull AS not_null,pg_get_expr(d.adbin,d.adrelid) AS default_expr FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum WHERE a.attrelid='strategy_definitions'::regclass AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`).Scan(&columns).Error; err != nil {
			return err
		}
		if len(columns) != 3 {
			return invalidDefinitionSchema()
		}
		seen := map[string]bool{}
		for _, column := range columns {
			if !column.NotNull || seen[column.Name] {
				return invalidDefinitionSchema()
			}
			seen[column.Name] = true
			switch column.Name {
			case "id":
				if column.DataType != "bigint" || column.DefaultExpr == nil || !strings.HasPrefix(*column.DefaultExpr, "nextval(") {
					return invalidDefinitionSchema()
				}
			case "name":
				if column.DataType != "bytea" {
					return invalidDefinitionSchema()
				}
			case "config_options":
				if column.DataType != "text" {
					return invalidDefinitionSchema()
				}
			default:
				return invalidDefinitionSchema()
			}
		}
		for _, key := range []string{"id", "name"} {
			var count int64
			query := `SELECT count(*) FROM pg_index i JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=i.indkey[0] WHERE i.indrelid='strategy_definitions'::regclass AND i.indisvalid AND i.indisunique AND i.indnkeyatts=1 AND i.indpred IS NULL AND i.indexprs IS NULL AND a.attname=?`
			if key == "id" {
				query += " AND i.indisprimary"
			}
			if err := db.Raw(query, key).Scan(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				return invalidDefinitionSchema()
			}
		}
		return nil
	case "sqlite":
		var columns []struct {
			Name, Type  string
			NotNull, Pk int
		}
		if err := db.Raw(`SELECT name,type,"notnull" AS not_null,pk FROM pragma_table_info(?)`, "strategy_definitions").Scan(&columns).Error; err != nil {
			return err
		}
		if len(columns) != 3 {
			return invalidDefinitionSchema()
		}
		seen := map[string]bool{}
		for _, column := range columns {
			if seen[column.Name] {
				return invalidDefinitionSchema()
			}
			seen[column.Name] = true
			switch column.Name {
			case "id":
				if strings.ToUpper(column.Type) != "INTEGER" || column.Pk != 1 {
					return invalidDefinitionSchema()
				}
			case "name":
				if strings.ToUpper(column.Type) != "BLOB" || column.NotNull != 1 || column.Pk != 0 {
					return invalidDefinitionSchema()
				}
			case "config_options":
				if strings.ToUpper(column.Type) != "TEXT" || column.NotNull != 1 || column.Pk != 0 {
					return invalidDefinitionSchema()
				}
			default:
				return invalidDefinitionSchema()
			}
		}
		var ddl string
		if err := db.Raw("SELECT sql FROM sqlite_master WHERE type='table' AND name='strategy_definitions'").Scan(&ddl).Error; err != nil {
			return err
		}
		compact := strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return unicode.ToLower(r)
		}, ddl)
		if strings.Contains(compact, "--") || strings.Contains(compact, "/*") || !strings.Contains(compact, "primarykeyautoincrement") || !strings.Contains(compact, "check(typeof(name)='blob')") {
			return invalidDefinitionSchema()
		}
		var indexes []struct {
			Name            string
			Unique, Partial int
		}
		if err := db.Raw(`SELECT name,"unique" AS "unique",partial FROM pragma_index_list(?)`, "strategy_definitions").Scan(&indexes).Error; err != nil {
			return err
		}
		for _, index := range indexes {
			if index.Unique != 1 || index.Partial != 0 {
				continue
			}
			var keys []struct{ Name string }
			if err := db.Raw("SELECT name FROM pragma_index_info(?) ORDER BY seqno", index.Name).Scan(&keys).Error; err != nil {
				return err
			}
			if len(keys) == 1 && keys[0].Name == "name" {
				return nil
			}
		}
		return invalidDefinitionSchema()
	}
	return invalidDefinitionSchema()
}
