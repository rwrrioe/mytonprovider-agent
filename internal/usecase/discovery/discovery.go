package discovery

import (
	"context"
	"encoding/hex"
	"log/slog"
	"strings"
	"time"

	"github.com/rwrrioe/mytonprovider-agent/internal/domain"
	"github.com/rwrrioe/mytonprovider-agent/internal/lib/sl"
)

type Config struct {
	MasterAddr  string
	EndpointTTL time.Duration // не резолвить endpoint моложе этого возраста
}

type Discovery struct {
	logger      *slog.Logger
	masterAddr  string
	endpointTTL time.Duration

	masterScanner MasterScanner
	walletScanner WalletScanner
	resolver      EndpointResolver

	providerRepo ProviderRepo
	contractRepo ContractRepo
	endpointRepo EndpointRepo
	systemRepo   SystemRepo
}

func New(
	logger *slog.Logger,
	cfg Config,
	masterScanner MasterScanner,
	walletScanner WalletScanner,
	resolver EndpointResolver,
	providerRepo ProviderRepo,
	contractRepo ContractRepo,
	endpointRepo EndpointRepo,
	systemRepo SystemRepo,
) *Discovery {
	return &Discovery{
		logger:        logger,
		masterAddr:    cfg.MasterAddr,
		endpointTTL:   cfg.EndpointTTL,
		masterScanner: masterScanner,
		walletScanner: walletScanner,
		resolver:      resolver,
		providerRepo:  providerRepo,
		contractRepo:  contractRepo,
		endpointRepo:  endpointRepo,
		systemRepo:    systemRepo,
	}
}

func (d *Discovery) CollectNewProviders(ctx context.Context) (interval time.Duration, err error) {
	const (
		op = "usecase.discovery.CollectNewProviders"

		successInterval = 1 * time.Minute
		failureInterval = 5 * time.Second
	)

	log := d.logger.With(
		slog.String("stage", "collect new providers"),
	)

	log.Debug("collecting new providers")

	interval = successInterval

	fromLT, err := d.systemRepo.GetMasterWalletLT(ctx)
	if err != nil {
		log.Error("failed to get master wallet last lt", sl.Err(err))
		interval = failureInterval
		return
	}

	knownPubkeys, err := d.providerRepo.GetAllPubkeys(ctx)
	if err != nil {
		log.Error("failed to get known pubkeys", sl.Err(err))
		interval = failureInterval
		return
	}

	known := make(map[string]struct{}, len(knownPubkeys))
	for _, pk := range knownPubkeys {
		known[strings.ToLower(pk)] = struct{}{}
	}

	scanned, lastLT, err := d.masterScanner.Scan(ctx, d.masterAddr, fromLT)
	if err != nil {
		log.Error("failed to scan master wallet", "error", err.Error())
		interval = failureInterval
		return
	}

	newProviders := make([]domain.Provider, 0, len(scanned))
	for _, p := range scanned {
		if _, ok := known[strings.ToLower(p.PublicKey)]; ok {
			continue
		}
		newProviders = append(newProviders, p)
	}

	if len(newProviders) > 0 {
		if err = d.providerRepo.Create(ctx, newProviders); err != nil {
			log.Error("failed to create providers",
				"error", err.Error(), "count", len(newProviders))
			interval = failureInterval
			return
		}
	}

	if lastLT > fromLT {
		if sErr := d.systemRepo.SetMasterWalletLT(ctx, lastLT); sErr != nil {
			log.Error("failed to update master wallet lt", "error", sErr.Error())
		}
	}

	log.Info(
		"new providers successfully collected",
		slog.Int("new", len(newProviders)),
		slog.Int("scanned", len(scanned)),
		slog.Int("last_lt", int(lastLT)),
	)

	return
}

func (d *Discovery) CollectStorageContracts(ctx context.Context) (interval time.Duration, err error) {
	const (
		op = "usecase.discovery.CollectStorageContracts"

		successInterval = 60 * time.Minute
		failureInterval = 15 * time.Second
	)

	log := d.logger.With(
		slog.String("stage", "collect storage contracts"),
	)
	log.Debug("collecting storage contracts")

	interval = successInterval

	wallets, err := d.providerRepo.GetAllWallets(ctx)
	if err != nil {
		log.Error("failed to get provider wallets", sl.Err(err))
		interval = failureInterval
		return
	}

	if len(wallets) == 0 {
		log.Debug("no providers found")
		return
	}

	addrToPubkey := make(map[string]string, len(wallets))
	for _, w := range wallets {
		addrToPubkey[w.Address] = w.PublicKey
	}

	merged := make(map[string]domain.StorageContract)
	updatedWallets := make([]domain.Provider, 0, len(wallets))

	for k, w := range wallets {
		select {
		case <-ctx.Done():
			log.Info(
				"context done, stop wallet scan",
				slog.Int("processed", k),
				slog.Int("total", len(wallets)),
			)
			interval = failureInterval
			err = ctx.Err()
			return
		default:
		}

		contracts, lastLT, scanErr := d.walletScanner.Scan(ctx, w.Address, w.LT)
		if scanErr != nil {
			log.Error(
				"failed to scan provider wallet",
				slog.String("pubkey", w.PublicKey),
				slog.String("address", w.Address),
				sl.Err(scanErr),
			)
			continue
		}

		for _, c := range contracts {
			if existing, ok := merged[c.Address]; ok {
				existing.Providers = append(existing.Providers, c.Providers...)
				if c.LastLT > existing.LastLT {
					existing.LastLT = c.LastLT
				}
				merged[c.Address] = existing
			} else {
				merged[c.Address] = c
			}
		}

		if lastLT > w.LT {
			w.LT = lastLT
			updatedWallets = append(updatedWallets, w)
		}
	}

	if len(merged) == 0 {
		log.Debug("no new storage contracts")
		if len(updatedWallets) > 0 {
			if uErr := d.providerRepo.UpdateLT(ctx, updatedWallets); uErr != nil {
				log.Error("failed to update wallet lts",
					"error", uErr.Error(), "count", len(updatedWallets))
			}
		}
		return
	}

	contracts := make([]domain.StorageContract, 0, len(merged))
	relations := make([]domain.ContractProviderRelation, 0, len(merged)*2)

	for _, c := range merged {
		contracts = append(contracts, c)

		for _, providerAddr := range c.Providers {
			pubkey, ok := addrToPubkey[providerAddr]
			if !ok {
				log.Warn(
					"contract references unknown provider",
					slog.String("contract", c.Address),
					slog.String("provider_address", providerAddr),
				)
				continue
			}

			relations = append(relations, domain.ContractProviderRelation{
				ContractAddr:    c.Address,
				ProviderPubkey:  pubkey,
				ProviderAddress: providerAddr,
				BagID:           c.BagID,
				Size:            c.Size,
			})
		}
	}

	if err = d.contractRepo.CreateContracts(ctx, contracts); err != nil {
		log.Error(
			"failed to create contracts",
			slog.Int("count", len(contracts)),
			sl.Err(err),
		)
		interval = failureInterval
		return
	}

	if err = d.contractRepo.CreateRelations(ctx, relations); err != nil {
		log.Error("failed to create relations",
			"error", err.Error(), "count", len(relations))
		interval = failureInterval
		return
	}

	if len(updatedWallets) > 0 {
		if uErr := d.providerRepo.UpdateLT(ctx, updatedWallets); uErr != nil {
			log.Error("failed to update wallet lts",
				"error", uErr.Error(), "count", len(updatedWallets))
		}
	}

	log.Info(
		"new contracts successfully collected",
		slog.Int("contracts", len(contracts)),
		slog.Int("relations", len(relations)),
		slog.Int("wallets_advanced", len(updatedWallets)),
	)

	return
}

// ResolveEndpoints — best-effort резолв сетевых координат провайдеров.
// Discovery — единственный owner записи endpoints.
// Skip провайдеров со свежим endpoint (моложе endpointTTL).
func (d *Discovery) ResolveEndpoints(ctx context.Context) (interval time.Duration, err error) {
	const (
		op = "usecase.discovery.ResolveEndpoints"

		successInterval = 30 * time.Minute
		failureInterval = 1 * time.Minute
	)

	log := d.logger.With(
		slog.String("op", op),
	)
	log.Debug("resolving provider endpoints")

	interval = successInterval

	relations, err := d.contractRepo.GetActiveRelations(ctx)
	if err != nil {
		log.Error("failed to load active relations", sl.Err(err))
		interval = failureInterval
		return
	}

	if len(relations) == 0 {
		log.Debug("no active relations")
		return
	}

	existing, err := d.endpointRepo.LoadAll(ctx)
	if err != nil {
		log.Error("failed to load existing endpoints", sl.Err(err))
		interval = failureInterval
		return
	}

	contractByPubkey := make(map[string]string, len(relations))
	bagsByPubkey := make(map[string][]string, len(relations))
	for _, r := range relations {
		if _, ok := contractByPubkey[r.ProviderPubkey]; !ok {
			contractByPubkey[r.ProviderPubkey] = r.ContractAddr
		}
		bagsByPubkey[r.ProviderPubkey] = append(bagsByPubkey[r.ProviderPubkey], r.BagID)
	}

	now := time.Now()
	resolved := 0
	skipped := 0
	failed := 0

	for pubkey, contractAddr := range contractByPubkey {
		select {
		case <-ctx.Done():
			log.Info(
				"context done stop endpoint resolve",
				slog.Int("resolved", resolved),
				slog.Int("skipped", skipped),
				slog.Int("failed", failed),
			)
			interval = failureInterval
			err = ctx.Err()
			return
		default:
		}

		if ep, ok := existing[pubkey]; ok && now.Sub(ep.UpdatedAt) < d.endpointTTL {
			skipped++
			continue
		}

		pk, dErr := hex.DecodeString(pubkey)
		if dErr != nil || len(pk) != 32 {
			log.Warn(
				"invalid provider pubkey",
				slog.String("pubkey", pubkey),
			)
			failed++
			continue
		}

		ep, rErr := d.resolver.Resolve(ctx, pk, contractAddr)
		if rErr != nil {
			log.Debug(
				"failed to resolve provider endpoint",
				slog.String("pubkey", pubkey),
				sl.Err(rErr),
			)
			failed++
			continue
		}

		if ep.Provider.IP == "" {
			log.Debug("provider ip not found", "pubkey", pubkey)
			failed++
			continue
		}

		if ep.Storage.IP == "" {
			storageEP, sErr := d.resolver.ResolveStorageWithOverlay(
				ctx,
				ep.Provider.IP,
				bagsByPubkey[pubkey],
			)
			if sErr != nil {
				log.Debug(
					"storage overlay failed",
					slog.String("pubkey", pubkey),
					sl.Err(sErr),
				)
			} else {
				ep.Storage = storageEP
			}
		}

		ep.PublicKey = pubkey
		ep.UpdatedAt = now

		if uErr := d.endpointRepo.Upsert(ctx, ep); uErr != nil {
			log.Error("failed to upsert endpoint",
				"pubkey", pubkey, "error", uErr.Error())
			failed++
			continue
		}

		resolved++
	}

	log.Info(
		"resolved provider endpoints",
		"resolved", resolved,
		"skipped", skipped,
		"failed", failed,
		"total", len(contractByPubkey),
	)

	return
}
