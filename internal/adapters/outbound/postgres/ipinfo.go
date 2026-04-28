package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rwrrioe/mytonprovider-agent/internal/domain"
)

type IPInfoRepo struct {
	db *pgxpool.Pool
}

func NewIPInfoRepo(db *pgxpool.Pool) *IPInfoRepo {
	return &IPInfoRepo{db: db}
}

// GetProvidersIPs возвращает провайдеров у которых есть IP, но ip_info пуста или устарела.
// Read-only — write принадлежит бэкенду.
func (r *IPInfoRepo) GetProvidersIPs(ctx context.Context) ([]domain.ProviderEndpoint, error) {
	const op = "postgres.IPInfoRepo.GetProvidersIPs"

	rows, err := r.db.Query(ctx, `
		SELECT public_key, ip
		FROM providers.providers
		WHERE length(ip) > 0
		  AND (ip_info = '{}'::jsonb OR ip_info->>'ip' <> ip)
	`)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s:%w", op, err)
	}
	defer rows.Close()

	var endpoints []domain.ProviderEndpoint
	for rows.Next() {
		var ep domain.ProviderEndpoint
		if err = rows.Scan(&ep.PublicKey, &ep.Provider.IP); err != nil {
			return nil, fmt.Errorf("%s:%w", op, err)
		}
		endpoints = append(endpoints, ep)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("%s:%w", op, err)
	}

	return endpoints, nil
}
