package proof

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/adnl/dht"
	"github.com/xssnick/tonutils-go/adnl/keys"
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-storage-provider/pkg/transport"
)

func (w *providersMasterWorker) StoreProof(ctx context.Context) (interval time.Duration, err error) {
	const (
		successInterval = 60 * time.Minute
		failureInterval = 15 * time.Second
	)

	log := w.logger.With(slog.String("worker", "StoreProof"))
	log.Debug("checking storage proofs")

	interval = successInterval

	storageContracts, err := w.providers.GetStorageContracts(ctx)
	if err != nil {
		log.Error("failed to get storage contracts", "error", err)
		interval = failureInterval

		return
	}

	storageContracts, err = w.updateRejectedContracts(ctx, storageContracts)
	if err != nil {
		interval = failureInterval
		return
	}

	availableProvidersIPs, err := w.updateProvidersIPs(ctx, storageContracts)
	if err != nil {
		interval = failureInterval
		return
	}

	err = w.updateActiveContracts(ctx, storageContracts, availableProvidersIPs)
	if err != nil {
		interval = failureInterval
		return
	}

	err = w.providers.UpdateStatuses(ctx)
	if err != nil {
		log.Error("failed to update provider statuses", "error", err)
		interval = failureInterval
		return
	}

	return
}

func (w *providersMasterWorker) updateRejectedContracts(ctx context.Context, storageContracts []db.ContractToProviderRelation) (activeContracts []db.ContractToProviderRelation, err error) {
	log := w.logger.With(slog.String("worker", "updateRejectedContracts"))

	if len(storageContracts) == 0 {
		log.Debug("no storage contracts to process")
		return
	}

	uniqueContractAddresses := make(map[string]uint64, len(storageContracts))
	for _, sc := range storageContracts {
		uniqueContractAddresses[sc.Address] = sc.Size
	}

	contractAddresses := make([]string, 0, len(uniqueContractAddresses))
	for addr := range uniqueContractAddresses {
		contractAddresses = append(contractAddresses, addr)
	}

	contractsProvidersList, err := w.ton.GetProvidersInfo(ctx, contractAddresses)
	if err != nil {
		log.Error("failed to get providers info", "error", err)
		return
	}

	type contractInfo struct {
		providers map[string]struct{}
		skip      bool
	}

	// map of storage contract addresses to their active providers
	activeRelations := make(map[string]contractInfo, len(contractsProvidersList))
	for _, contract := range contractsProvidersList {
		contractProviders := make(map[string]struct{}, len(contract.Providers))
		for _, provider := range contract.Providers {
			providerPublicKey := fmt.Sprintf("%x", provider.Key)
			if isRemovedByLowBalance(new(big.Int).SetUint64(uniqueContractAddresses[contract.Address]), provider, contract) {
				log.Warn("storage contract has not enough balance for too long, will be removed",
					"provider", providerPublicKey,
					"address", contract.Address,
					"balance", contract.Balance)
				continue
			}

			contractProviders[providerPublicKey] = struct{}{}
		}

		// in case no available lite servers use skip, to not remove contracts from db
		activeRelations[contract.Address] = contractInfo{
			providers: contractProviders,
			skip:      contract.LiteServerError,
		}
	}

	activeContracts = make([]db.ContractToProviderRelation, 0, len(storageContracts))
	closedContracts := make([]db.ContractToProviderRelation, 0, len(storageContracts))

	for _, sc := range storageContracts {
		if contractInfo, exists := activeRelations[sc.Address]; exists {
			if contractInfo.skip {
				log.Debug("lite servers is not available, skip providers check for", "address", sc.Address)
				continue
			}

			if _, providerExists := contractInfo.providers[sc.ProviderPublicKey]; providerExists {
				activeContracts = append(activeContracts, sc)
			} else {
				closedContracts = append(closedContracts, sc)
			}
		} else {
			closedContracts = append(closedContracts, sc)
		}
	}

	err = w.providers.UpdateRejectedStorageContracts(ctx, closedContracts)
	if err != nil {
		log.Error("failed to update rejected storage contracts", "error", err)
		return nil, err
	}

	log.Info("successfully updated rejected storage contracts",
		"closed_count", len(closedContracts),
		"active_count", len(activeContracts))

	return
}

func (w *providersMasterWorker) updateProvidersIPs(ctx context.Context, storageContracts []db.ContractToProviderRelation) (availableProvidersIPs map[string]db.ProviderIP, err error) {
	log := w.logger.With(slog.String("worker", "StoreProof"), slog.String("function", "updateProvidersIPs"))

	if len(storageContracts) == 0 {
		log.Debug("no storage contracts to process for IP update")
		return
	}

	uniqueProviders := make(map[string]db.ContractToProviderRelation)
	for _, sc := range storageContracts {
		if _, exists := uniqueProviders[sc.ProviderPublicKey]; !exists {
			uniqueProviders[sc.ProviderPublicKey] = sc
		}
	}

	availableProvidersIPs = make(map[string]db.ProviderIP, len(uniqueProviders))
	notFoundIPs := make([]string, 0)

	semaphore := make(chan struct{}, maxConcurrentProviderChecks)

	var wg sync.WaitGroup
	var mu sync.Mutex

	// try to find storage IPs using provider's storage adnl proof
	for _, sc := range uniqueProviders {
		wg.Add(1)
		go func(contract db.ContractToProviderRelation) {
			defer wg.Done()

			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			providerIPs, pErr := w.findProviderIPs(ctx, contract, log)
			if pErr != nil {
				notFoundIPs = append(notFoundIPs, contract.ProviderPublicKey)
			}

			mu.Lock()
			availableProvidersIPs[contract.ProviderPublicKey] = providerIPs
			mu.Unlock()
		}(sc)
	}

	wg.Wait()

	// reserve way. Try to find storage IPs using overlay DHT for not found IPs
	for _, pk := range notFoundIPs {
		ip := availableProvidersIPs[pk]
		// nothing we can do if provider IP not found
		if ip.Provider.IP == "" {
			log.Info("provider IP not found", "provider_pubkey", pk)
			delete(availableProvidersIPs, pk)
			continue
		}

		providerContracts := make([]db.ContractToProviderRelation, 0)
		for _, sc := range storageContracts {
			if sc.ProviderPublicKey == pk {
				providerContracts = append(providerContracts, sc)
			}
		}

		if len(providerContracts) == 0 {
			log.Info("no contracts found for provider to find storage IP via overlay", "provider_pubkey", pk)
			delete(availableProvidersIPs, pk)
			continue
		}

		storageIP, err := w.findStorageIPOverlay(ctx, ip.Provider.IP, providerContracts, log)
		if err != nil {
			log.Error("failed to find storage IP via overlay", "provider_pubkey", pk, "error", err)
			delete(availableProvidersIPs, pk)
			continue
		}

		ip.Storage = storageIP
		availableProvidersIPs[pk] = ip
	}

	ips := make([]db.ProviderIP, 0, len(availableProvidersIPs))
	for _, p := range availableProvidersIPs {
		ips = append(ips, p)
	}

	err = w.providers.UpdateProvidersIPs(ctx, ips)
	if err != nil {
		log.Error("failed to update providers IPs", "error", err)
		return
	}

	log.Info("successfully updated providers IPs", "count", len(availableProvidersIPs))
	return
}

func (w *providersMasterWorker) findStorageIPOverlay(ctx context.Context, providerIP string, contracts []db.ContractToProviderRelation, log *slog.Logger) (ip db.IPInfo, err error) {
	if len(contracts) == 0 {
		err = fmt.Errorf("no contracts provided")
		return
	}

	bagsToCheck := len(contracts)
	switch {
	case len(contracts) > 100:
		bagsToCheck = max(1, len(contracts)*10/100)
	case len(contracts) > 5:
		bagsToCheck = max(1, len(contracts)*20/100)
	}

	log = log.With("provider_ip", providerIP, "bags_to_check", bagsToCheck, "total_bags", len(contracts))

	shuffled := make([]db.ContractToProviderRelation, len(contracts))
	copy(shuffled, contracts)
	rand.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})

	for i := 0; i < bagsToCheck && i < len(shuffled); i++ {
		sc := shuffled[i]

		bag, dErr := hex.DecodeString(sc.BagID)
		if dErr != nil {
			log.Error("failed to decode bag ID", "bag_id", sc.BagID, "error", dErr)
			continue
		}

		dhtTimeoutCtx, cancel := context.WithTimeout(ctx, dhtTimeout)
		nodesList, _, fErr := w.dhtClient.FindOverlayNodes(dhtTimeoutCtx, bag)
		cancel()

		if fErr != nil {
			if !errors.Is(fErr, dht.ErrDHTValueIsNotFound) {
				log.Error("failed to find bag overlay nodes", "bag_id", sc.BagID, "error", fErr)
			}
			continue
		}

		if nodesList == nil || len(nodesList.List) == 0 {
			log.Debug("no peers found for bag in DHT", "bag_id", sc.BagID)
			continue
		}

		for _, node := range nodesList.List {
			key, ok := node.ID.(keys.PublicKeyED25519)
			if !ok {
				continue
			}

			adnlID, hErr := tl.Hash(key)
			if hErr != nil {
				log.Error("failed to hash overlay key", "error", hErr)
				continue
			}

			dhtTimeoutCtx2, cancel2 := context.WithTimeout(ctx, dhtTimeout)
			addrList, pubKey, fErr := w.dhtClient.FindAddresses(dhtTimeoutCtx2, adnlID)
			cancel2()

			if fErr != nil {
				if !errors.Is(fErr, dht.ErrDHTValueIsNotFound) {
					log.Debug("failed to find addresses in DHT", "error", fErr)
				}
				continue
			}

			if addrList == nil || len(addrList.Addresses) == 0 {
				continue
			}

			for _, addr := range addrList.Addresses {
				if addr.IP.String() == providerIP {
					ip.PublicKey = pubKey
					ip.IP = addr.IP.String()
					ip.Port = addr.Port

					log.Info("found storage IP via overlay DHT", "provider_pubkey", sc.ProviderPublicKey, "ip", ip.IP, "port", ip.Port)
					return
				}
			}
		}
	}

	err = fmt.Errorf("storage IP not found via overlay DHT after checking %d bags", bagsToCheck)
	return
}

func (w *providersMasterWorker) findProviderIPs(ctx context.Context, sc db.ContractToProviderRelation, log *slog.Logger) (result db.ProviderIP, err error) {
	log = log.With("provider_pubkey", sc.ProviderPublicKey)

	result.PublicKey = sc.ProviderPublicKey

	addr, err := address.ParseAddr(sc.Address)
	if err != nil {
		log.Error("failed to parse address", "address", sc.Address, "error", err)
		return
	}

	pk, err := hex.DecodeString(sc.ProviderPublicKey)
	if err != nil {
		log.Error("failed to decode provider public key", "error", err)
		return
	}

	result.Provider, err = w.findProviderIP(ctx, pk)
	if err != nil {
		log.Error("failed to verify provider IP", "error", err)
		return
	}

	result.Storage, err = w.findStorageIP(ctx, addr, pk)
	if err != nil {
		log.Error("failed to find storage IP", "address", sc.Address, "error", err)
		return
	}

	return
}

func (w *providersMasterWorker) findStorageIP(ctx context.Context, addr *address.Address, pk []byte) (ip db.IPInfo, err error) {
	var proof []byte
	err = utils.TryNTimes(func() (cErr error) {
		timeoutCtx, cancel := context.WithTimeout(ctx, providerResponseTimeout)
		defer cancel()

		proof, cErr = w.providerClient.VerifyStorageADNLProof(timeoutCtx, pk, addr)
		return
	}, verifyStorageRetries)
	if err != nil {
		err = fmt.Errorf("failed to verify storage adnl proof: %w", err)
		return
	}

	dhtTimeoutCtx, cancel := context.WithTimeout(ctx, dhtTimeout)
	defer cancel()
	l, pub, err := w.dhtClient.FindAddresses(dhtTimeoutCtx, proof)
	if err != nil {
		err = fmt.Errorf("failed to find addresses in dht: %w", err)
		return
	}

	if l == nil || len(l.Addresses) == 0 {
		err = fmt.Errorf("no storage addresses found")
		return
	}

	ip.PublicKey = pub
	ip.IP = l.Addresses[0].IP.String()
	ip.Port = l.Addresses[0].Port

	return
}

func (w *providersMasterWorker) findProviderIP(ctx context.Context, pk []byte) (ip db.IPInfo, err error) {
	channelKeyId, err := tl.Hash(keys.PublicKeyED25519{Key: pk})
	if err != nil {
		err = fmt.Errorf("failed to calc hash of provider key: %w", err)
		return
	}

	dhtTimeoutCtx, cancel := context.WithTimeout(ctx, dhtTimeout)
	defer cancel()
	dhtVal, _, err := w.dhtClient.FindValue(dhtTimeoutCtx, &dht.Key{
		ID:    channelKeyId,
		Name:  []byte("storage-provider"),
		Index: 0,
	})
	if err != nil {
		err = fmt.Errorf("failed to find storage-provider in dht: %w", err)
		return
	}

	var nodeAddr transport.ProviderDHTRecord
	if _, pErr := tl.Parse(&nodeAddr, dhtVal.Data, true); pErr != nil {
		err = fmt.Errorf("failed to parse node dht value: %w", pErr)
		return
	}

	if len(nodeAddr.ADNLAddr) == 0 {
		err = fmt.Errorf("no adnl addresses in node dht value")
		return
	}

	dhtTimeoutCtx2, cancel2 := context.WithTimeout(ctx, dhtTimeout)
	defer cancel2()
	l, pub, fErr := w.dhtClient.FindAddresses(dhtTimeoutCtx2, nodeAddr.ADNLAddr)
	if fErr != nil {
		err = fmt.Errorf("failed to find adnl addresses in dht: %w", fErr)
		return
	}

	if l == nil || len(l.Addresses) == 0 {
		err = fmt.Errorf("no provider addresses found")
		return
	}

	ip.PublicKey = pub
	ip.IP = l.Addresses[0].IP.String()
	ip.Port = l.Addresses[0].Port

	return
}

func (w *providersMasterWorker) scanProviderTransactions(ctx context.Context, provider db.ProviderWallet) (contracts map[string]db.StorageContract, lastLT uint64, err error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, getTxTimeout)
	defer cancel()

	txs, err := w.ton.GetTransactions(timeoutCtx, provider.Address, provider.LT)
	if err != nil {
		err = fmt.Errorf("failed to get transactions error: %w", err)
		return
	}

	contracts = make(map[string]db.StorageContract, len(txs))

	lastLT = provider.LT
	for _, tx := range txs {
		if tx == nil {
			continue
		}

		if tx.Op != storageRewardWithdrawalOpCode {
			continue
		}

		s := db.StorageContract{
			ProvidersAddresses: make(map[string]struct{}),
			Address:            tx.From,
			LastLT:             tx.LT,
		}
		s.ProvidersAddresses[provider.Address] = struct{}{}

		if tx.LT > lastLT {
			lastLT = tx.LT
		}

		contracts[tx.From] = s
	}

	return
}
