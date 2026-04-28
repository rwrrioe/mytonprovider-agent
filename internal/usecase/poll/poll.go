package poll

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/xssnick/tonutils-go/tlb"

	"github.com/rwrrioe/mytonprovider-agent/internal/domain"
	"github.com/rwrrioe/mytonprovider-agent/internal/jobs"
	"github.com/rwrrioe/mytonprovider-agent/internal/lib/sl"
)

type contractInfo struct {
	providers map[string]struct{}
	skip      bool
}

type Config struct {
	ProbeTimeout       time.Duration
	MaxConcurrentProbe int

	ProbeRatesStream       string
	InspectContractsStream string
}

type Poll struct {
	logger *slog.Logger
	cfg    Config

	probe     RatesProbe
	inspector ContractInspector

	providerRepo ProviderRepo
	contractRepo ContractRepo
	publisher    Publisher
}

func New(
	logger *slog.Logger,
	cfg Config,
	probe RatesProbe,
	inspector ContractInspector,
	providerRepo ProviderRepo,
	contractRepo ContractRepo,
	publisher Publisher,
) *Poll {
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = 14 * time.Second
	}
	if cfg.MaxConcurrentProbe <= 0 {
		cfg.MaxConcurrentProbe = 30
	}
	return &Poll{
		logger:       logger,
		cfg:          cfg,
		probe:        probe,
		inspector:    inspector,
		providerRepo: providerRepo,
		contractRepo: contractRepo,
		publisher:    publisher,
	}
}

func (p *Poll) UpdateProviderRates(ctx context.Context) error {
	const op = "usecase.poll.UpdateProviderRates"

	log := p.logger.With(slog.String("op", op))
	log.Debug("updating provider rates")

	pubkeys, err := p.providerRepo.GetAllPubkeys(ctx)
	if err != nil {
		log.Error("failed to get pubkeys", sl.Err(err))
		return err
	}

	if len(pubkeys) == 0 {
		log.Debug("no providers to poll")
		return p.publisher.PublishResult(ctx, p.cfg.ProbeRatesStream, jobs.JobIDFrom(ctx), jobs.CycleProbeRates, jobs.ProbeRatesResult{})
	}

	statuses := make([]domain.ProviderStatus, 0, len(pubkeys))
	rateUpdates := make([]jobs.ProviderRateUpdate, 0, len(pubkeys))

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		sem = make(chan struct{}, p.cfg.MaxConcurrentProbe)
	)

	for _, pubkey := range pubkeys {
		if ctx.Err() != nil {
			break
		}

		raw, dErr := hex.DecodeString(pubkey)
		if dErr != nil || len(raw) != 32 {
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(pubkey string, raw []byte) {
			defer wg.Done()
			defer func() {
				<-sem
			}()

			probeCtx, cancel := context.WithTimeout(ctx, p.cfg.ProbeTimeout)
			rates, online, pErr := p.probe.Probe(probeCtx, raw)
			cancel()

			now := time.Now()
			isOnline := online && pErr == nil

			mu.Lock()
			defer mu.Unlock()

			statuses = append(statuses, domain.ProviderStatus{
				PublicKey: pubkey,
				IsOnline:  isOnline,
				CheckedAt: now,
			})
			if isOnline {
				rateUpdates = append(rateUpdates, jobs.ProviderRateUpdate{
					PublicKey: pubkey,
					Rates:     rates,
				})
			}
		}(pubkey, raw)
	}

	wg.Wait()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	result := jobs.ProbeRatesResult{
		Statuses: statuses,
		Rates:    rateUpdates,
	}

	if err = p.publisher.PublishResult(ctx, p.cfg.ProbeRatesStream, jobs.JobIDFrom(ctx), jobs.CycleProbeRates, result); err != nil {
		log.Error("failed to publish probe_rates result", sl.Err(err))
		return fmt.Errorf("%s:%w", op, err)
	}

	log.Info(
		"polled providers",
		slog.Int("total", len(pubkeys)),
		slog.Int("online", len(rateUpdates)),
	)

	return nil
}

func (p *Poll) UpdateRejectedContracts(ctx context.Context) error {
	const op = "usecase.poll.UpdateRejectedContracts"

	log := p.logger.With(slog.String("op", op))
	log.Debug("updating rejected contracts")

	relations, err := p.contractRepo.GetActiveRelations(ctx)
	if err != nil {
		log.Error("failed to load active relations", sl.Err(err))
		return err
	}

	if len(relations) == 0 {
		log.Debug("no active relations")
		return p.publisher.PublishResult(ctx, p.cfg.InspectContractsStream, jobs.JobIDFrom(ctx), jobs.CycleInspectContracts, jobs.InspectContractsResult{})
	}

	bagSizeByContract := make(map[string]uint64, len(relations))
	uniqueAddrs := make(map[string]struct{}, len(relations))
	for _, rel := range relations {
		bagSizeByContract[rel.ContractAddr] = rel.Size
		uniqueAddrs[rel.ContractAddr] = struct{}{}
	}

	addrs := make([]string, 0, len(uniqueAddrs))
	for a := range uniqueAddrs {
		addrs = append(addrs, a)
	}

	states, err := p.inspector.InspectProviders(ctx, addrs)
	if err != nil {
		log.Error("failed to inspect contracts", sl.Err(err))
		return err
	}

	stateByAddr := make(map[string]contractInfo, len(states))
	skippedAddrs := make([]string, 0)

	for _, s := range states {
		set := make(map[string]struct{}, len(s.Providers))

		for _, prov := range s.Providers {
			pubkey := fmt.Sprintf("%x", prov.Key)
			bagSize := new(big.Int).SetUint64(bagSizeByContract[s.Address])
			if isRemovedByLowBalance(bagSize, prov, s) {
				log.Warn(
					"contract has not enough balance for too long, will be removed",
					slog.String("provider", pubkey),
					slog.String("address", s.Address),
					slog.Uint64("balance", s.Balance),
				)
				continue
			}
			set[pubkey] = struct{}{}
		}

		stateByAddr[s.Address] = contractInfo{
			providers: set,
			skip:      s.LiteServerError,
		}

		if s.LiteServerError {
			skippedAddrs = append(skippedAddrs, s.Address)
		}
	}

	rejected := make([]domain.ContractProviderRelation, 0)
	active := 0
	for _, rel := range relations {
		info, ok := stateByAddr[rel.ContractAddr]
		if !ok {
			rejected = append(rejected, rel)
			continue
		}
		if info.skip {
			continue
		}
		if _, ok := info.providers[rel.ProviderPubkey]; ok {
			active++
		} else {
			rejected = append(rejected, rel)
		}
	}

	result := jobs.InspectContractsResult{
		Rejected: rejected,
		Skipped:  skippedAddrs,
	}

	if err = p.publisher.PublishResult(ctx, p.cfg.InspectContractsStream, jobs.JobIDFrom(ctx), jobs.CycleInspectContracts, result); err != nil {
		log.Error("failed to publish inspect_contracts result", sl.Err(err))
		return fmt.Errorf("%s:%w", op, err)
	}

	log.Info(
		"inspected contracts",
		slog.Int("rejected", len(rejected)),
		slog.Int("active", active),
		slog.Int("skipped", len(skippedAddrs)),
	)

	return nil
}

func isRemovedByLowBalance(bagSize *big.Int, provider domain.OnChainProvider, contract domain.ContractOnChainState) bool {
	storageFee := tlb.MustFromTON("0.05").Nano()

	mul := new(big.Int).Mul(new(big.Int).SetUint64(provider.RatePerMBDay), bagSize)
	mul.Mul(mul, new(big.Int).SetUint64(uint64(provider.MaxSpan)))
	bounty := new(big.Int).Div(mul, big.NewInt(24*60*60*1024*1024))
	bounty.Add(bounty, storageFee)

	if new(big.Int).SetUint64(contract.Balance).Cmp(bounty) >= 0 {
		return false
	}

	if provider.LastProofTime.Unix() <= 0 {
		return false
	}

	deadline := provider.LastProofTime.Unix() + int64(provider.MaxSpan) + 3600
	return time.Now().Unix() > deadline
}
