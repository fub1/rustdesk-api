package service

import (
	"testing"

	adminRequest "github.com/lejianwen/rustdesk-api/v2/http/request/admin"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/lejianwen/rustdesk-api/v2/model/custom_types"
)

func TestLegacyAddressBookUpdatePreservesClientNote(t *testing.T) {
	for _, tc := range []struct {
		name         string
		collectionID uint
	}{{"personal", 0}, {"collection", 9}} {
		t.Run(tc.name, func(t *testing.T) {
			db := issuanceDB(t)
			previous := DB
			DB = db
			t.Cleanup(func() { DB = previous })
			user := issuanceUser(t, db)
			original := model.AddressBook{
				Id: "device", UserId: user.Id, CollectionId: tc.collectionID,
				Alias: "before", Hash: "old-hash", Note: "客户端备注\nkeep this note",
				Tags: custom_types.AutoJson(`["old-tag"]`),
			}
			issuanceCreate(t, db, &original)
			// Both legacy admin controllers build this form, then call UpdateAll.
			// The form has no note field, so its model always contains Note == "".
			form := adminRequest.AddressBookForm{
				RowId: original.RowId, Id: original.Id, UserId: user.Id,
				CollectionId: tc.collectionID, Alias: "after", Tags: []string{},
			}
			updated := form.ToAddressBook()
			if updated.Note != "" {
				t.Fatal("fixture must reproduce a legacy form without note")
			}
			if err := (&AddressBookService{}).UpdateAll(updated); err != nil {
				t.Fatal(err)
			}
			var got model.AddressBook
			if err := db.First(&got, original.RowId).Error; err != nil {
				t.Fatal(err)
			}
			if got.Note != original.Note {
				t.Fatal("legacy update cleared an omitted client note")
			}
			if got.Alias != "after" || got.Hash != "" || string(got.Tags) != "[]" || got.UserId != user.Id || got.CollectionId != tc.collectionID {
				t.Fatal("legacy field update/clear behavior changed")
			}
		})
	}
}
