package discovery

import (
	"context"

	"github.com/rwrrioe/mytonprovider-agent/internal/domain"
)

type MasterScanner interface {
	Scan(
		ctx context.Context,
		masterAddr string,
		fromLT uint64,
	) (
		providers []domain.Provider,
		lastLT uint64,
		err error,
	)
}

type WalletScanner interface {
	Scan(
		ctx context.Context,
		walletAddr string,
		fromLT uint64,
	) (
		contracts []domain.StorageContract,
		lastLT uint64,
		err error,
	)
}

type EndpointResolver interface {
	Resolve(
		ctx context.Context,
		pubkey []byte,
		contractAddr string,
	) (domain.ProviderEndpoint, error)

	ResolveStorageWithOverlay(
		ctx context.Context,
		providerIP string,
		bags []string,
	) (domain.Endpoint, error)
}

// ProviderRepo: read-only кроме UpdateLT (per-wallet LT — checkpoint, пишет агент).
type ProviderRepo interface {
	GetAllPubkeys(ctx context.Context) ([]string, error)
	GetAllWallets(ctx context.Context) ([]domain.Provider, error)

	UpdateLT(
		ctx context.Context,
		providers []domain.Provider,
	) error
}

// ContractRepo: read-only.
type ContractRepo interface {
	GetActiveRelations(ctx context.Context) ([]domain.ContractProviderRelation, error)
}

// EndpointRepo: read-only.
type EndpointRepo interface {
	LoadAll(ctx context.Context) (map[string]domain.ProviderEndpoint, error)
}

// SystemRepo: master_wallet_lt — checkpoint, пишет агент.
type SystemRepo interface {
	GetMasterWalletLT(ctx context.Context) (uint64, error)
	SetMasterWalletLT(ctx context.Context, lt uint64) error
}

// Publisher публикует ResultEnvelope в result-stream.
type Publisher interface {
	PublishResult(
		ctx context.Context,
		streamKey, jobID, cycleType string,
		payload any,
	) error
}
