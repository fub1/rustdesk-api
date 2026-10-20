// Package clientstrategy produces the fixed client's incremental configuration
// protocol from a durable device history and an explicit database dependency.
package clientstrategy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/lejianwen/rustdesk-api/v2/internal/strategydefinitions"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

var (
	ErrNotFound    = errors.New("strategy device not found")
	ErrUnavailable = errors.New("strategy storage unavailable")
)

type Store struct{ db *gorm.DB }

type Result struct {
	Peer          model.Peer
	ConfigOptions map[string]string
	Revision      int64
	Changed       bool
}

func New(db *gorm.DB) (*Store, error) {
	if db == nil || db.Error != nil {
		return nil, ErrUnavailable
	}
	// Strategy values may contain credentials. Neither failed SQL nor malformed
	// documents should print them through a caller-provided SQL logger.
	return &Store{db: db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})}, nil
}

// Heartbeat commits the union of keys before returning any payload. A lost
// response or process restart therefore cannot forget keys requiring clearing.
// History stays managed: removed keys remain explicit empty strings forever.
func (s *Store) Heartbeat(ctx context.Context, id, uuid string, known int64) (Result, error) {
	var result Result
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if id == "" || uuid == "" {
		return result, ErrNotFound
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx
		if tx.Dialector.Name() != "sqlite" {
			// The existing parent is also the lock for a not-yet-created state.
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := query.Where("id = ? AND uuid = ?", id, uuid).First(&result.Peer).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		defaultID := model.UnboundDefaultPeerId
		if result.Peer.Alias != "" {
			defaultID = ""
		}
		// Read text directly. Legacy AutoJson.Scan intentionally hides malformed
		// JSON, which must not become a successful empty policy here.
		var policies []struct {
			PeerID        string `gorm:"column:peer_id"`
			ConfigOptions string
		}
		if err := tx.Model(&model.PeerStrategy{}).Select("peer_id, config_options").Where("peer_id IN ?", []string{defaultID, id}).Find(&policies).Error; err != nil {
			return err
		}
		defaults, device := map[string]string{}, map[string]string{}
		for _, row := range policies {
			options, err := parseOptions(row.ConfigOptions)
			if err != nil {
				return err
			}
			if row.PeerID == defaultID {
				defaults = options
			} else {
				device = options
			}
		}
		// Named policy is a live reference. Read it for every heartbeat while
		// holding the peer first, matching CLI's peer -> definition order.
		if result.Peer.StrategyDefinitionID != nil {
			if *result.Peer.StrategyDefinitionID == 0 {
				return ErrUnavailable
			}
			var definition model.StrategyDefinition
			query := tx
			if tx.Dialector.Name() != "sqlite" {
				query = query.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			if err := query.First(&definition, *result.Peer.StrategyDefinitionID).Error; err != nil {
				return err
			}
			named, err := strategydefinitions.DecodeOptions([]byte(definition.ConfigOptions))
			if err != nil {
				return ErrUnavailable
			}
			for key, value := range named {
				defaults[key] = value
			}
		}
		for key, value := range device {
			defaults[key] = value
		}
		current := defaults
		// Only a real assignment supplies current preset values. Missing
		// assignments must neither claim previously unmanaged local keys nor
		// overwrite an explicitly configured policy fallback with empty strings.
		if result.Peer.Alias != "" || result.Peer.AbId != 0 {
			current["preset-address-book-alias"] = result.Peer.Alias
		}
		if result.Peer.AbId != 0 {
			var collection model.AddressBookCollection
			if err := tx.First(&collection, result.Peer.AbId).Error; err != nil {
				return err
			}
			current["preset-address-book-name"] = collection.Name
			current["preset-device-username"] = result.Peer.Username
			current["preset-device-name"] = result.Peer.Hostname
		}
		var state model.PeerStrategyState
		err := tx.First(&state, "peer_row_id = ?", result.Peer.RowId).Error
		fresh := errors.Is(err, gorm.ErrRecordNotFound)
		if err != nil && !fresh {
			return err
		}
		history := []string{}
		if !fresh {
			if err := json.Unmarshal([]byte(state.ManagedKeys), &history); err != nil || history == nil {
				return ErrUnavailable
			}
		}
		keys := make(map[string]struct{}, len(history)+len(current))
		for _, key := range history {
			if key == "" {
				return ErrUnavailable
			}
			keys[key] = struct{}{}
		}
		for key := range current {
			keys[key] = struct{}{}
		}
		names := make([]string, 0, len(keys))
		for key := range keys {
			names = append(names, key)
			if _, exists := current[key]; !exists {
				current[key] = ""
			}
		}
		sort.Strings(names)
		encoded, err := json.Marshal(names)
		if err != nil {
			return err
		}
		if fresh {
			state = model.PeerStrategyState{PeerRowID: result.Peer.RowId, ManagedKeys: string(encoded)}
			if err := tx.Create(&state).Error; err != nil {
				return err
			}
		} else if state.ManagedKeys != string(encoded) {
			if err := tx.Model(&state).Update("managed_keys", string(encoded)).Error; err != nil {
				return err
			}
		}
		// encoding/json sorts string map keys. Timestamp ordering, preset cache
		// fields and the order administrators entered JSON do not affect revision.
		payload, err := json.Marshal(current)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(payload)
		revision := int64(binary.BigEndian.Uint64(digest[:8]) & ((1 << 63) - 1))
		if revision == 0 {
			revision = 1
		}
		result.ConfigOptions, result.Revision, result.Changed = current, revision, known != revision
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		// Never leak a partial payload or underlying provider/SQL error.
		for _, safe := range []error{context.Canceled, context.DeadlineExceeded, ErrNotFound} {
			if errors.Is(err, safe) {
				return Result{}, safe
			}
		}
		return Result{}, ErrUnavailable
	}
	return result, nil
}

func parseOptions(raw string) (map[string]string, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &values); err != nil || values == nil {
		return nil, ErrUnavailable
	}
	options := make(map[string]string, len(values))
	for key, value := range values {
		var text string
		if key == "" || !strings.HasPrefix(strings.TrimSpace(string(value)), "\"") || json.Unmarshal(value, &text) != nil {
			return nil, ErrUnavailable
		}
		options[key] = text
	}
	return options, nil
}
