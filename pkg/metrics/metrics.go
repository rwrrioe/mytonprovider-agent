package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/rwrrioe/mytonprovider-agent/internal/lib/sl"
)

type Metrics struct {
	CyclesTotal    *prometheus.CounterVec
	CycleDuration  *prometheus.HistogramVec
	CyclesInflight *prometheus.GaugeVec
	RedisErrors    prometheus.Counter
	PublishErrors  prometheus.Counter
}

func New(namespace, subsystem string) *Metrics {
	return &Metrics{
		CyclesTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "cycles_total",
			Help:      "Total cycles processed by type and status.",
		}, []string{"cycle", "status"}),

		CycleDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "cycle_duration_seconds",
			Help:      "Cycle handler execution time in seconds.",
			Buckets:   prometheus.ExponentialBuckets(0.05, 2, 14),
		}, []string{"cycle"}),

		CyclesInflight: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "cycles_inflight",
			Help:      "Currently in-flight cycle invocations.",
		}, []string{"cycle"}),

		RedisErrors: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "redis_errors_total",
			Help:      "Redis operation errors (XREADGROUP/XACK/XADD).",
		}),

		PublishErrors: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "publish_errors_total",
			Help:      "Failed result publishes.",
		}),
	}
}

func Serve(ctx context.Context, port string, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Info("metrics server starting", slog.String("addr", srv.Addr))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("metrics server", sl.Err(err))
	}
}
