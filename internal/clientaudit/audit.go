// Package clientaudit stores controlled-client audit posts. Its database and
// request contexts are explicit; it starts no workers and reads no globals.
package clientaudit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

var (
	ErrInvalidInput      = errors.New("invalid audit request")
	ErrUnavailable       = errors.New("audit storage unavailable")
	ErrMissingConnection = errors.New("audit connection not yet stored")
	ErrConflict          = errors.New("audit nonce conflicts with stored event")
)

type Service struct {
	db *gorm.DB
}

func New(db *gorm.DB) (*Service, error) {
	if db == nil || db.Error != nil {
		return nil, ErrUnavailable
	}
	return &Service{db: db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})}, nil
}

// The receipt's primary key arbitrates concurrent writers. A failed insert or
// COMMIT ends its transaction before checking a committed receipt: PostgreSQL
// forbids querying again inside a transaction aborted by a unique violation.
func (s *Service) write(ctx context.Context, receipt *model.AuditDeliveryReceipt, operation func(*gorm.DB) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if receipt != nil {
			if err := tx.Create(receipt).Error; err != nil {
				return err
			}
		}
		return operation(tx)
	})
	if err == nil {
		return nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if receipt != nil {
		var committed model.AuditDeliveryReceipt
		lookup := s.db.WithContext(ctx).Where(map[string]interface{}{"key": receipt.Key}).Take(&committed).Error
		if lookup == nil {
			if committed.PayloadHash != receipt.PayloadHash {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(lookup, gorm.ErrRecordNotFound) {
			return classify(lookup)
		}
	}
	return classify(err)
}

func delivery(kind, device, uuid, nonce string, payload interface{}) (*model.AuditDeliveryReceipt, error) {
	if nonce == "" {
		return nil, nil
	} // No nonce is explicitly best effort.
	key, err := json.Marshal([]string{kind, device, uuid, nonce})
	if err != nil {
		return nil, ErrInvalidInput
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, ErrInvalidInput
	}
	keyHash, payloadHash := sha256.Sum256(key), sha256.Sum256(body)
	return &model.AuditDeliveryReceipt{Key: hex.EncodeToString(keyHash[:]), PayloadHash: hex.EncodeToString(payloadHash[:])}, nil
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, ErrInvalidInput, ErrMissingConnection, ErrConflict} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrUnavailable
}

func (s *Service) Conn(ctx context.Context, event model.AuditConn) error {
	session, err := strconv.ParseUint(event.SessionId, 10, 64)
	if err != nil || strconv.FormatUint(session, 10) != event.SessionId || event.PeerId == "" || event.Uuid == "" || event.ConnId <= 0 ||
		event.Type < 0 || event.Type > 4 || event.PrimaryAuth < 0 || event.PrimaryAuth > 4 || event.TwoFactor < 0 || event.TwoFactor > 2 ||
		(event.Action != model.AuditActionNew && event.Action != model.AuditActionClose && event.Action != "") {
		return ErrInvalidInput
	}
	receipt, err := delivery("conn", event.PeerId, event.Uuid, event.Nonce, map[string]interface{}{
		"action": event.Action, "id": event.PeerId, "uuid": event.Uuid, "conn_id": event.ConnId, "session_id": event.SessionId,
		"peer": []string{event.FromPeer, event.FromName}, "ip": event.Ip, "type": event.Type, "nonce": event.Nonce,
		"conn_audit_ref": event.ConnAuditRef, "primary_auth": event.PrimaryAuth, "two_factor": event.TwoFactor,
	})
	if err != nil {
		return err
	}
	return s.write(ctx, receipt, func(tx *gorm.DB) error {
		if event.Action == model.AuditActionNew {
			return tx.Create(&event).Error
		}
		row, err := connection(tx, event)
		if err != nil {
			return err
		}
		if event.Action == model.AuditActionClose {
			if row.CloseTime != 0 {
				return nil
			}
			updates := map[string]interface{}{"close_time": time.Now().UTC().Unix()}
			if session != 0 {
				updates["session_id"] = event.SessionId
			}
			result := tx.Model(&model.AuditConn{}).Where("id = ? AND close_time = 0", row.Id).Updates(updates)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrMissingConnection
			}
			return nil
		}
		// Map updates preserve valid zero values, including remote type=0 and
		// absent authentication factors representing None=0.
		if row.FromPeer == event.FromPeer && row.FromName == event.FromName && row.SessionId == event.SessionId &&
			row.Type == event.Type && row.PrimaryAuth == event.PrimaryAuth && row.TwoFactor == event.TwoFactor {
			return nil
		}
		result := tx.Model(&model.AuditConn{}).Where("id = ?", row.Id).Updates(map[string]interface{}{
			"from_peer": event.FromPeer, "from_name": event.FromName, "session_id": event.SessionId,
			"type": event.Type, "primary_auth": event.PrimaryAuth, "two_factor": event.TwoFactor,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrMissingConnection
		}
		return nil
	})
}

func connection(tx *gorm.DB, event model.AuditConn) (model.AuditConn, error) {
	var row model.AuditConn
	base := func() *gorm.DB {
		q := tx.Where("peer_id = ? AND uuid = ? AND conn_id = ?", event.PeerId, event.Uuid, event.ConnId)
		if tx.Dialector.Name() != "sqlite" {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		return q
	}
	// A crashed process can leave an exact session open while its restarted
	// replacement reuses conn_id and the controller's session_id. Both an exact
	// live row and a not-yet-authorized live open are candidates; the latest
	// stored open wins. A different nonzero session is never a candidate.
	// Committed receipt replay has already been rejected before this lookup.
	// The wire has no open-generation or event occurrence time, so an unseen
	// old delivery (including pre-upgrade/no-receipt traffic) remains ambiguous;
	// this rule does not claim exactly-once business ordering.
	candidates := []string{"", "0"}
	if event.SessionId != "0" {
		candidates = append(candidates, event.SessionId)
	}
	err := base().Where("session_id IN ? AND close_time = 0", candidates).Order("id DESC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if event.SessionId == "0" {
			err = base().Where("session_id IN ?", []string{"", "0"}).Order("id DESC").First(&row).Error
		} else {
			err = base().Where("session_id = ?", event.SessionId).Order("id DESC").First(&row).Error
		}
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return row, ErrMissingConnection
	}
	return row, err
}

// FileSource preserves the two wire values transformed by the legacy filename
// adapter. They belong in the receipt fingerprint before that transformation.
type FileSource struct {
	Info   string
	IsFile bool
}

func (s *Service) File(ctx context.Context, event model.AuditFile, source ...FileSource) error {
	var info map[string]json.RawMessage
	if event.PeerId == "" || event.Uuid == "" || event.ConnId <= 0 || event.Type < 0 || event.Type > 1 || json.Unmarshal([]byte(event.Info), &info) != nil || info == nil {
		return ErrInvalidInput
	}
	if len(source) > 1 {
		return ErrInvalidInput
	}
	raw := FileSource{Info: event.Info, IsFile: event.IsFile}
	if len(source) == 1 {
		raw = source[0]
	}
	var original map[string]interface{}
	decoder := json.NewDecoder(strings.NewReader(raw.Info))
	decoder.UseNumber()
	if decoder.Decode(&original) != nil || original == nil || !json.Valid([]byte(raw.Info)) {
		return ErrInvalidInput
	}
	receipt, err := delivery("file", event.PeerId, event.Uuid, event.Nonce, map[string]interface{}{
		"id": event.PeerId, "uuid": event.Uuid, "conn_id": event.ConnId, "nonce": event.Nonce,
		"peer_id": event.FromPeer, "type": event.Type, "path": event.Path, "is_file": raw.IsFile, "info": original,
	})
	if err != nil {
		return err
	}
	return s.write(ctx, receipt, func(tx *gorm.DB) error {
		return tx.Create(&event).Error
	})
}
