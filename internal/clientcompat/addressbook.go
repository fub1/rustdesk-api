package clientcompat

import (
	"context"
	"encoding/json"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
	"gorm.io/gorm"
)

func (s *Store) Peers(ctx context.Context, actor Actor, raw string, page Page) (int64, []Peer, error) {
	rows := []Peer{}
	var total int64
	if !page.valid() {
		return 0, rows, ErrInput
	}
	err := s.transaction(ctx, func(tx *gorm.DB) error {
		u, err := current(tx, actor)
		if err != nil {
			return err
		}
		uid, cid, err := scope(tx, u, raw, 1)
		if err != nil {
			return err
		}
		query := tx.Model(&peerRecord{}).Where("user_id = ? AND collection_id = ?", uid, cid)
		if err := query.Count(&total).Error; err != nil {
			return err
		}
		var stored []peerRecord
		if err := query.Order("row_id ASC").Offset(page.offset()).Limit(page.Size).Find(&stored).Error; err != nil {
			return err
		}
		for _, row := range stored {
			dto, err := peerDTO(row)
			if err != nil {
				return err
			}
			rows = append(rows, dto)
		}
		return nil
	})
	return total, rows, err
}

func tags(tx *gorm.DB, uid, cid uint) ([]Tag, error) {
	rows := []Tag{}
	err := tx.Model(&model.Tag{}).Where("user_id = ? AND collection_id = ?", uid, cid).Order("id ASC").Select("name, color").Find(&rows).Error
	return rows, err
}
func (s *Store) Tags(ctx context.Context, actor Actor, raw string) ([]Tag, error) {
	rows := []Tag{}
	err := s.transaction(ctx, func(tx *gorm.DB) error {
		u, err := current(tx, actor)
		if err != nil {
			return err
		}
		uid, cid, err := scope(tx, u, raw, 1)
		if err != nil {
			return err
		}
		rows, err = tags(tx, uid, cid)
		return err
	})
	return rows, err
}
func (s *Store) Personal(ctx context.Context, actor Actor) ([]Peer, []Tag, error) {
	peers := []Peer{}
	labels := []Tag{}
	err := s.transaction(ctx, func(tx *gorm.DB) error {
		u, err := current(tx, actor)
		if err != nil {
			return err
		}
		var stored []peerRecord
		if err := tx.Model(&peerRecord{}).Where("user_id = ? AND collection_id = 0", u.Id).Order("row_id ASC").Find(&stored).Error; err != nil {
			return err
		}
		for _, row := range stored {
			dto, err := peerDTO(row)
			if err != nil {
				return err
			}
			peers = append(peers, dto)
		}
		labels, err = tags(tx, u.Id, 0)
		return err
	})
	return peers, labels, err
}

func (s *Store) Profiles(ctx context.Context, actor Actor, page Page) (int64, []Profile, error) {
	rows := []Profile{}
	var total int64
	if !page.valid() {
		return 0, rows, ErrInput
	}
	err := s.transaction(ctx, func(tx *gorm.DB) error {
		u, err := current(tx, actor)
		if err != nil {
			return err
		}
		grant := "r.collection_id = c.id AND r.user_id = c.user_id AND r.rule BETWEEN 1 AND 3 AND ((r.type = 1 AND r.to_id = ?) OR (r.type = 2 AND r.to_id = ?))"
		query := tx.Table("address_book_collections AS c").Joins("JOIN users AS owner ON owner.id = c.user_id").Where("c.user_id = ? OR EXISTS (SELECT 1 FROM address_book_collection_rules AS r WHERE "+grant+")", u.Id, u.Id, u.GroupId)
		if err := query.Count(&total).Error; err != nil {
			return err
		}
		var stored []struct {
			ID, UserID, GroupID uint
			Name, Owner         string
			SharedRule          int
		}
		selection := "c.id, c.user_id, owner.group_id AS group_id, c.name, owner.username AS owner, COALESCE((SELECT MAX(r.rule) FROM address_book_collection_rules AS r WHERE " + grant + "), 0) AS shared_rule"
		if err := query.Select(selection, u.Id, u.GroupId).Order("c.id ASC").Offset(page.offset()).Limit(page.Size).Scan(&stored).Error; err != nil {
			return err
		}
		for _, v := range stored {
			rule := v.SharedRule
			if v.UserID == u.Id {
				rule = 3
			}
			rows = append(rows, Profile{GUID: guid(v.GroupID, v.UserID, v.ID), Name: v.Name, Owner: v.Owner, Rule: rule})
		}
		return nil
	})
	return total, rows, err
}

func patchMap(p Patch) (map[string]any, error) {
	if p.ID == "" {
		return nil, ErrInput
	}
	values := map[string]any{}
	for name, field := range map[string]StringField{"alias": p.Alias, "hash": p.Hash, "password": p.Password, "username": p.Username, "hostname": p.Hostname, "platform": p.Platform, "note": p.Note} {
		if field.Present {
			values[name] = field.Value
		}
	}
	if p.Tags.Present {
		value := p.Tags.Value
		if value == nil {
			value = []string{}
		}
		tags, err := json.Marshal(value)
		if err != nil {
			return nil, ErrInput
		}
		values["tags"] = string(tags)
	}
	if len(values) == 0 {
		return nil, ErrInput
	}
	return values, nil
}
func (s *Store) Patch(ctx context.Context, actor Actor, raw string, p Patch) error {
	values, err := patchMap(p)
	if err != nil {
		return err
	}
	return s.transaction(ctx, func(tx *gorm.DB) error {
		uid, cid, err := writeScope(tx, actor, raw, 2)
		if err != nil {
			return err
		}
		var rows []model.AddressBook
		if err := lockRows(tx).Where("user_id = ? AND collection_id = ? AND id = ?", uid, cid, p.ID).Order("row_id ASC").Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return ErrNotFound
		}
		// Historical duplicate IDs are retained; the client's ID patch updates all
		// rows in this exact owner/collection scope, without creating a unique key.
		return tx.Model(&model.AddressBook{}).Where("user_id = ? AND collection_id = ? AND id = ?", uid, cid, p.ID).Updates(values).Error
	})
}

func inputRow(tx *gorm.DB, p PeerInput, uid, cid uint, enrich bool) (model.AddressBook, error) {
	if p.ID == "" {
		return model.AddressBook{}, ErrInput
	}
	if p.Tags.Value == nil {
		p.Tags.Value = []string{}
	}
	tags, err := json.Marshal(p.Tags.Value)
	if err != nil {
		return model.AddressBook{}, ErrInput
	}
	row := model.AddressBook{Id: p.ID, Hash: p.Hash.Value, Password: p.Password.Value, Username: p.Username.Value, Hostname: p.Hostname.Value, Platform: p.Platform.Value, Alias: p.Alias.Value, Note: p.Note.Value, Tags: custom_types.AutoJson(tags), UserId: uid, CollectionId: cid, ForceAlwaysRelay: p.ForceAlwaysRelay.Value, RdpPort: p.RdpPort.Value, RdpUsername: p.RdpUsername.Value, LoginName: p.LoginName.Value, SameServer: p.SameServer.Value}
	if enrich && (row.Username == "" || row.Hostname == "" || row.Platform == "") {
		var peer model.Peer
		// A guessed device ID is not permission to read another owner's host
		// metadata. Enrichment is limited to this address book owner's devices.
		err := tx.Where("id = ? AND user_id = ?", p.ID, uid).First(&peer).Error
		if err != nil && err != gorm.ErrRecordNotFound {
			return model.AddressBook{}, err
		}
		if err == nil {
			if row.Username == "" {
				row.Username = peer.Username
			}
			if row.Hostname == "" {
				row.Hostname = peer.Hostname
			}
			if row.Platform == "" {
				row.Platform = platform(peer.Os)
			}
		}
	}
	return row, nil
}
func platform(os string) string {
	for _, p := range []struct{ match, value string }{{"android", "Android"}, {"windows", "Windows"}, {"linux", "Linux"}, {"mac", "Mac OS"}} {
		if containsOS(os, p.match) {
			return p.value
		}
	}
	return ""
}
func (s *Store) Add(ctx context.Context, actor Actor, raw string, p PeerInput) error {
	return s.transaction(ctx, func(tx *gorm.DB) error {
		uid, cid, err := writeScope(tx, actor, raw, 2)
		if err != nil {
			return err
		}
		row, err := inputRow(tx, p, uid, cid, true)
		if err != nil {
			return err
		}
		return tx.Create(&row).Error
	})
}

// ReplacePersonal is the legacy whole-document protocol, limited to the
// actor's collection_id=0 peers AND tags. No helper escapes this transaction.
func (s *Store) ReplacePersonal(ctx context.Context, actor Actor, doc Document) error {
	// A partial/null document is not an instruction to erase a whole book.
	// Sciter's initial/reset snapshots omit optional color metadata, but still
	// contain both fact arrays. Explicit invalid metadata is never omission.
	if doc.Peers == nil || doc.Tags == nil {
		return ErrInput
	}
	colors := map[string]uint{}
	if doc.TagColors.Present {
		var provided map[string]*uint
		if json.Unmarshal([]byte(doc.TagColors.Value), &provided) != nil || provided == nil {
			return ErrInput
		}
		for name, color := range provided {
			if color == nil {
				return ErrInput
			}
			colors[name] = *color
		}
	}
	seen := map[string]bool{}
	for _, p := range doc.Peers {
		if p.ID == "" || seen[p.ID] {
			return ErrInput
		}
		seen[p.ID] = true
	}
	return s.transaction(ctx, func(tx *gorm.DB) error {
		u, err := writeActor(tx, actor, actor.ID)
		if err != nil {
			return err
		}
		var existing []model.AddressBook
		if err := lockRows(tx).Where("user_id = ? AND collection_id = 0", u.Id).Order("row_id ASC").Find(&existing).Error; err != nil {
			return err
		}
		present := map[string]bool{}
		for _, row := range existing {
			present[row.Id] = true
			if !seen[row.Id] {
				if err := tx.Delete(&row).Error; err != nil {
					return err
				}
			}
		}
		for _, p := range doc.Peers {
			row, err := inputRow(tx, p, u.Id, 0, !present[p.ID])
			if err != nil {
				return err
			}
			if !present[p.ID] {
				if err := tx.Create(&row).Error; err != nil {
					return err
				}
				continue
			}
			values := map[string]any{}
			for name, field := range map[string]StringField{"username": p.Username, "password": p.Password, "hostname": p.Hostname, "alias": p.Alias, "platform": p.Platform, "hash": p.Hash, "rdp_port": p.RdpPort, "rdp_username": p.RdpUsername, "login_name": p.LoginName, "note": p.Note} {
				if field.Present {
					values[name] = field.Value
				}
			}
			if p.Tags.Present {
				values["tags"] = string(row.Tags)
			}
			if p.ForceAlwaysRelay.Present {
				values["force_always_relay"] = row.ForceAlwaysRelay
			}
			if p.SameServer.Present {
				values["same_server"] = row.SameServer
			}
			if len(values) == 0 {
				continue
			}
			if err := tx.Model(&model.AddressBook{}).Where("user_id = ? AND collection_id = 0 AND id = ?", u.Id, p.ID).Updates(values).Error; err != nil {
				return err
			}
		}
		var previousTags []model.Tag
		if err := lockRows(tx).Where("user_id = ? AND collection_id = 0", u.Id).Order("id ASC").Find(&previousTags).Error; err != nil {
			return err
		}
		previousColors := map[string]uint{}
		for _, tag := range previousTags {
			if _, exists := previousColors[tag.Name]; !exists {
				previousColors[tag.Name] = tag.Color
			}
		}
		if err := tx.Where("user_id = ? AND collection_id = 0", u.Id).Delete(&model.Tag{}).Error; err != nil {
			return err
		}
		// The tags array is the fact set; tag_colors is only metadata. Native
		// clients can retain stale colors after deleting a tag. Missing colors
		// preserve an existing value, or use the model's zero color for a new tag.
		seenTags := map[string]bool{}
		for _, name := range doc.Tags {
			if seenTags[name] {
				continue
			}
			seenTags[name] = true
			color, provided := colors[name]
			if !provided {
				color = previousColors[name]
			}
			if err := tx.Create(&model.Tag{UserId: u.Id, CollectionId: 0, Name: name, Color: color}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
