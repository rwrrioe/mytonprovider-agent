package app

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rwrrioe/mytonprovider-agent/internal/config"
	"github.com/rwrrioe/mytonprovider-agent/internal/lib/sl"
	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/adnl/dht"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-storage-provider/pkg/transport"

	dhtAdapter "github.com/rwrrioe/mytonprovider-agent/internal/adapters/outbound/dht"
	"github.com/rwrrioe/mytonprovider-agent/internal/adapters/outbound/directprovider"
	"github.com/rwrrioe/mytonprovider-agent/internal/adapters/outbound/ipconfig"
	"github.com/rwrrioe/mytonprovider-agent/internal/adapters/outbound/postgres"
	tonAdapter "github.com/rwrrioe/mytonprovider-agent/internal/adapters/outbound/ton"
	tonclient "github.com/rwrrioe/mytonprovider-agent/internal/adapters/outbound/ton/liteclient"

	redisin "github.com/rwrrioe/mytonprovider-agent/internal/adapters/inbound/redisstream"
	redisout "github.com/rwrrioe/mytonprovider-agent/internal/adapters/outbound/redisstream"

	"github.com/rwrrioe/mytonprovider-agent/internal/jobs"
	"github.com/rwrrioe/mytonprovider-agent/internal/usecase/discovery"
	"github.com/rwrrioe/mytonprovider-agent/internal/usecase/poll"
	"github.com/rwrrioe/mytonprovider-agent/internal/usecase/proof"
	"github.com/rwrrioe/mytonprovider-agent/internal/usecase/update"
	"github.com/rwrrioe/mytonprovider-agent/pkg/metrics"
)

type binding struct {
	cycleType string
	usecase   config.UsecaseCfg
	handler   redisin.CycleHandler
}

type App struct {
	ProviderRepo *postgres.ProviderRepo
	ContractRepo *postgres.ContractRepo
	EndPointRepo *postgres.EndpointRepo
	SystemRepo   *postgres.SystemRepo
	IPInforRepo  *postgres.IPInfoRepo

	RDB *redis.Client

	MasterScanner *tonAdapter.MasterScanner
	WalletScanner *tonAdapter.Scanner
	Inspector     *tonAdapter.Inspector
	DHTResolver   *dhtAdapter.DHT
	IPInfoCLI     *ipconfig.Client

	Discovery *discovery.Discovery
	Poll      *poll.Poll
	Proof     *proof.Proof
	Update    *update.Update

	Prometheus *metrics.Metrics
	Logger     *slog.Logger

	bindings []binding
	Config   *config.Config

	pool         *pgxpool.Pool
	gateProvider *adnl.Gateway
}

func New(
	ctx context.Context,
	cfg *config.Config,
	logger *slog.Logger,
) (*App, error) {
	const op = "app.New"

	streamKey := func(kind, cycleType string) string {
		return fmt.Sprintf("%s:%s:%s", cfg.Redis.StreamPrefix, kind, cycleType)
	}

	dhtCli, transportCli, gateProvider, err := initADNL(ctx, cfg.TON.ConfigURL, cfg.System.ADNLPort, cfg.System.Key)
	if err != nil {
		return nil, fmt.Errorf("%s:%w", op, err)
	}

	pool, err := postgres.NewPool(ctx, cfg.Postgres.DSN())
	if err != nil {
		return nil, fmt.Errorf("%s:%w", op, err)
	}

	lite, err := tonclient.NewClient(ctx, cfg.TON.ConfigURL, logger)
	if err != nil {
		return nil, fmt.Errorf("%s:%w", op, err)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})

	publisher := redisout.NewPublisher(rdb, cfg.Redis.ResultMaxLen, cfg.System.AgentID)

	providerRepo := postgres.NewProviderRepo(pool)
	contractRepo := postgres.NewContractRepo(pool)
	endpointRepo := postgres.NewEndpointRepo(pool)
	systemRepo := postgres.NewSystemRepo(pool)
	ipinfoRepo := postgres.NewIPInfoRepo(pool)

	masterScanner := tonAdapter.NewMasterScanner(lite, logger)
	walletScanner := tonAdapter.NewScanner(lite, logger)
	inspector := tonAdapter.NewInspector(lite, logger)
	dhtResolver := dhtAdapter.New(dhtCli, transportCli, logger)
	direct := directprovider.New(transportCli, gateProvider, logger)
	ipinfoCli := ipconfig.New(logger)

	discoveryUC := discovery.New(
		logger,
		discovery.Config{
			MasterAddr:             cfg.TON.MasterAddress,
			EndpointTTL:            cfg.Workers.Discovery.EndpointTTL,
			ScanMasterStream:       streamKey("result", jobs.CycleScanMaster),
			ScanWalletsStream:      streamKey("result", jobs.CycleScanWallets),
			ResolveEndpointsStream: streamKey("result", jobs.CycleResolveEndpoints),
		},
		masterScanner, walletScanner, dhtResolver,
		providerRepo, contractRepo, endpointRepo, systemRepo,
		publisher,
	)

	pollUC := poll.New(
		logger,
		poll.Config{
			MaxConcurrentProbe:     cfg.Workers.Poll.Concurrency,
			ProbeRatesStream:       streamKey("result", jobs.CycleProbeRates),
			InspectContractsStream: streamKey("result", jobs.CycleInspectContracts),
		},
		direct, inspector, providerRepo, contractRepo, publisher,
	)

	proofUC := proof.New(
		logger,
		proof.Config{
			EndpointMaxAge:    cfg.Workers.Proof.EndpointTTL,
			MaxConcurrentBags: cfg.Workers.Proof.Concurrency,
			CheckProofsStream: streamKey("result", jobs.CycleCheckProofs),
		},
		inspector, dhtResolver, direct,
		contractRepo, endpointRepo, publisher,
	)

	updateUC := update.New(
		logger,
		update.Config{
			LookupIPInfoStream: streamKey("result", jobs.CycleLookupIPInfo),
		},
		ipinfoCli, ipinfoRepo, publisher,
	)

	m := metrics.New("ton_storage", "mtpa")

	bindings := []binding{
		{jobs.CycleScanMaster, cfg.Workers.Discovery, discoveryUC.CollectNewProviders},
		{jobs.CycleScanWallets, cfg.Workers.Discovery, discoveryUC.CollectStorageContracts},
		{jobs.CycleResolveEndpoints, cfg.Workers.Discovery, discoveryUC.ResolveEndpoints},
		{jobs.CycleProbeRates, cfg.Workers.Poll, pollUC.UpdateProviderRates},
		{jobs.CycleInspectContracts, cfg.Workers.Poll, pollUC.UpdateRejectedContracts},
		{jobs.CycleCheckProofs, cfg.Workers.Proof, proofUC.CheckStorageProofs},
		{jobs.CycleLookupIPInfo, cfg.Workers.Update, updateUC.UpdateIPInfo},
	}
	return &App{
		ProviderRepo:  providerRepo,
		ContractRepo:  contractRepo,
		EndPointRepo:  endpointRepo,
		SystemRepo:    systemRepo,
		IPInforRepo:   ipinfoRepo,
		MasterScanner: masterScanner,
		WalletScanner: walletScanner,
		RDB:           rdb,
		Inspector:     inspector,
		DHTResolver:   dhtResolver,
		IPInfoCLI:     ipinfoCli,
		Discovery:     discoveryUC,
		Poll:          pollUC,
		Proof:         proofUC,
		Update:        updateUC,
		Prometheus:    m,
		Logger:        logger,
		bindings:      bindings,
		Config:        cfg,
	}, nil
}

func (a *App) MustRun(ctx context.Context) {
	//!!dry
	streamKey := func(kind, cycleType string) string {
		return fmt.Sprintf("%s:%s:%s", a.Config.Redis.StreamPrefix, kind, cycleType)
	}

	var wg sync.WaitGroup
	for _, b := range a.bindings {
		if !b.usecase.Enabled {
			a.Logger.Info(
				"cycle disabled",
				slog.String("cycle", b.cycleType),
			)
			continue
		}

		stream := streamKey("cycle", b.cycleType)
		if err := redisin.EnsureGroup(ctx, a.RDB, stream, a.Config.Redis.Group); err != nil {
			a.Logger.Error("ensure group", slog.String("cycle", b.cycleType), sl.Err(err))
			panic(err)
		}

		consumer := redisin.NewConsumer(a.RDB, redisin.ConsumerConfig{
			Stream:     stream,
			Group:      a.Config.Redis.Group,
			ConsumerID: a.Config.System.AgentID,
			CycleType:  b.cycleType,
			Pool:       b.usecase.Pool,
			Timeout:    b.usecase.Timeout,
			BlockMs:    b.usecase.BlockMs,
		}, b.handler, a.Logger, a.Prometheus)

		wg.Add(1)
		go func(c *redisin.Consumer, name string, p int) {
			defer wg.Done()
			c.Run(ctx)
		}(consumer, b.cycleType, b.usecase.Pool)

		a.Logger.Info(
			"cycle consumer started",
			slog.String("cycle", b.cycleType),
			slog.Int("pool", b.usecase.Pool),
			slog.Duration("timeout", b.usecase.Timeout),
		)
	}

	<-ctx.Done()
	a.Logger.Info("shutdown signal received, draining consumers")
	wg.Wait()
	a.Logger.Info("agent stopped")
}

func initADNL(
	ctx context.Context,
	configURL, adnlPort string,
	privKey ed25519.PrivateKey,
) (*dht.Client,
	*transport.Client,
	*adnl.Gateway,
	error,
) {
	const op = "main.initADNL"

	lsCfg, err := liteclient.GetConfigFromUrl(ctx, configURL)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: liteclient config: %w", op, err)
	}

	_, dhtAdnlKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: dht key: %w", op, err)
	}

	dl, err := adnl.DefaultListener("0.0.0.0:" + adnlPort)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: udp listen: %w", op, err)
	}

	netMgr := adnl.NewMultiNetReader(dl)

	dhtGate := adnl.NewGatewayWithNetManager(dhtAdnlKey, netMgr)
	if err = dhtGate.StartClient(); err != nil {
		return nil, nil, nil, fmt.Errorf("%s: dht gateway: %w", op, err)
	}

	dhtCli, err := dht.NewClientFromConfig(dhtGate, lsCfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: dht client: %w", op, err)
	}

	gateProvider := adnl.NewGatewayWithNetManager(privKey, netMgr)
	if err = gateProvider.StartClient(); err != nil {
		return nil, nil, nil, fmt.Errorf("%s: provider gateway: %w", op, err)
	}

	transportCli := transport.NewClient(gateProvider, dhtCli)

	return dhtCli, transportCli, gateProvider, nil
}

func (a *App) Close() {
	a.pool.Close()
	a.RDB.Close()
	a.gateProvider.Close()
}
