package update

import (
	"context"

	"github.com/rwrrioe/mytonprovider-agent/internal/domain"
)

type IPInfoClient interface {
	GetIPInfo(ctx context.Context, ip string) (domain.IpInfo, error)
}

type IPInfoRepo interface {
	GetProvidersIPs(ctx context.Context) ([]domain.ProviderEndpoint, error)
}

type Publisher interface {
	PublishResult(
		ctx context.Context,
		streamKey, jobID, cycleType string,
		payload any,
	) error
}
