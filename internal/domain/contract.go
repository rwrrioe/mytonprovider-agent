package domain

import "time"

// !!todo move structs with tags to redis adapter
const StorageRewardWithdrawalOpCode uint64 = 0xa91baf56

type StorageContract struct {
	Address   string `json:"address"`
	BagID     string `json:"bag_id"`
	OwnerAddr string `json:"owner_address"`
	Size      uint64 `json:"size"`
	ChunkSize uint64 `json:"chunk_size"`
	LastLT    uint64 `json:"last_tx_lt"`
	//!!!providers addressesnot pubkeys
	Providers []string `json:"providers"`
}

type ContractProviderRelation struct {
	ContractAddr    string `json:"contract_address"`
	ProviderPubkey  string `json:"provider_public_key"`
	ProviderAddress string `json:"provider_address"`
	BagID           string `json:"bag_id"`
	Size            uint64 `json:"size"`
}

type ContractOnChainState struct {
	Address         string            `json:"address"`
	Balance         uint64            `json:"balance"`
	Providers       []OnChainProvider `json:"providers"`
	LiteServerError bool              `json:"lite_server_error"`
}

type OnChainProvider struct {
	Key           []byte    `json:"key"`
	LastProofTime time.Time `json:"last_proof_time"`
	RatePerMBDay  uint64    `json:"rate_per_mb_day"`
	MaxSpan       uint32    `json:"max_span"`
}

type ProofResult struct {
	ContractAddr string     `json:"contract_address"`
	ProviderAddr string     `json:"provider_address"`
	Reason       ReasonCode `json:"reason"`
	CheckedAt    time.Time  `json:"checked_at"`
}
