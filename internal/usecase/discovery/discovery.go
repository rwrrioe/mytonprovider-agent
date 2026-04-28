package discovery

import (
	"context"
	"encoding/hex"
	"log/slog"
	"strings"
	"time"

	"github.com/rwrrioe/mytonprovider-agent/internal/domain"
	"github.com/rwrrioe/mytonprovider-agent/internal/jobs"
	"github.com/rwrrioe/mytonprovider-agent/internal/lib/sl"
)

type Config struct {
	MasterAddr  string
	EndpointTTL time.Duration

	ScanMasterStream       string
	ScanWalletsStream      string
	ResolveEndpointsStream string
}

type Discovery struct {
	logger *slog.Logger
	cfg    Config

	masterScanner MasterScanner
	walletScanner WalletScanner
	resolver      EndpointResolver

	providerRepo ProviderRepo
	contractRepo ContractRepo
	endpointRepo EndpointRepo
	systemRepo   SystemRepo
	publisher    Publisher
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
	publisher Publisher,
) *Discovery {
	return &Discovery{
		logger:        logger,
		cfg:           cfg,
		masterScanner: masterScanner,
		walletScanner: walletScanner,
		resolver:      resolver,
		providerRepo:  providerRepo,
		contractRepo:  contractRepo,
		endpointRepo:  endpointRepo,
		systemRepo:    systemRepo,
		publisher:     publisher,
	}
}

// CollectNewProviders сканирует master-кошелёк, дедуплицирует по известным
// pubkey'ям, публикует список новых провайдеров в result-stream и обновляет
// master_wallet_lt в БД (checkpoint).
func (d *Discovery) CollectNewProviders(ctx context.Context) error {
	const op = "usecase.discovery.CollectNewProviders"

	log := d.logger.With(slog.String("op", op))
	log.Debug("collecting new providers")

	fromLT, err := d.systemRepo.GetMasterWalletLT(ctx)
	if err != nil {
		log.Error("failed to get master wallet last lt", sl.Err(err))
		return err
	}

	knownPubkeys, err := d.providerRepo.GetAllPubkeys(ctx)
	if err != nil {
		log.Error("failed to get known pubkeys", sl.Err(err))
		return err
	}

	known := make(map[string]struct{}, len(knownPubkeys))
	for _, pk := range knownPubkeys {
		known[strings.ToLower(pk)] = struct{}{}
	}

	scanned, lastLT, err := d.masterScanner.Scan(ctx, d.cfg.MasterAddr, fromLT)
	if err != nil {
		log.Error("failed to scan master wallet", sl.Err(err))
		return err
	}

	newProviders := make([]domain.Provider, 0, len(scanned))
	for _, p := range scanned {
		if _, ok := known[strings.ToLower(p.PublicKey)]; ok {
			continue
		}
		newProviders = append(newProviders, p)
	}

	result := jobs.ScanMasterResult{
		NewProviders: newProviders,
		LastLT:       lastLT,
		ScannedCount: len(scanned),
	}

	if err = d.publisher.PublishResult(ctx, d.cfg.ScanMasterStream, jobs.JobIDFrom(ctx), jobs.CycleScanMaster, result); err != nil {
		log.Error("failed to publish scan_master result", sl.Err(err))
		return err
	}

	if lastLT > fromLT {
		if sErr := d.systemRepo.SetMasterWalletLT(ctx, lastLT); sErr != nil {
			log.Error("failed to update master wallet lt", sl.Err(sErr))
		}
	}

	log.Info(
		"new providers collected",
		slog.Int("new", len(newProviders)),
		slog.Int("scanned", len(scanned)),
		slog.Uint64("last_lt", lastLT),
	)

	return nil
}

func (d *Discovery) CollectStorageContracts(ctx context.Context) error {
	const op = "usecase.discovery.CollectStorageContracts"

	log := d.logger.With(slog.String("op", op))
	log.Debug("collecting storage contracts")

	wallets, err := d.providerRepo.GetAllWallets(ctx)
	if err != nil {
		log.Error("failed to get provider wallets", sl.Err(err))
		return err
	}

	if len(wallets) == 0 {
		log.Debug("no providers found")
		return d.publisher.PublishResult(ctx, d.cfg.ScanWalletsStream, jobs.JobIDFrom(ctx), jobs.CycleScanWallets, jobs.ScanWalletsResult{})
	}

	addrToPubkey := make(map[string]string, len(wallets))
	for _, w := range wallets {
		addrToPubkey[w.Address] = w.PublicKey
	}

	merged := make(map[string]domain.StorageContract)
	updatedWallets := make([]domain.Provider, 0, len(wallets))

	for k, w := range wallets {
		if ctx.Err() != nil {
			log.Info("context done, stop wallet scan",
				slog.Int("processed", k),
				slog.Int("total", len(wallets)),
			)
			return ctx.Err()
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

	result := jobs.ScanWalletsResult{
		Contracts:      contracts,
		Relations:      relations,
		UpdatedWallets: updatedWallets,
	}
	if err = d.publisher.PublishResult(ctx, d.cfg.ScanWalletsStream, jobs.JobIDFrom(ctx), jobs.CycleScanWallets, result); err != nil {
		log.Error("failed to publish scan_wallets result", sl.Err(err))
		return err
	}

	if len(updatedWallets) > 0 {
		if uErr := d.providerRepo.UpdateLT(ctx, updatedWallets); uErr != nil {
			log.Error("failed to update wallet lts", sl.Err(uErr))
		}
	}

	log.Info(
		"contracts collected",
		slog.Int("contracts", len(contracts)),
		slog.Int("relations", len(relations)),
		slog.Int("wallets_advanced", len(updatedWallets)),
	)

	return nil
}

func (d *Discovery) ResolveEndpoints(ctx context.Context) error {
	const op = "usecase.discovery.ResolveEndpoints"

	log := d.logger.With(slog.String("op", op))
	log.Debug("resolving provider endpoints")

	relations, err := d.contractRepo.GetActiveRelations(ctx)
	if err != nil {
		log.Error("failed to load active relations", sl.Err(err))
		return err

	}
	if len(relations) == 0 {
		log.Debug("no active relations")
		return d.publisher.PublishResult(ctx, d.cfg.ResolveEndpointsStream, jobs.JobIDFrom(ctx), jobs.CycleResolveEndpoints, jobs.ResolveEndpointsResult{})
	}

	existing, err := d.endpointRepo.LoadAll(ctx)
	if err != nil {
		log.Error("failed to load existing endpoints", sl.Err(err))
		return err
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
	resolved := make([]domain.ProviderEndpoint, 0, len(contractByPubkey))
	skipped := 0
	failed := 0

	for pubkey, contractAddr := range contractByPubkey {
		if ctx.Err() != nil {
			log.Info("context done, stop endpoint resolve",
				slog.Int("resolved", len(resolved)),
				slog.Int("skipped", skipped),
				slog.Int("failed", failed),
			)
			return ctx.Err()
		}

		if ep, ok := existing[pubkey]; ok && now.Sub(ep.UpdatedAt) < d.cfg.EndpointTTL {
			skipped++
			continue
		}

		pk, dErr := hex.DecodeString(pubkey)
		if dErr != nil || len(pk) != 32 {
			log.Warn("invalid provider pubkey", slog.String("pubkey", pubkey))
			failed++
			continue
		}

		ep, rErr := d.resolver.Resolve(ctx, pk, contractAddr)
		if rErr != nil {
			log.Debug("failed to resolve provider endpoint",
				slog.String("pubkey", pubkey),
				sl.Err(rErr),
			)
			failed++
			continue
		}

		if ep.Provider.IP == "" {
			log.Debug("provider ip not found", slog.String("pubkey", pubkey))
			failed++
			continue
		}

		if ep.Storage.IP == "" {
			storageEP, sErr := d.resolver.ResolveStorageWithOverlay(
				ctx, ep.Provider.IP, bagsByPubkey[pubkey],
			)
			if sErr != nil {
				log.Debug("storage overlay failed",
					slog.String("pubkey", pubkey),
					sl.Err(sErr),
				)
			} else {
				ep.Storage = storageEP
			}
		}

		ep.PublicKey = pubkey
		ep.UpdatedAt = now
		resolved = append(resolved, ep)
	}

	result := jobs.ResolveEndpointsResult{
		Endpoints: resolved,
		Skipped:   skipped,
		Failed:    failed,
	}

	if err = d.publisher.PublishResult(ctx, d.cfg.ResolveEndpointsStream, jobs.JobIDFrom(ctx), jobs.CycleResolveEndpoints, result); err != nil {
		log.Error("failed to publish resolve_endpoints result", sl.Err(err))
		return err
	}

	log.Info(
		"resolved provider endpoints",
		slog.Int("resolved", len(resolved)),
		slog.Int("skipped", skipped),
		slog.Int("failed", failed),
		slog.Int("total", len(contractByPubkey)),
	)

	return nil
}
