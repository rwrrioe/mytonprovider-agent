package ton

import (
	"context"
	"fmt"
	"log/slog"

	tonclient "github.com/rwrrioe/mytonprovider-agent/internal/adapters/outbound/ton/liteclient"
	"github.com/rwrrioe/mytonprovider-agent/internal/domain"
)

// Inspector — обёртка над liteclient.Client.GetProvidersInfo,
// которая маппит результат в доменную модель.
// Реализует poll.ContractInspector и proof.ContractInspector.
type Inspector struct {
	client tonclient.Client
	logger *slog.Logger
}

func NewInspector(client tonclient.Client, logger *slog.Logger) *Inspector {
	return &Inspector{
		client: client,
		logger: logger,
	}
}

func (i *Inspector) InspectProviders(
	ctx context.Context,
	addrs []string,
) ([]domain.ContractOnChainState, error) {
	const op = "adapters.outbound.ton.Inspector.InspectProviders"

	if len(addrs) == 0 {
		return nil, nil
	}

	raw, err := i.client.GetProvidersInfo(ctx, addrs)
	if err != nil {
		return nil, fmt.Errorf("%s:%w", op, err)
	}

	out := make([]domain.ContractOnChainState, 0, len(raw))
	for _, r := range raw {
		providers := make([]domain.OnChainProvider, 0, len(r.Providers))
		for _, p := range r.Providers {
			providers = append(providers, domain.OnChainProvider{
				Key:           []byte(p.Key),
				LastProofTime: p.LastProofTime,
				RatePerMBDay:  p.RatePerMBDay,
				MaxSpan:       p.MaxSpan,
			})
		}
		out = append(out, domain.ContractOnChainState{
			Address:         r.Address,
			Balance:         r.Balance,
			Providers:       providers,
			LiteServerError: r.LiteServerError,
		})
	}

	return out, nil
}
