package ton

import (
	"context"
	"fmt"
	"log/slog"

	tonclient "github.com/rwrrioe/mytonprovider-agent/internal/adapters/outbound/ton/liteclient"
	"github.com/rwrrioe/mytonprovider-agent/internal/domain"
)

type Scanner struct {
	client tonclient.Client
	logger *slog.Logger
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
	const op = "adapters.outbound.ton.scanner.Scan"

	//todo
	_, err = s.client.GetTransactions(ctx, walletAddr, fromLT)
	if err != nil {
		s.logger.Error("failed to scan wallet", err)
		return nil, 0, fmt.Errorf("%s:%w", op, err)
	}

	// add filter reward

	// add bag info

	//add domain mapping

	return nil, 0, nil
}
