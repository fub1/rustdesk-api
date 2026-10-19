package model

// AuditDeliveryReceipt records a committed nonce-bearing delivery. Keeping it
// after deletion of a business audit row prevents a retry from recreating it.
// It has no expiration or inferred links to historical audit records.
type AuditDeliveryReceipt struct {
	Key         string `gorm:"type:char(64);primaryKey"`
	PayloadHash string `gorm:"type:char(64);not null"`
}
