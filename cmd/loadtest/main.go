// Утилита тестовой нагрузки: кладёт N триггеров в указанный cycle-stream и
// ждёт N результатов из result-stream. Печатает статистику и любые ошибки.
//
// Пример:
//
//	loadtest -addr localhost:6379 -cycle scan_master -count 5 -timeout 60s
//
// Cycle: один из scan_master | scan_wallets | resolve_endpoints |
// probe_rates | inspect_contracts | check_proofs | lookup_ipinfo.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/rwrrioe/mytonprovider-agent/internal/jobs"
)

func main() {
	addr := flag.String("addr", "localhost:6379", "redis address")
	password := flag.String("password", "", "redis password")
	db := flag.Int("db", 0, "redis db")
	prefix := flag.String("prefix", "mtpa", "stream prefix")
	cycle := flag.String("cycle", jobs.CycleScanMaster, "cycle type to trigger")
	count := flag.Int("count", 1, "number of triggers to send")
	timeout := flag.Duration("timeout", 60*time.Second, "wait timeout per result")
	flag.Parse()

	if *cycle == "" {
		log.Fatal("cycle is required")
	}

	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: *addr, Password: *password, DB: *db})
	defer rdb.Close()

	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis ping: %v", err)
	}

	cycleStream := fmt.Sprintf("%s:cycle:%s", *prefix, *cycle)
	resultStream := fmt.Sprintf("%s:result:%s", *prefix, *cycle)

	// Запоминаем точку чтения result-stream до отправки триггеров,
	// чтобы потом прочитать только новые сообщения.
	lastID := "$"

	// Отправляем триггеры
	jobIDs := make(map[string]bool, *count)
	sendStart := time.Now()
	for i := 0; i < *count; i++ {
		id := uuid.NewString()
		env := jobs.TriggerEnvelope{
			JobID:      id,
			Type:       *cycle,
			EnqueuedAt: time.Now(),
		}
		data, err := json.Marshal(env)
		if err != nil {
			log.Fatalf("marshal: %v", err)
		}
		if err = rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: cycleStream,
			Values: map[string]any{"data": data},
		}).Err(); err != nil {
			log.Fatalf("xadd: %v", err)
		}
		jobIDs[id] = true
		fmt.Printf("→ trigger #%d job_id=%s\n", i+1, id)
	}
	fmt.Printf("sent %d triggers in %s\n\n", *count, time.Since(sendStart))

	// Читаем результаты
	deadline := time.Now().Add(*timeout * time.Duration(*count))
	gotOK, gotErr := 0, 0
	for len(jobIDs) > 0 && time.Now().Before(deadline) {
		readCtx, cancel := context.WithTimeout(ctx, time.Until(deadline))
		streams, err := rdb.XRead(readCtx, &redis.XReadArgs{
			Streams: []string{resultStream, lastID},
			Count:   int64(*count),
			Block:   2 * time.Second,
		}).Result()
		cancel()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			log.Printf("xread: %v", err)
			continue
		}

		for _, s := range streams {
			for _, msg := range s.Messages {
				lastID = msg.ID
				raw, _ := msg.Values["data"].(string)
				var env jobs.ResultEnvelope
				if err = json.Unmarshal([]byte(raw), &env); err != nil {
					log.Printf("malformed result %s: %v", msg.ID, err)
					continue
				}
				if !jobIDs[env.JobID] {
					continue
				}
				delete(jobIDs, env.JobID)
				if env.Status == jobs.StatusOK {
					gotOK++
					fmt.Printf("← ok    job_id=%s agent=%s\n", env.JobID, env.AgentID)
				} else {
					gotErr++
					fmt.Printf("← error job_id=%s err=%q\n", env.JobID, env.Error)
				}
			}
		}
	}

	fmt.Printf("\nresults: ok=%d error=%d missing=%d total=%d\n", gotOK, gotErr, len(jobIDs), *count)
	if len(jobIDs) > 0 {
		fmt.Println("missing job_ids:")
		for id := range jobIDs {
			fmt.Printf("  %s\n", id)
		}
		os.Exit(1)
	}
	if gotErr > 0 {
		os.Exit(2)
	}
}
