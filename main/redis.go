package main

import (
	"context"
	localenv "mensalocalizations/tools/env"
	"time"

	"github.com/go-redis/redis/v8"
	"golang.org/x/sync/singleflight"
)

var (
	rdb = redis.NewClient(&redis.Options{
		Addr:     localenv.GetRedisAddr(),
		Password: localenv.GetRedisPassword(),
		DB:       0,
	})

	sf singleflight.Group
)

// redisPut writes a value with the given TTL into Redis using the shared client.
// If ttl <= 0, the key is stored without expiration (infinite TTL).
func redisPut(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl <= 0 {
		return rdb.Set(ctx, key, value, 0).Err()
	}
	return rdb.Set(ctx, key, value, ttl).Err()
}

// redisGet fetches a value by key from Redis using the shared client.
// It returns the raw bytes and any error from the underlying call.
func redisGet(ctx context.Context, key string) ([]byte, error) {
	return rdb.Get(ctx, key).Bytes()
}
