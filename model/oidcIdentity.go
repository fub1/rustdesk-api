package model

// OIDCIdentity stores an explicitly verified external identity. Old UserThird
// records are deliberately not inferred or copied into this trust boundary.
type OIDCIdentity struct {
	Id      uint   `json:"id" gorm:"primaryKey"`
	Issuer  string `json:"issuer" gorm:"size:512;not null;uniqueIndex:oidc_identity_key,priority:1"`
	Subject string `json:"sub" gorm:"column:sub;size:255;not null;uniqueIndex:oidc_identity_key,priority:2"`
	UserID  uint   `json:"user_id" gorm:"not null;index"`
	User    *User  `json:"-" gorm:"foreignKey:UserID;constraint:OnUpdate:RESTRICT,OnDelete:RESTRICT"`
}

func (OIDCIdentity) TableName() string { return "oidc_identities" }
