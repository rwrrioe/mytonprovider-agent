package proof

import (
	"context"
	"time"

	"github.com/rwrrioe/mytonprovider-agent/internal/domain"
)

type ContractInspector interface {
	InspectProviders(ctx context.Context, addrs []string) ([]domain.ContractOnChainState, error)
}

type EndpointResolver interface {
	Resolve(ctx context.Context, pubkey []byte, contractAddr string) (domain.ProviderEndpoint, error)
	ResolveStorageWithOverlay(ctx context.Context, providerIP string, bags []string) (domain.Endpoint, error)
}

type BagProver interface {
	Verify(ctx context.Context, ep domain.ProviderEndpoint, bagID string) (domain.ReasonCode, error)
}

type ContractRepo interface {
	GetActiveRelations(ctx context.Context) ([]domain.ContractProviderRelation, error)
}

type EndpointRepo interface {
	LoadFresh(
		ctx context.Context,
		pubkey string,
		maxAge time.Duration,
	) (domain.ProviderEndpoint, bool, error)
}

type Publisher interface {
	PublishResult(
		ctx context.Context,
		streamKey, jobID, cycleType string,
		payload any,
	) error
}
