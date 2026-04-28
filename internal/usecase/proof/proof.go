package proof

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/rwrrioe/mytonprovider-agent/internal/domain"
	"github.com/rwrrioe/mytonprovider-agent/internal/jobs"
	"github.com/rwrrioe/mytonprovider-agent/internal/lib/sl"
)

type Config struct {
	EndpointMaxAge      time.Duration
	MaxConcurrentBags   int
	MaxBagsPerProvider  int
	BagsSamplePercent   int
	BetweenBagsCooldown time.Duration

	CheckProofsStream string
}

type Proof struct {
	logger *slog.Logger
	cfg    Config

	inspector ContractInspector
	resolver  EndpointResolver
	prover    BagProver

	contractRepo ContractRepo
	endpointRepo EndpointRepo
	publisher    Publisher
}

func New(
	logger *slog.Logger,
	cfg Config,
	inspector ContractInspector,
	resolver EndpointResolver,
	prover BagProver,
	contractRepo ContractRepo,
	endpointRepo EndpointRepo,
	publisher Publisher,
) *Proof {
	if cfg.EndpointMaxAge <= 0 {
		cfg.EndpointMaxAge = 1 * time.Hour
	}
	if cfg.MaxConcurrentBags <= 0 {
		cfg.MaxConcurrentBags = 30
	}
	if cfg.BetweenBagsCooldown <= 0 {
		cfg.BetweenBagsCooldown = 500 * time.Millisecond
	}
	return &Proof{
		logger:       logger,
		cfg:          cfg,
		inspector:    inspector,
		resolver:     resolver,
		prover:       prover,
		contractRepo: contractRepo,
		endpointRepo: endpointRepo,
		publisher:    publisher,
	}
}

func (p *Proof) CheckStorageProofs(ctx context.Context) error {
	const op = "usecase.proof.CheckStorageProofs"

	log := p.logger.With(slog.String("op", op))
	log.Debug("checking storage proofs")

	relations, err := p.contractRepo.GetActiveRelations(ctx)
	if err != nil {
		log.Error("failed to load active relations", sl.Err(err))
		return err
	}

	if len(relations) == 0 {
		log.Debug("no active relations")
		return p.publisher.PublishResult(ctx, p.cfg.CheckProofsStream, jobs.JobIDFrom(ctx), jobs.CycleCheckProofs, jobs.CheckProofsResult{})
	} //!!!todo dry

	byProvider := make(map[string][]domain.ContractProviderRelation, len(relations))
	for _, rel := range relations {
		byProvider[rel.ProviderPubkey] = append(byProvider[rel.ProviderPubkey], rel)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		sem     = make(chan struct{}, p.cfg.MaxConcurrentBags)
		results = make([]domain.ProofResult, 0, len(relations))
		valid   int
	)

	appendResults := func(rs []domain.ProofResult) {
		mu.Lock()
		results = append(results, rs...)
		for _, r := range rs {
			if r.Reason == domain.ValidStorageProof {
				valid++
			}
		}
		mu.Unlock()
	}

	for pubkey, contracts := range byProvider {
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(pubkey string, contracts []domain.ContractProviderRelation) {
			defer wg.Done()
			defer func() {
				<-sem
			}()

			rs := p.checkProvider(ctx, pubkey, contracts, log)
			appendResults(rs)
		}(pubkey, contracts)
	}

	wg.Wait()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	result := jobs.CheckProofsResult{
		Results: results,
	}

	if err = p.publisher.PublishResult(ctx, p.cfg.CheckProofsStream, jobs.JobIDFrom(ctx), jobs.CycleCheckProofs, result); err != nil {
		log.Error("failed to publish check_proofs result", sl.Err(err))
		return fmt.Errorf("%s:%w", op, err)
	}

	log.Info(
		"checked storage proofs",
		slog.Int("total", len(results)),
		slog.Int("valid", valid),
	)
	return nil
}

func (p *Proof) checkProvider(
	ctx context.Context,
	pubkey string,
	contracts []domain.ContractProviderRelation,
	log *slog.Logger,
) []domain.ProofResult {
	log = log.With(slog.String("provider_pubkey", pubkey))

	ep, ok, err := p.endpointRepo.LoadFresh(ctx, pubkey, p.cfg.EndpointMaxAge)
	if err != nil {
		log.Error("failed to load endpoint", sl.Err(err))
		return fillReason(contracts, domain.IPNotFound)
	}

	if !ok || ep.Storage.IP == "" {
		log.Debug("no fresh endpoint for provider")
		return fillReason(contracts, domain.IPNotFound)
	}

	sample := p.sampleContracts(contracts)

	results := make([]domain.ProofResult, 0, len(sample))
	now := time.Now()

	maxFailureThreshold := uint32(float32(len(sample)) * 0.20)
	var failsInARow uint32

	for _, sc := range sample {
		if ctx.Err() != nil {
			return results
		}

		if maxFailureThreshold > 0 && failsInARow > maxFailureThreshold {
			results = append(results, domain.ProofResult{
				ContractAddr: sc.ContractAddr,
				ProviderAddr: sc.ProviderAddress,
				Reason:       domain.UnavailableProvider,
				CheckedAt:    now,
			})
			continue
		}

		reason, vErr := p.prover.Verify(ctx, ep, sc.BagID)
		if vErr != nil {
			log.Debug("verify failed",
				slog.String("bag_id", sc.BagID),
				sl.Err(vErr),
			)
		}

		results = append(results, domain.ProofResult{
			ContractAddr: sc.ContractAddr,
			ProviderAddr: sc.ProviderAddress,
			Reason:       reason,
			CheckedAt:    time.Now(),
		})

		if reason == domain.ValidStorageProof {
			failsInARow = 0
		} else {
			failsInARow++
		}

		if p.cfg.BetweenBagsCooldown > 0 {
			select {
			case <-ctx.Done():
				return results
			case <-time.After(p.cfg.BetweenBagsCooldown):
			}
		}
	}

	return results
}

func (p *Proof) sampleContracts(contracts []domain.ContractProviderRelation) []domain.ContractProviderRelation {
	if p.cfg.MaxBagsPerProvider <= 0 || len(contracts) <= p.cfg.MaxBagsPerProvider {
		return contracts
	}

	limit := p.cfg.MaxBagsPerProvider
	if p.cfg.BagsSamplePercent > 0 && p.cfg.BagsSamplePercent < 100 {
		calc := len(contracts) * p.cfg.BagsSamplePercent / 100
		if calc > 0 && calc < limit {
			limit = calc
		}
	}

	shuffled := make([]domain.ContractProviderRelation, len(contracts))
	copy(shuffled, contracts)
	rand.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})

	return shuffled[:limit]
}

func fillReason(contracts []domain.ContractProviderRelation, reason domain.ReasonCode) []domain.ProofResult {
	now := time.Now()
	out := make([]domain.ProofResult, len(contracts))
	for i, c := range contracts {
		out[i] = domain.ProofResult{
			ContractAddr: c.ContractAddr,
			ProviderAddr: c.ProviderAddress,
			Reason:       reason,
			CheckedAt:    now,
		}
	}
	return out
}
