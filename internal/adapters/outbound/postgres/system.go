package postgres

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

const lastLTKey = "masterWalletLastLT"

type SystemRepo struct {
	db *pgxpool.Pool
}

func NewSystemRepo(db *pgxpool.Pool) *SystemRepo {
	return &SystemRepo{db: db}
}

// SetMasterWalletLT сохраняет LT как текст — value в system.params имеет тип varchar.
func (r *SystemRepo) SetMasterWalletLT(ctx context.Context, lt uint64) error {
	const op = "postgres.SystemRepo.SetMasterWalletLT"

	_, err := r.db.Exec(ctx, `
		INSERT INTO system.params (key, value)
		VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE
		SET value      = EXCLUDED.value,
		    updated_at = now()
	`, lastLTKey, strconv.FormatUint(lt, 10))
	if err != nil {
		return fmt.Errorf("%s:%w", op, err)
	}

	return nil
}

// GetMasterWalletLT читает LT из varchar-колонки и парсит в uint64.
// Возвращает 0 если запись ещё не существует.
func (r *SystemRepo) GetMasterWalletLT(ctx context.Context) (uint64, error) {
	const op = "postgres.SystemRepo.GetMasterWalletLT"

	var val string

	rows, err := r.db.Query(ctx, `
		SELECT value
		FROM system.params
		WHERE key = $1
		LIMIT 1
	`, lastLTKey)
	if err != nil {
		return 0, fmt.Errorf("%s:%w", op, err)
	}
	defer rows.Close()

	if rows.Next() {
		if err = rows.Scan(&val); err != nil {
			return 0, fmt.Errorf("%s:%w", op, err)
		}
	}

	if err = rows.Err(); err != nil {
		return 0, fmt.Errorf("%s:%w", op, err)
	}

	if val == "" {
		return 0, nil
	}

	lt, err := strconv.ParseUint(val, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s:%w", op, err)
	}

	return lt, nil
}
