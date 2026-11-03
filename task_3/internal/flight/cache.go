package flight

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"flightbooking/internal/storage"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

type cache struct {
	client   *redis.Client
	disabled atomic.Bool
}

func newCache() *cache {
	c := &cache{client: redis.NewFailoverClient(&redis.FailoverOptions{
		MasterName:    storage.Env("REDIS_MASTER_NAME", "flights"),
		SentinelAddrs: strings.Split(storage.Env("REDIS_SENTINELS", "sentinel:26379"), ","),
		DialTimeout:   200 * time.Millisecond, ReadTimeout: 200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond, ContextTimeoutEnabled: true, MaxRetries: -1,
	})}
	c.invalidate()
	return c
}

func (c *cache) get(ctx context.Context, key string, out proto.Message) bool {
	if c.disabled.Load() {
		slog.Info("cache miss", "key", key, "reason", "disabled")
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	data, err := c.client.Get(ctx, key).Bytes()
	hit := err == nil && proto.Unmarshal(data, out) == nil
	if hit {
		slog.Info("cache hit", "key", key)
	} else {
		slog.Info("cache miss", "key", key)
	}
	return hit
}

func (c *cache) set(ctx context.Context, key string, value proto.Message) {
	if c.disabled.Load() {
		return
	}
	data, err := proto.Marshal(value)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err = c.client.Set(ctx, key, data, 5*time.Minute).Err(); err != nil {
		slog.Warn("cache write failed", "error", err)
	}
}

func (c *cache) invalidate() {
	// This Redis DB belongs exclusively to Flight Service. Clearing it also
	// invalidates every affected search, including searches without a date.
	// The service lock prevents a concurrent read from filling an old value.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := c.client.FlushDB(ctx).Err()
	c.disabled.Store(err != nil)
	if err != nil {
		slog.Warn("cache disabled after failed invalidation", "error", err)
	}
}
