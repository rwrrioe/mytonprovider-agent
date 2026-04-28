package ton

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	tonclient "github.com/rwrrioe/mytonprovider-agent/internal/adapters/outbound/ton/liteclient"
	"github.com/rwrrioe/mytonprovider-agent/internal/domain"
)

const getTxTimeout = 20 * time.Second

type Scanner struct {
	client tonclient.Client
	logger *slog.Logger
}

func NewScanner(client tonclient.Client, logger *slog.Logger) *Scanner {
	return &Scanner{
		client: client,
		logger: logger,
	}
}

func (s *Scanner) Scan(
	ctx context.Context,
	walletAddr string,
	fromLT uint64,
) (
	contracts []domain.StorageContract,
	lastLT uint64,
	err error,
) {
	const op = "adapters.outbound.ton.Scanner.Scan"

	log := s.logger.With(
		slog.String("op", op),
		slog.String("wallet", walletAddr),
	)

	txCtx, cancel := context.WithTimeout(ctx, getTxTimeout)
	defer cancel()

	txs, err := s.client.GetTransactions(txCtx, walletAddr, fromLT)
	if err != nil {
		return nil, fromLT, fmt.Errorf("%s: get transactions: %w", op, err)
	}

	contractLT := make(map[string]uint64, len(txs))

	lastLT = fromLT
	for _, tx := range txs {
		if tx == nil {
			continue
		}
		if tx.Op != domain.StorageRewardWithdrawalOpCode {
			continue
		}
		if tx.LT > lastLT {
			lastLT = tx.LT
		}
		if tx.LT > contractLT[tx.From] {
			contractLT[tx.From] = tx.LT
		}
	}

	if len(contractLT) == 0 {
		return nil, lastLT, nil
	}

	addrs := make([]string, 0, len(contractLT))
	for a := range contractLT {
		addrs = append(addrs, a)
	}

	infos, err := s.client.GetStorageContractsInfo(ctx, addrs)
	if err != nil {
		return nil, fromLT, fmt.Errorf("%s: get storage contracts info: %w", op, err)
	}

	contracts = make([]domain.StorageContract, 0, len(infos))
	for _, info := range infos {
		txLT, ok := contractLT[info.Address]
		if !ok {
			log.Warn("inspector returned unknown contract", slog.String("address", info.Address))
			continue
		}

		contracts = append(contracts, domain.StorageContract{
			Address:   info.Address,
			BagID:     info.BagID,
			OwnerAddr: info.OwnerAddr,
			Size:      info.Size,
			ChunkSize: info.ChunkSize,
			LastLT:    txLT,
			Providers: []string{walletAddr},
		})
	}

	if len(contracts) < len(contractLT) {
		log.Debug(
			"some contracts not enriched by inspector",
			slog.Int("scanned", len(contractLT)),
			slog.Int("enriched", len(contracts)),
		)
	}

	return contracts, lastLT, nil
}
