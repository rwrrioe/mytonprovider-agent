package update

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/rwrrioe/mytonprovider-agent/internal/jobs"
	"github.com/rwrrioe/mytonprovider-agent/internal/lib/sl"
)

type Config struct {
	IPInfoTimeout      time.Duration
	BetweenIPsCooldown time.Duration

	LookupIPInfoStream string
}

type Update struct {
	logger *slog.Logger
	cfg    Config

	ipinfo IPInfoClient

	ipinfoRepo IPInfoRepo
	publisher  Publisher
}

func New(
	logger *slog.Logger,
	cfg Config,
	ipinfo IPInfoClient,
	ipinfoRepo IPInfoRepo,
	publisher Publisher,
) *Update {
	if cfg.IPInfoTimeout <= 0 {
		cfg.IPInfoTimeout = 10 * time.Second
	}
	if cfg.BetweenIPsCooldown <= 0 {
		cfg.BetweenIPsCooldown = 1 * time.Second
	}
	return &Update{
		logger:     logger,
		cfg:        cfg,
		ipinfo:     ipinfo,
		ipinfoRepo: ipinfoRepo,
		publisher:  publisher,
	}
}

func (u *Update) UpdateIPInfo(ctx context.Context) error {
	const op = "usecase.update.UpdateIPInfo"

	log := u.logger.With(slog.String("op", op))
	log.Debug("updating ip info")

	endpoints, err := u.ipinfoRepo.GetProvidersIPs(ctx)
	if err != nil {
		log.Error("failed to load endpoints needing geo update", sl.Err(err))
		return err
	}
	if len(endpoints) == 0 {
		log.Debug("no endpoints needing geo update")
		return u.publisher.PublishResult(ctx, u.cfg.LookupIPInfoStream, jobs.JobIDFrom(ctx), jobs.CycleLookupIPInfo, jobs.LookupIPInfoResult{})
	}

	items := make([]jobs.IPInfoUpdate, 0, len(endpoints))

	for i, ep := range endpoints {
		if ctx.Err() != nil {
			log.Info("context done, stop ip info update",
				slog.Int("processed", i),
				slog.Int("collected", len(items)),
			)
			return ctx.Err()
		}

		if ep.Provider.IP == "" {
			continue
		}

		if i > 0 && u.cfg.BetweenIPsCooldown > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(u.cfg.BetweenIPsCooldown):
			}
		}

		fetchCtx, cancel := context.WithTimeout(ctx, u.cfg.IPInfoTimeout)
		info, fErr := u.ipinfo.GetIPInfo(fetchCtx, ep.Provider.IP)
		cancel()
		if fErr != nil {
			log.Error("failed to get ip info",
				slog.String("ip", ep.Provider.IP),
				sl.Err(fErr),
			)
			continue
		}

		items = append(items, jobs.IPInfoUpdate{
			PublicKey: ep.PublicKey,
			IP:        ep.Provider.IP,
			Info:      info,
		})
	}

	result := jobs.LookupIPInfoResult{Items: items}
	if err = u.publisher.PublishResult(ctx, u.cfg.LookupIPInfoStream, jobs.JobIDFrom(ctx), jobs.CycleLookupIPInfo, result); err != nil {
		log.Error("failed to publish lookup_ipinfo result", sl.Err(err))
		return fmt.Errorf("%s:%w", op, err)
	}

	log.Info(
		"ip info collected",
		slog.Int("total", len(endpoints)),
		slog.Int("collected", len(items)),
	)
	return nil
}
