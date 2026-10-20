package database

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

const peerDefinitionColumn = "strategy_definition_id"
const peerDefinitionConstraint = "fk_peers_strategy_definition"
const peerDefinitionIndex = "idx_peers_strategy_definition_id"

func invalidPeerDefinitionSchema() error {
	return fmt.Errorf("invalid existing peer strategy definition schema")
}

// The scalar field is deliberately excluded from AutoMigrate. Adding the SQLite
// nullable reference in one ALTER preserves peers, its dependants and triggers.
func migratePeerStrategyDefinition(db *gorm.DB) error {
	present, err := peerDefinitionColumnPresent(db)
	if err != nil {
		return err
	}
	if !present {
		var sql string
		switch db.Dialector.Name() {
		case "sqlite":
			sql = "ALTER TABLE peers ADD COLUMN strategy_definition_id INTEGER NULL REFERENCES strategy_definitions(id) ON UPDATE RESTRICT ON DELETE RESTRICT"
		case "mysql":
			sql = "ALTER TABLE peers ADD COLUMN strategy_definition_id BIGINT UNSIGNED NULL"
		case "postgres":
			sql = "ALTER TABLE peers ADD COLUMN strategy_definition_id BIGINT NULL"
		default:
			return invalidPeerDefinitionSchema()
		}
		if err := db.Exec(sql).Error; err != nil {
			return fmt.Errorf("add peer strategy definition: %w", err)
		}
	}
	if present, err = peerDefinitionColumnPresent(db); err != nil {
		return err
	} else if !present {
		return invalidPeerDefinitionSchema()
	}
	valid, err := peerDefinitionForeignKey(db)
	if err != nil {
		return err
	}
	if !valid {
		if db.Dialector.Name() == "sqlite" {
			return invalidPeerDefinitionSchema()
		}
		if err := db.Exec("ALTER TABLE peers ADD CONSTRAINT " + peerDefinitionConstraint + " FOREIGN KEY (strategy_definition_id) REFERENCES strategy_definitions(id) ON UPDATE RESTRICT ON DELETE RESTRICT").Error; err != nil {
			return fmt.Errorf("add peer strategy foreign key: %w", err)
		}
		if valid, err = peerDefinitionForeignKey(db); err != nil {
			return err
		} else if !valid {
			return invalidPeerDefinitionSchema()
		}
	}
	indexed, err := peerDefinitionIndexed(db)
	if err != nil {
		return err
	}
	if !indexed {
		if err := db.Exec("CREATE INDEX " + peerDefinitionIndex + " ON peers(strategy_definition_id)").Error; err != nil {
			return fmt.Errorf("index peer strategy definition: %w", err)
		}
		if indexed, err = peerDefinitionIndexed(db); err != nil {
			return err
		} else if !indexed {
			return invalidPeerDefinitionSchema()
		}
	}
	return nil
}

func peerDefinitionColumnPresent(db *gorm.DB) (bool, error) {
	switch db.Dialector.Name() {
	case "sqlite":
		var rows []struct {
			Name, Type   string
			NotNull      int
			DefaultValue *string
		}
		if err := db.Raw(`SELECT name,type,"notnull" AS not_null,dflt_value AS default_value FROM pragma_table_info('peers') WHERE name=?`, peerDefinitionColumn).Scan(&rows).Error; err != nil {
			return false, err
		}
		if len(rows) == 0 {
			return false, nil
		}
		if len(rows) != 1 || !strings.EqualFold(rows[0].Type, "INTEGER") || rows[0].NotNull != 0 || (rows[0].DefaultValue != nil && !strings.EqualFold(*rows[0].DefaultValue, "NULL")) {
			return false, invalidPeerDefinitionSchema()
		}
	case "mysql":
		var rows []struct {
			ColumnType, Nullable string
			DefaultValue         *string
		}
		if err := db.Raw(`SELECT COLUMN_TYPE AS column_type, IS_NULLABLE AS nullable,COLUMN_DEFAULT AS default_value FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='peers' AND column_name=?`, peerDefinitionColumn).Scan(&rows).Error; err != nil {
			return false, err
		}
		if len(rows) == 0 {
			return false, nil
		}
		if len(rows) != 1 || !strings.EqualFold(rows[0].ColumnType, "bigint unsigned") || rows[0].Nullable != "YES" || rows[0].DefaultValue != nil {
			return false, invalidPeerDefinitionSchema()
		}
	case "postgres":
		var rows []struct {
			DataType, Nullable string
			DefaultValue       *string
		}
		if err := db.Raw(`SELECT data_type,is_nullable AS nullable,column_default AS default_value FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='peers' AND column_name=?`, peerDefinitionColumn).Scan(&rows).Error; err != nil {
			return false, err
		}
		if len(rows) == 0 {
			return false, nil
		}
		if len(rows) != 1 || rows[0].DataType != "bigint" || rows[0].Nullable != "YES" || (rows[0].DefaultValue != nil && !strings.EqualFold(*rows[0].DefaultValue, "NULL::bigint")) {
			return false, invalidPeerDefinitionSchema()
		}
	default:
		return false, invalidPeerDefinitionSchema()
	}
	return true, nil
}

func peerDefinitionForeignKey(db *gorm.DB) (bool, error) {
	switch db.Dialector.Name() {
	case "sqlite":
		var rows []struct {
			ID, Seq                             int
			Table, From, To, OnUpdate, OnDelete string
		}
		if err := db.Raw(`SELECT id,seq,"table","from","to",on_update,on_delete FROM pragma_foreign_key_list('peers')`).Scan(&rows).Error; err != nil {
			return false, err
		}
		found := false
		for _, row := range rows {
			if row.From == peerDefinitionColumn {
				if found || row.Seq != 0 || row.Table != "strategy_definitions" || row.To != "id" || row.OnUpdate != "RESTRICT" || row.OnDelete != "RESTRICT" {
					return false, invalidPeerDefinitionSchema()
				}
				for _, other := range rows {
					if other.ID == row.ID && other.Seq != 0 {
						return false, invalidPeerDefinitionSchema()
					}
				}
				found = true
			}
		}
		return found, nil
	case "mysql":
		var rows []struct {
			Name, Source, TargetTable, Target, DeleteRule, UpdateRule string
			SameSchema                                                bool
		}
		if err := db.Raw(`SELECT k.CONSTRAINT_NAME AS name,k.COLUMN_NAME AS source,k.REFERENCED_TABLE_NAME AS target_table,k.REFERENCED_COLUMN_NAME AS target,r.DELETE_RULE AS delete_rule,r.UPDATE_RULE AS update_rule,(k.REFERENCED_TABLE_SCHEMA=DATABASE()) AS same_schema FROM information_schema.key_column_usage k JOIN information_schema.referential_constraints r ON r.CONSTRAINT_SCHEMA=k.CONSTRAINT_SCHEMA AND r.CONSTRAINT_NAME=k.CONSTRAINT_NAME AND r.TABLE_NAME=k.TABLE_NAME WHERE k.TABLE_SCHEMA=DATABASE() AND k.TABLE_NAME='peers'`).Scan(&rows).Error; err != nil {
			return false, err
		}
		found := false
		for _, row := range rows {
			if row.Source == peerDefinitionColumn || row.Name == peerDefinitionConstraint {
				if found || row.Source != peerDefinitionColumn || row.TargetTable != "strategy_definitions" || row.Target != "id" || !row.SameSchema || row.DeleteRule != "RESTRICT" || row.UpdateRule != "RESTRICT" {
					return false, invalidPeerDefinitionSchema()
				}
				count := 0
				for _, other := range rows {
					if other.Name == row.Name {
						count++
					}
				}
				if count != 1 {
					return false, invalidPeerDefinitionSchema()
				}
				found = true
			}
		}
		return found, nil
	case "postgres":
		var rows []struct {
			Name, Source, Target, DeleteRule, UpdateRule string
			Single, SameTable, Valid                     bool
		}
		if err := db.Raw(`SELECT c.conname AS name,c.convalidated AS valid,a.attname AS source,b.attname AS target,c.confdeltype::text AS delete_rule,c.confupdtype::text AS update_rule,(cardinality(c.conkey)=1 AND cardinality(c.confkey)=1) AS single,(c.confrelid='strategy_definitions'::regclass) AS same_table FROM pg_constraint c JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=ANY(c.conkey) JOIN pg_attribute b ON b.attrelid=c.confrelid AND b.attnum=c.confkey[1] WHERE c.conrelid='peers'::regclass AND c.contype='f'`).Scan(&rows).Error; err != nil {
			return false, err
		}
		found := false
		for _, row := range rows {
			if row.Source == peerDefinitionColumn || row.Name == peerDefinitionConstraint {
				if found || row.Source != peerDefinitionColumn || row.Target != "id" || !row.Single || !row.SameTable || !row.Valid || row.DeleteRule != "r" || row.UpdateRule != "r" {
					return false, invalidPeerDefinitionSchema()
				}
				found = true
			}
		}
		return found, nil
	}
	return false, invalidPeerDefinitionSchema()
}

func peerDefinitionIndexed(db *gorm.DB) (bool, error) {
	found := false
	switch db.Dialector.Name() {
	case "sqlite":
		var indexes []struct {
			Name            string
			Unique, Partial int
		}
		if err := db.Raw(`SELECT name,"unique" AS "unique",partial FROM pragma_index_list('peers')`).Scan(&indexes).Error; err != nil {
			return false, err
		}
		for _, index := range indexes {
			var keys []struct{ Name string }
			if err := db.Raw(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index.Name).Scan(&keys).Error; err != nil {
				return false, err
			}
			if len(keys) == 1 && keys[0].Name == peerDefinitionColumn {
				if index.Unique != 0 || index.Partial != 0 {
					return false, invalidPeerDefinitionSchema()
				}
				found = true
			} else if index.Name == peerDefinitionIndex {
				return false, invalidPeerDefinitionSchema()
			}
		}
	case "mysql":
		var rows []struct {
			Name, ColumnName string
			NonUnique        int
			Prefix           *int
		}
		if err := db.Raw(`SELECT INDEX_NAME AS name,COLUMN_NAME AS column_name,NON_UNIQUE AS non_unique,SUB_PART AS prefix FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='peers' ORDER BY index_name,seq_in_index`).Scan(&rows).Error; err != nil {
			return false, err
		}
		groups := map[string][]int{}
		for i, row := range rows {
			groups[row.Name] = append(groups[row.Name], i)
		}
		for name, positions := range groups {
			if len(positions) == 1 && rows[positions[0]].ColumnName == peerDefinitionColumn {
				row := rows[positions[0]]
				if row.NonUnique != 1 || row.Prefix != nil {
					return false, invalidPeerDefinitionSchema()
				}
				found = true
			} else if name == peerDefinitionIndex {
				return false, invalidPeerDefinitionSchema()
			}
		}
	case "postgres":
		var rows []struct {
			Name, ColumnName            string
			NonUnique, Single, Complete bool
		}
		if err := db.Raw(`SELECT x.relname AS name,a.attname AS column_name,NOT i.indisunique AS non_unique,(i.indnkeyatts=1) AS single,(i.indisvalid AND i.indpred IS NULL AND i.indexprs IS NULL) AS complete FROM pg_index i JOIN pg_class x ON x.oid=i.indexrelid LEFT JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=i.indkey[0] WHERE i.indrelid='peers'::regclass`).Scan(&rows).Error; err != nil {
			return false, err
		}
		for _, row := range rows {
			if row.Single && row.ColumnName == peerDefinitionColumn {
				if !row.NonUnique || !row.Complete {
					return false, invalidPeerDefinitionSchema()
				}
				found = true
			} else if row.Name == peerDefinitionIndex {
				return false, invalidPeerDefinitionSchema()
			}
		}
	default:
		return false, invalidPeerDefinitionSchema()
	}
	return found, nil
}
