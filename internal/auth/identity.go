package auth

// ExternalIdentity is a verified protocol result, never an untrusted HTTP DTO.
type ExternalIdentity struct {
	Issuer, Subject, Username, Name, Email, Picture string
}

// ProviderConfig is an immutable snapshot captured when an attempt begins.
type ProviderConfig struct {
	ID                   uint
	Op, Issuer, ClientID string
	AutoRegister         bool
}
