package redisstream

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rwrrioe/mytonprovider-agent/internal/lib/sl"
)

func (c *Consumer) runReaper(ctx context.Context) {
	log := c.logger.With(
		"reaper",
		slog.String("cycle", c.cycleType),
		slog.String("consumer", c.consumerID),
	)

	ticker := time.NewTicker(c.reaperTimeout)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.claimPending(ctx, log)
		}
	}
}

func (c *Consumer) claimPending(ctx context.Context, log *slog.Logger) {
	cursor := "0-0"

	for {

		jobs, start, err := c.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream:   c.stream,
			Group:    c.group,
			MinIdle:  c.midIdle,
			Start:    cursor,
			Count:    10,
			Consumer: c.consumerID,
		}).Result()

		if err != nil {
			log.Error("claim pending", sl.Err(err))
			c.metrics.RedisErrors.Inc()
		}

		for _, job := range jobs {
			log.Warn("job reassigned", job.ID)
			c.process(ctx, log, job)
		}

		cursor = start
		if cursor == "0-0" {
			break
		}
	}
}
