package model

// PeerStrategyState remembers only keys ever controlled for one registered
// device. ManagedKeys contains a canonical JSON string array, never values.
// There is deliberately no timestamp: the wire revision describes the payload.
type PeerStrategyState struct {
	PeerRowID   uint   `json:"peer_row_id" gorm:"primaryKey;autoIncrement:false"`
	ManagedKeys string `json:"managed_keys" gorm:"type:text;not null"`
	Peer        *Peer  `json:"-" gorm:"foreignKey:PeerRowID;references:RowId;constraint:OnUpdate:RESTRICT,OnDelete:CASCADE"`
}

func (PeerStrategyState) TableName() string { return "peer_strategy_states" }
