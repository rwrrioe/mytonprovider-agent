package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rwrrioe/mytonprovider-agent/internal/domain"
)

type EndpointRepo struct {
	db *pgxpool.Pool
}

func NewEndpointRepo(db *pgxpool.Pool) *EndpointRepo {
	return &EndpointRepo{db: db}
}

// LoadFresh возвращает (endpoint, true, nil) если endpoint существует и обновлялся
// не позднее maxAge назад. Иначе — (zero, false, nil).
func (r *EndpointRepo) LoadFresh(ctx context.Context, pubkey string, maxAge time.Duration) (domain.ProviderEndpoint, bool, error) {
	const op = "postgres.EndpointRepo.LoadFresh"

	var ep domain.ProviderEndpoint
	var ip, storageIP *string
	var port, storagePort *int32

	err := r.db.QueryRow(ctx, `
		SELECT public_key, ip, port, storage_ip, storage_port
		FROM providers.providers
		WHERE lower(public_key) = lower($1)
		  AND ip IS NOT NULL
		  AND updated_at > NOW() - make_interval(secs => $2)
	`, pubkey, maxAge.Seconds()).Scan(
		&ep.PublicKey,
		&ip, &port,
		&storageIP, &storagePort,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ProviderEndpoint{}, false, nil
		}
		return domain.ProviderEndpoint{}, false, fmt.Errorf("%s:%w", op, err)
	}

	if ip != nil {
		ep.Provider.IP = *ip
	}
	if port != nil {
		ep.Provider.Port = *port
	}
	if storageIP != nil {
		ep.Storage.IP = *storageIP
	}
	if storagePort != nil {
		ep.Storage.Port = *storagePort
	}

	return ep, true, nil
}

// LoadAll возвращает все endpoint-ы с непустым IP, индексированные по pubkey.
// Используется в update-цикле для обогащения IP-инфо.
func (r *EndpointRepo) LoadAll(ctx context.Context) (map[string]domain.ProviderEndpoint, error) {
	const op = "postgres.EndpointRepo.LoadAll"

	rows, err := r.db.Query(ctx, `
		SELECT public_key, ip, port, storage_ip, storage_port, updated_at
		FROM providers.providers
		WHERE ip IS NOT NULL AND length(ip) > 0
	`)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s:%w", op, err)
	}
	defer rows.Close()

	result := make(map[string]domain.ProviderEndpoint)
	for rows.Next() {
		var ep domain.ProviderEndpoint
		var ip, storageIP *string
		var port, storagePort *int32

		if err = rows.Scan(
			&ep.PublicKey,
			&ip, &port,
			&storageIP, &storagePort,
			&ep.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("%s:%w", op, err)
		}

		if ip != nil {
			ep.Provider.IP = *ip
		}
		if port != nil {
			ep.Provider.Port = *port
		}
		if storageIP != nil {
			ep.Storage.IP = *storageIP
		}
		if storagePort != nil {
			ep.Storage.Port = *storagePort
		}

		result[ep.PublicKey] = ep
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("%s:%w", op, err)
	}

	return result, nil
}
