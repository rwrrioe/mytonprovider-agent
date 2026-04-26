package update

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

func (w *providersMasterWorker) UpdateRating(ctx context.Context) (interval time.Duration, err error) {
	const (
		successInterval = 5 * time.Minute
		failureInterval = 5 * time.Second
	)

	log := w.logger.With(slog.String("worker", "UpdateRating"))
	log.Debug("updating provider ratings")

	interval = successInterval

	err = w.providers.UpdateRating(ctx)
	if err != nil {
		interval = failureInterval
		return
	}

	return
}

func (w *providersMasterWorker) UpdateIPInfo(ctx context.Context) (interval time.Duration, err error) {
	const (
		successInterval = 240 * time.Minute
		failureInterval = 30 * time.Second
	)

	log := w.logger.With(slog.String("worker", "UpdateIPInfo"))
	log.Debug("updating provider IP info")

	interval = failureInterval

	ips, err := w.providers.GetProvidersIPs(ctx)
	if err != nil {
		log.Error("failed to get provider IPs", "error", err)
		return
	}

	if len(ips) == 0 {
		log.Info("no provider IPs to update")
		interval = successInterval
		return
	}

	ipsInfo := make([]db.ProviderIPInfo, 0, len(ips))
	for _, ip := range ips {
		time.Sleep(ipInfoSleepDuration)

		ipErr := func() error {
			timeoutCtx, cancel := context.WithTimeout(ctx, ipInfoTimeout)
			defer cancel()

			info, err := w.ipinfo.GetIPInfo(timeoutCtx, ip.Provider.IP)
			if err != nil {
				return fmt.Errorf("failed to get IP info: %w", err)
			}

			s, err := json.Marshal(info)
			if err != nil {
				return fmt.Errorf("failed to marshal IP info: %w, ip: %s, info: %s", err, ip.Provider.IP, info)
			}

			ipsInfo = append(ipsInfo, db.ProviderIPInfo{
				PublicKey: ip.PublicKey,
				IPInfo:    string(s),
			})

			return nil
		}()
		if ipErr != nil {
			log.Error(ipErr.Error())
			continue
		}
	}

	err = w.providers.UpdateProvidersIPInfo(ctx, ipsInfo)
	if err != nil {
		log.Error("failed to update provider IP info", "error", err)
		interval = failureInterval
		return
	}

	interval = successInterval

	return
}
