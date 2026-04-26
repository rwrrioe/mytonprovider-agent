package dht

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/rwrrioe/mytonprovider-agent/internal/domain"
	"github.com/rwrrioe/mytonprovider-agent/internal/lib/sl"
	"github.com/rwrrioe/mytonprovider-agent/internal/lib/utils"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/adnl/dht"
	"github.com/xssnick/tonutils-go/adnl/keys"
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-storage-provider/pkg/transport"
)

const (
	verifyStorageRetries    = 3
	providerResponseTimeout = 14 * time.Second
	dhtTimeout              = 14 * time.Second
)

func New(
	cl *dht.Client,
	transport *transport.Client,
	log *slog.Logger,
) *DHT {
	return &DHT{
		dht:       cl,
		transport: transport,
		logger:    log,
	}
}

type DHT struct {
	dht       *dht.Client
	transport *transport.Client
	gate      *adnl.Gateway
	logger    *slog.Logger
	mx        sync.Mutex
}

func (d *DHT) Resolve(
	ctx context.Context,
	pubkey []byte,
	contractAddr string,
) (domain.ProviderEndpoint, error) {
	const op = "adapters.dht.Resolve"

	result := domain.ProviderEndpoint{}

	log := d.logger.With(
		slog.String("op", op),
	)

	providerEP, err := d.findProviderEndpoint(ctx, pubkey)
	if err != nil {
		log.Error("failed to find provider endpoint")
		return result, fmt.Errorf("%s:%w", op, err)
	}

	result.Provider = providerEP

	storageEP, err := d.findStorageEndpoint(ctx, pubkey, contractAddr)
	if err != nil {
		log.Warn("failed to find storage endpoint")
		//!! dont edit best effotr
		return result, nil
	}

	result.Storage = storageEP
	return result, nil
}

func (d *DHT) ResolveStorageWithOverlay(
	ctx context.Context,
	providerIP string,
	bags []string,
) (domain.Endpoint, error) {

	const op = "adapters.dht.ResolveStorageWithOverlay"

	if len(bags) == 0 {
		return domain.Endpoint{}, fmt.Errorf("no bags provided")
	}

	bagsToCheck := len(bags)
	switch {
	case len(bags) > 100:
		bagsToCheck = max(1, len(bags)*10/100)
	case len(bags) > 5:
		bagsToCheck = max(1, len(bags)*20/100)
	}

	d.logger.Info(
		"start checking bags",
		slog.String("provider_ip", providerIP),
		slog.Int("bags_to_check", bagsToCheck),
		slog.Int("total_bags", len(bags)),
	)

	shuffled := make([]string, len(bags))
	copy(shuffled, bags)
	rand.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})

	for i := 0; i < bagsToCheck && i < len(shuffled); i++ {
		bag, err := hex.DecodeString(shuffled[i])
		if err != nil {
			d.logger.Error(
				"failed to decode bag ID",
				"bag_id", shuffled[i],
				sl.Err(err),
			)
			continue
		}

		dhtTimeoutCtx, cancel := context.WithTimeout(ctx, dhtTimeout)
		nodesList, _, err := d.dht.FindOverlayNodes(dhtTimeoutCtx, bag)
		cancel()

		if err != nil {
			if !errors.Is(err, dht.ErrDHTValueIsNotFound) {
				d.logger.Error("failed to find bag overlay nodes", "bag_id", shuffled[i], "error", err)
			}
			continue
		}

		if nodesList == nil || len(nodesList.List) == 0 {
			d.logger.Debug("no peers found for bag in DHT", "bag_id", shuffled[i])
			continue
		}

		for _, node := range nodesList.List {
			key, ok := node.ID.(keys.PublicKeyED25519)
			if !ok {
				continue
			}

			adnlID, err := tl.Hash(key)
			if err != nil {
				d.logger.Error("failed to hash overlay key", sl.Err(err))
				continue
			}

			dhtTimeoutCtx2, cancel2 := context.WithTimeout(ctx, dhtTimeout)
			addrList, pubKey, err := d.dht.FindAddresses(dhtTimeoutCtx2, adnlID)
			cancel2()

			if err != nil {
				if !errors.Is(err, dht.ErrDHTValueIsNotFound) {
					d.logger.Debug("failed to find addresses in DHT", sl.Err(err))
				}
				continue
			}

			if addrList == nil || len(addrList.Addresses) == 0 {
				continue
			}

			for _, addr := range addrList.Addresses {
				if addr.IP.String() == providerIP {

					d.logger.Info("successfully found")

					return domain.Endpoint{
						Publickey: pubKey,
						IP:        addr.IP.String(),
						Port:      addr.Port,
					}, nil
				}
			}
		}
	}

	return domain.Endpoint{}, fmt.Errorf("storage not found via overlay after checking %d bags", bagsToCheck)
}

func (d *DHT) findProviderEndpoint(ctx context.Context, pubkey []byte) (domain.Endpoint, error) {
	channelKeyId, err := tl.Hash(keys.PublicKeyED25519{Key: pubkey})
	if err != nil {
		return domain.Endpoint{}, fmt.Errorf("hash provider key: %w", err)
	}

	dhtCtx, cancel := context.WithTimeout(ctx, dhtTimeout)
	defer cancel()

	dhtVal, _, err := d.dht.FindValue(dhtCtx, &dht.Key{
		ID:    channelKeyId,
		Name:  []byte("storage-provider"),
		Index: 0,
	})
	if err != nil {
		return domain.Endpoint{}, fmt.Errorf("find value in dht: %w", err)
	}

	var nodeAddr transport.ProviderDHTRecord
	if _, err = tl.Parse(&nodeAddr, dhtVal.Data, true); err != nil {
		return domain.Endpoint{}, fmt.Errorf("parse dht record: %w", err)
	}

	dhtCtx2, cancel2 := context.WithTimeout(ctx, dhtTimeout)
	defer cancel2()

	l, pub, err := d.dht.FindAddresses(dhtCtx2, nodeAddr.ADNLAddr)
	if err != nil || len(l.Addresses) == 0 {
		return domain.Endpoint{}, fmt.Errorf("find addresses: %w", err)
	}

	return domain.Endpoint{
		Publickey: pub,
		IP:        l.Addresses[0].IP.String(),
		Port:      l.Addresses[0].Port,
	}, nil
}

func (d *DHT) findStorageEndpoint(
	ctx context.Context,
	pubkey []byte,
	contractAddr string,
) (domain.Endpoint, error) {
	addr, err := address.ParseAddr(contractAddr)
	if err != nil {
		return domain.Endpoint{}, fmt.Errorf("parse contract address: %w", err)
	}

	var proof []byte
	err = utils.TryNTimes(func() error {
		timeoutCtx, cancel := context.WithTimeout(ctx, providerResponseTimeout)
		defer cancel()

		proof, err = d.transport.VerifyStorageADNLProof(timeoutCtx, pubkey, addr)
		return err
	}, verifyStorageRetries)
	if err != nil {
		return domain.Endpoint{}, fmt.Errorf("verify storage adnl proof: %w", err)
	}

	dhtTimeoutCtx, cancel := context.WithTimeout(ctx, dhtTimeout)
	defer cancel()

	l, pub, err := d.dht.FindAddresses(dhtTimeoutCtx, proof)
	if err != nil {
		return domain.Endpoint{}, fmt.Errorf("find storage addresses: %w", err)
	}

	if l == nil || len(l.Addresses) == 0 {
		return domain.Endpoint{}, fmt.Errorf("no storage addresses found")
	}

	return domain.Endpoint{
		Publickey: pub,
		IP:        l.Addresses[0].IP.String(),
		Port:      l.Addresses[0].Port,
	}, nil
}
