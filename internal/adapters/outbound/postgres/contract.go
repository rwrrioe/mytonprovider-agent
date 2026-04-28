package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rwrrioe/mytonprovider-agent/internal/domain"
)

type ContractRepo struct {
	db *pgxpool.Pool
}

func NewContractRepo(db *pgxpool.Pool) *ContractRepo {
	return &ContractRepo{db: db}
}

// GetActiveRelations возвращает все активные relations с JOIN на providers для получения pubkey.
// Соответствует бэкенд-методу GetStorageContracts. Read-only — write принадлежит бэкенду.
func (r *ContractRepo) GetActiveRelations(ctx context.Context) ([]domain.ContractProviderRelation, error) {
	const op = "postgres.ContractRepo.GetActiveRelations"

	rows, err := r.db.Query(ctx, `
		SELECT
			p.public_key,
			sc.provider_address,
			sc.address,
			sc.bag_id,
			sc.size
		FROM providers.storage_contracts sc
			JOIN providers.providers p ON p.address = sc.provider_address
	`)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s:%w", op, err)
	}
	defer rows.Close()

	var relations []domain.ContractProviderRelation
	for rows.Next() {
		var rel domain.ContractProviderRelation
		if err = rows.Scan(
			&rel.ProviderPubkey,
			&rel.ProviderAddress,
			&rel.ContractAddr,
			&rel.BagID,
			&rel.Size,
		); err != nil {
			return nil, fmt.Errorf("%s:%w", op, err)
		}
		relations = append(relations, rel)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("%s:%w", op, err)
	}

	return relations, nil
}
