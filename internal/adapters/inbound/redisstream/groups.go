package redisstream

import (
	"context"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

// EnsureGroup создаёт consumer-group на стриме (с MKSTREAM, чтобы стрим тоже создался,
// если его ещё нет). Игнорирует BUSYGROUP — нормальная ситуация при рестарте.
func EnsureGroup(
	ctx context.Context,
	rdb *redis.Client,
	stream, group string,
) error {
	const op = "redisstream.EnsureGroup"

	err := rdb.XGroupCreateMkStream(ctx, stream, group, "$").Err()

	if err == nil || strings.Contains(err.Error(), "BUSYGROUP") {
		return nil
	}
	
	return fmt.Errorf("%s: stream=%s group=%s: %w", op, stream, group, err)
}
