package ton

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"

	tonclient "github.com/rwrrioe/mytonprovider-agent/internal/adapters/outbound/ton/liteclient"
	"github.com/rwrrioe/mytonprovider-agent/internal/domain"
)

const pubkeyHexLen = 64

type MasterScanner struct {
	client tonclient.Client
	logger *slog.Logger
}

func NewMasterScanner(client tonclient.Client, logger *slog.Logger) *MasterScanner {
	return &MasterScanner{
		client: client,
		logger: logger,
	}
}

func (s *MasterScanner) Scan(
	ctx context.Context,
	masterAddr string,
	fromLT uint64,
) (providers []domain.Provider,
	lastLT uint64,
	err error,
) {
	const op = "adapters.outbound.ton.MasterScanner.Scan"

	log := s.logger.With(
		slog.String("op", op),
		slog.String("master", masterAddr),
	)

	txCtx, cancel := context.WithTimeout(ctx, getTxTimeout)
	defer cancel()

	txs, err := s.client.GetTransactions(txCtx, masterAddr, fromLT)
	if err != nil {
		return nil, fromLT, fmt.Errorf("%s: get transactions: %w", op, err)
	}

	unique := make(map[string]domain.Provider, len(txs))
	lastLT = fromLT

	for _, tx := range txs {
		if tx == nil {
			continue
		}
		if tx.LT <= fromLT {
			continue
		}
		if tx.LT > lastLT {
			lastLT = tx.LT
		}

		pos := strings.Index(tx.Message, domain.TspRegistrationPrefix)
		if pos < 0 {
			continue
		}

		pos += len(domain.TspRegistrationPrefix)
		if pos >= len(tx.Message) {
			continue
		}

		pubkey := strings.ToLower(tx.Message[pos:])
		if len(pubkey) != pubkeyHexLen {
			continue
		}

		raw, decErr := hex.DecodeString(pubkey)
		if decErr != nil || len(raw) != 32 {
			log.Debug(
				"invalid pubkey in registration comment",
				slog.String("pubkey", pubkey),
			)
			continue
		}

		if _, dup := unique[pubkey]; dup {
			continue
		}

		unique[pubkey] = domain.Provider{
			PublicKey:    pubkey,
			Address:      tx.From,
			RegisteredAt: tx.CreatedAt,
		}
	}

	if len(unique) == 0 {
		return nil, lastLT, nil
	}

	providers = make([]domain.Provider, 0, len(unique))
	for _, p := range unique {
		providers = append(providers, p)
	}

	return providers, lastLT, nil
}
