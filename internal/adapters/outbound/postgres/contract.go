package postgres

import (
	"context"
	"encoding/json"
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

// internal DTOs — не утекают в usecase.
type contractRowDTO struct {
	Address      string `json:"address"`
	ProviderAddr string `json:"provider_address"`
	BagID        string `json:"bag_id"`
	OwnerAddress string `json:"owner_address"`
	Size         uint64 `json:"size"`
	ChunkSize    uint64 `json:"chunk_size"`
	LastTxLT     uint64 `json:"last_tx_lt"`
}

type proofCheckDTO struct {
	ContractAddr string `json:"contract_address"`
	ProviderAddr string `json:"provider_address"`
	Reason       uint32 `json:"reason"`
}

// CreateContracts вставляет контракты вместе с relation к каждому провайдеру из Providers.
func (r *ContractRepo) CreateContracts(ctx context.Context, contracts []domain.StorageContract) error {
	const op = "postgres.ContractRepo.CreateContracts"

	if len(contracts) == 0 {
		return nil
	}

	var rows []contractRowDTO
	for _, c := range contracts {
		for _, addr := range c.Providers {
			rows = append(rows, contractRowDTO{
				Address:      c.Address,
				ProviderAddr: addr,
				BagID:        c.BagID,
				OwnerAddress: c.OwnerAddr,
				Size:         c.Size,
				ChunkSize:    c.ChunkSize,
				LastTxLT:     c.LastLT,
			})
		}
	}

	data, err := json.Marshal(rows)
	if err != nil {
		return fmt.Errorf("%s:%w", op, err)
	}

	_, err = r.db.Exec(ctx, `
		INSERT INTO providers.storage_contracts
			(address, provider_address, bag_id, owner_address, size, chunk_size, last_tx_lt)
		SELECT
			c->>'address',
			c->>'provider_address',
			c->>'bag_id',
			c->>'owner_address',
			(c->>'size')::bigint,
			(c->>'chunk_size')::bigint,
			(c->>'last_tx_lt')::bigint
		FROM jsonb_array_elements($1::jsonb) AS c
		ON CONFLICT (address, provider_address) DO UPDATE SET
			last_tx_lt = EXCLUDED.last_tx_lt
	`, string(data))
	if err != nil {
		return fmt.Errorf("%s:%w", op, err)
	}

	return nil
}

// CreateRelations вставляет явные relation-записи (когда address провайдера уже известен).
func (r *ContractRepo) CreateRelations(ctx context.Context, relations []domain.ContractProviderRelation) error {
	const op = "postgres.ContractRepo.CreateRelations"

	if len(relations) == 0 {
		return nil
	}

	type relDTO struct {
		Address      string `json:"address"`
		ProviderAddr string `json:"provider_address"`
		BagID        string `json:"bag_id"`
		Size         uint64 `json:"size"`
	}

	dtos := make([]relDTO, len(relations))
	for i, rel := range relations {
		dtos[i] = relDTO{
			Address:      rel.ContractAddr,
			ProviderAddr: rel.ProviderAddress,
			BagID:        rel.BagID,
			Size:         rel.Size,
		}
	}

	data, err := json.Marshal(dtos)
	if err != nil {
		return fmt.Errorf("%s:%w", op, err)
	}

	_, err = r.db.Exec(ctx, `
		INSERT INTO providers.storage_contracts (address, provider_address, bag_id, size)
		SELECT
			c->>'address',
			c->>'provider_address',
			c->>'bag_id',
			(c->>'size')::bigint
		FROM jsonb_array_elements($1::jsonb) AS c
		ON CONFLICT (address, provider_address) DO NOTHING
	`, string(data))
	if err != nil {
		return fmt.Errorf("%s:%w", op, err)
	}

	return nil
}

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

// MarkRejected удаляет закрытые контракты из таблицы (owner: proof/poll).
func (r *ContractRepo) MarkRejected(ctx context.Context, relations []domain.ContractProviderRelation) error {
	const op = "postgres.ContractRepo.MarkRejected"

	if len(relations) == 0 {
		return nil
	}

	type rejectDTO struct {
		Address      string `json:"address"`
		ProviderAddr string `json:"provider_address"`
	}

	dtos := make([]rejectDTO, len(relations))
	for i, rel := range relations {
		dtos[i] = rejectDTO{
			Address:      rel.ContractAddr,
			ProviderAddr: rel.ProviderAddress,
		}
	}

	data, err := json.Marshal(dtos)
	if err != nil {
		return fmt.Errorf("%s:%w", op, err)
	}

	_, err = r.db.Exec(ctx, `
		WITH to_delete AS (
			SELECT
				c->>'address'          AS address,
				c->>'provider_address' AS provider_address
			FROM jsonb_array_elements($1::jsonb) AS c
		)
		DELETE FROM providers.storage_contracts sc
		USING to_delete
		WHERE sc.address = to_delete.address
		  AND sc.provider_address = to_delete.provider_address
	`, string(data))
	if err != nil {
		return fmt.Errorf("%s:%w", op, err)
	}

	return nil
}

func (r *ContractRepo) SaveProofChecks(ctx context.Context, results []domain.ProofResult) error {
	const op = "postgres.ContractRepo.SaveProofChecks"

	if len(results) == 0 {
		return nil
	}

	dtos := make([]proofCheckDTO, len(results))
	for i, res := range results {
		dtos[i] = proofCheckDTO{
			ContractAddr: res.ContractAddr,
			ProviderAddr: res.ProviderAddr,
			Reason:       uint32(res.Reason),
		}
	}

	data, err := json.Marshal(dtos)
	if err != nil {
		return fmt.Errorf("%s:%w", op, err)
	}

	_, err = r.db.Exec(ctx, `
		WITH cte AS (
			SELECT
				c->>'contract_address' AS address,
				c->>'provider_address' AS provider_address,
				(c->>'reason')::integer AS reason
			FROM jsonb_array_elements($1::jsonb) AS c
		)
		UPDATE providers.storage_contracts sc
		SET
			reason           = cte.reason,
			reason_timestamp = now()
		FROM cte
		WHERE sc.address = cte.address
		  AND sc.provider_address = cte.provider_address
	`, string(data))
	if err != nil {
		return fmt.Errorf("%s:%w", op, err)
	}

	return nil
}
