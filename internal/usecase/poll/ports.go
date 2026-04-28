package poll

import (
	"context"

	"github.com/rwrrioe/mytonprovider-agent/internal/domain"
)

type RatesProbe interface {
	Probe(ctx context.Context, pubkey []byte) (rates domain.Rates, online bool, err error)
}

type ContractInspector interface {
	InspectProviders(ctx context.Context, addrs []string) ([]domain.ContractOnChainState, error)
}

type ProviderRepo interface {
	GetAllPubkeys(ctx context.Context) ([]string, error)
}

type ContractRepo interface {
	GetActiveRelations(ctx context.Context) ([]domain.ContractProviderRelation, error)
}

type Publisher interface {
	PublishResult(
		ctx context.Context,
		streamKey, jobID, cycleType string,
		payload any,
	) error
}
