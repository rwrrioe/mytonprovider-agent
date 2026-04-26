package domain

import "time"

type StorageContract struct {
	Address   string
	BagID     string
	OwnerAddr string
	Size      uint64
	ChunkSize uint64
	LastLT    uint64
	//!!!providers addresses, not pubkeys
	Providers []string
}

type ContractProviderRelation struct {
	ContractAddr    string
	ProviderPubkey  string
	ProviderAddress string
	BagID           string
	Size            uint64
}

type ContractOnChainState struct {
	Address         string
	Balance         uint64
	Providers       []OnChainProvider
	LiteServerError bool
}

type OnChainProvider struct {
	Key           []byte
	LastProofTime time.Time
	RatePerMBDay  uint64
	MaxSpan       uint32
}

type ProofResult struct {
	ContractAddr string
	ProviderAddr string
	Reason       ReasonCode
	CheckedAt    time.Time
}
