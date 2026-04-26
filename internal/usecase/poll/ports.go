package poll

import "context"

type ProviderRepository interface {
	GetAllProvidersPubkeys(ctx context.Context) ([]string, error)

	AddProviders(
		ctx context.Context,
		providers []db.ProviderCreate,
	) error
}

type DirectProviderClient interface {
}
