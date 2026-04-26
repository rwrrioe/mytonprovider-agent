package poll

import (
	"context"
	"encoding/hex"
	"log/slog"
	"math/big"
	"time"
)

type Poll struct {
	logger             *slog.Logger
	providerRepository ProviderRepository
}

func (w *Poll) UpdateKnownProviders(ctx context.Context) (interval time.Duration, err error) {
	const op = "usecase.poll.UpdateKnownProviders"

	const (
		successInterval = 1 * time.Minute
		failureInterval = 5 * time.Second
	)

	log := w.logger.With(
		slog.String("worker", "UpdateKnownProviders"))
	log.Debug("updating known providerRepository")

	interval = successInterval

	pubKeys, err := w.providerRepository.GetAllProvidersPubkeys(ctx)
	if err != nil {
		interval = failureInterval
		return
	}

	if len(pubKeys) == 0 {
		return
	}

	providersInfo := make([]db.ProviderUpdate, 0, len(p))
	providersStatuses := make([]db.ProviderStatusUpdate, 0, len(p))
	for _, pubkey := range pubKeys {
		select {
		case <-ctx.Done():
			log.Info("context done, stopping provider check")
			return
		default:
		}
		d, err := hex.DecodeString(pubkey)
		if err != nil || len(d) != 32 {
			continue
		}

		timeoutCtx, cancel := context.WithTimeout(ctx, providerResponseTimeout)
		rates, err := w.providerClient.GetStorageRates(timeoutCtx, d, fakeSize)
		cancel()
		if err != nil {
			providersStatuses = append(providersStatuses, db.ProviderStatusUpdate{
				Pubkey:   pubkey,
				IsOnline: false,
			})
			continue
		}

		providersStatuses = append(providersStatuses, db.ProviderStatusUpdate{
			Pubkey:   pubkey,
			IsOnline: true,
		})

		providersInfo = append(providersInfo, db.ProviderUpdate{
			Pubkey:       pubkey,
			RatePerMBDay: new(big.Int).SetBytes(rates.RatePerMBDay).Int64(),
			MinBounty:    new(big.Int).SetBytes(rates.MinBounty).Int64(),
			MinSpan:      rates.MinSpan,
			MaxSpan:      rates.MaxSpan,
		})
	}

	err = w.providerRepository.AddStatuses(ctx, providersStatuses)
	if err != nil {
		interval = failureInterval
		return
	}

	err = w.providerRepository.UpdateProviders(ctx, providersInfo)
	if err != nil {
		interval = failureInterval
		return
	}

	log.Info("successfully updated known providerRepository", "active", len(providersInfo))

	return
}
