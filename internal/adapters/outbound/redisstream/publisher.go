package redisstream

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/rwrrioe/mytonprovider-agent/internal/jobs"
)

// Publisher пишет в mtpa:result:<type>
type Publisher struct {
	rdb     *redis.Client
	maxLen  int64
	agentID string
}

func NewPublisher(
	rdb *redis.Client,
	maxLen int64,
	agentID string,
) *Publisher {
	return &Publisher{
		rdb:     rdb,
		maxLen:  maxLen,
		agentID: agentID,
	}
}

func (p *Publisher) PublishResult(
	ctx context.Context,
	streamKey, jobID, cycleType string,
	payload any,
) error {
	return p.publish(ctx, streamKey, jobs.ResultEnvelope{
		JobID:       jobID,
		Type:        cycleType,
		Status:      jobs.StatusOK,
		Payload:     payload,
		ProcessedAt: time.Now(),
		AgentID:     p.agentID,
	})
}

func (p *Publisher) PublishError(
	ctx context.Context,
	streamKey, jobID, cycleType string,
	errMsg string,
) error {
	return p.publish(ctx, streamKey, jobs.ResultEnvelope{
		JobID:       jobID,
		Type:        cycleType,
		Status:      jobs.StatusError,
		Error:       errMsg,
		ProcessedAt: time.Now(),
		AgentID:     p.agentID,
	})
}

func (p *Publisher) publish(
	ctx context.Context,
	streamKey string,
	res jobs.ResultEnvelope,
) error {
	const op = "redisstream.Publisher.publish"

	data, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("%s:%w", op, err)
	}

	args := &redis.XAddArgs{
		Stream: streamKey,
		Values: map[string]any{
			"data": data,
		},
	}

	if p.maxLen > 0 {
		args.MaxLen = p.maxLen
		args.Approx = true
	}

	if err := p.rdb.XAdd(ctx, args).Err(); err != nil {
		return fmt.Errorf("%s:%w", op, err)
	}
	return nil
}
