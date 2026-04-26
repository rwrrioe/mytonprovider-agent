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

type ProviderRepo interface {
	Create(
		ctx context.Context,
		providers []domain.Provider,
	) error

	GetAllPubkeys(ctx context.Context) ([]string, error)
	GetAllWallets(ctx context.Context) ([]domain.Provider, error)

	UpdateLT(
		ctx context.Context,
		providers []domain.Provider,
	) error
}

type ContractRepo interface {
	CreateContracts(
		ctx context.Context,
		contracts []domain.StorageContract,
	) error

	CreateRelations(
		ctx context.Context,
		relations []domain.ContractProviderRelation,
	) error

	GetActiveRelations(ctx context.Context) ([]domain.ContractProviderRelation, error)
}

type EndpointRepo interface {
	Upsert(
		ctx context.Context,
		endpoint domain.ProviderEndpoint,
	) error

	LoadAll(ctx context.Context) (map[string]domain.ProviderEndpoint, error)
}

type SystemRepo interface {
	GetMasterWalletLT(ctx context.Context) (uint64, error)
	SetMasterWalletLT(ctx context.Context, lt uint64) error
}
