package redisstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/rwrrioe/mytonprovider-agent/internal/jobs"
	"github.com/rwrrioe/mytonprovider-agent/internal/lib/sl"
	"github.com/rwrrioe/mytonprovider-agent/pkg/metrics"
)

type CycleHandler func(ctx context.Context) error

type Consumer struct {
	rdb           *redis.Client
	stream        string
	group         string
	consumerID    string
	cycleType     string
	pool          int
	timeout       time.Duration
	blockMs       int
	midIdle       time.Duration
	reaperTimeout time.Duration
	handler       CycleHandler
	logger        *slog.Logger
	metrics       *metrics.Metrics
}

type ConsumerConfig struct {
	Stream        string
	Group         string
	ConsumerID    string
	CycleType     string
	Pool          int
	Timeout       time.Duration
	BlockMs       int
	MinIdle       time.Duration
	ReaperTimeout time.Duration
}

func NewConsumer(
	rdb *redis.Client,
	cfg ConsumerConfig,
	handler CycleHandler,
	logger *slog.Logger,
	m *metrics.Metrics,
) *Consumer {
	if cfg.Pool <= 0 {
		cfg.Pool = 1
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Minute
	}
	if cfg.BlockMs <= 0 {
		cfg.BlockMs = 5000
	}
	return &Consumer{
		rdb:           rdb,
		stream:        cfg.Stream,
		group:         cfg.Group,
		consumerID:    cfg.ConsumerID,
		cycleType:     cfg.CycleType,
		pool:          cfg.Pool,
		timeout:       cfg.Timeout,
		blockMs:       cfg.BlockMs,
		midIdle:       cfg.MinIdle,
		reaperTimeout: cfg.ReaperTimeout,
		handler:       handler,
		logger:        logger,
		metrics:       m,
	}
}

func (c *Consumer) Run(ctx context.Context) {
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		c.runReaper(ctx)
	}()

	for i := range c.pool {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.runWorker(ctx, i)
		}()
	}
	wg.Wait()
}

func (c *Consumer) runWorker(ctx context.Context, idx int) {
	log := c.logger.With(
		slog.String("cycle", c.cycleType),
		slog.String("consumer", c.consumerID),
		slog.Int("worker", idx),
	)

	for {
		if ctx.Err() != nil {
			return
		}

		streams, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    c.group,
			Consumer: c.consumerID,
			Streams:  []string{c.stream, ">"},
			Count:    1,
			Block:    time.Duration(c.blockMs) * time.Millisecond,
		}).Result()

		if err != nil {
			if errors.Is(err, redis.Nil) || errors.Is(err, context.Canceled) {
				continue
			}

			log.Error("xreadgroup", sl.Err(err))
			if c.metrics != nil {
				c.metrics.RedisErrors.Inc()
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}

			continue
		}

		for _, s := range streams {
			for _, msg := range s.Messages {
				c.process(ctx, log, msg)
			}
		}
	}
}

func (c *Consumer) process(
	ctx context.Context,
	log *slog.Logger,
	msg redis.XMessage,
) {
	jobID := decodeJobID(msg)
	log = log.With(
		slog.String("msg_id", msg.ID),
		slog.String("job_id", jobID),
	)

	if c.metrics != nil {
		c.metrics.CyclesInflight.WithLabelValues(c.cycleType).Inc()
		defer c.metrics.CyclesInflight.WithLabelValues(c.cycleType).Dec()
	}

	handlerCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	handlerCtx = jobs.WithJobID(handlerCtx, jobID)

	start := time.Now()
	err := c.runHandler(handlerCtx, log)
	dur := time.Since(start)

	status := jobs.StatusOK
	if err != nil {
		log.Error(
			"cycle failed",
			slog.Duration("duration", dur),
			sl.Err(err),
		)
		status = jobs.StatusError
	} else {
		log.Info("cycle ok", slog.Duration("duration", dur))
	}

	if c.metrics != nil {
		c.metrics.CycleDuration.WithLabelValues(c.cycleType).Observe(dur.Seconds())
		c.metrics.CyclesTotal.WithLabelValues(c.cycleType, status).Inc()
	}

	//гонять цикл в случае ошибки должен ьбэк поэтому акн даем всегда
	if err := c.rdb.XAck(ctx, c.stream, c.group, msg.ID).Err(); err != nil {
		log.Error("xack", sl.Err(err))
		if c.metrics != nil {
			c.metrics.RedisErrors.Inc()
		}
	}
}

// runHandler чтобы паника не убила воркер
func (c *Consumer) runHandler(ctx context.Context, log *slog.Logger) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("error in handler: %v", r)
			log.Error("cycle panic", slog.Any("panic", r))
		}
	}()
	return c.handler(ctx)
}

func decodeJobID(msg redis.XMessage) string {
	raw, ok := msg.Values["data"]
	if !ok {
		return ""
	}

	s, ok := raw.(string)
	if !ok {
		return ""
	}

	var env jobs.TriggerEnvelope
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		return ""
	}
	return env.JobID
}
