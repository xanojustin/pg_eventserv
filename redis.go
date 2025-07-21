package main

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/viper"
	log "github.com/sirupsen/logrus"
)

var (
	globalRedis *redis.Client = nil
	redisConnMutex sync.RWMutex
	retryAttempt int
	lastSuccessfulConnect time.Time
)

func redisConnect() (*redis.Client, error) {
	redisConnMutex.Lock()
	defer redisConnMutex.Unlock()

	redisAddr := viper.GetString("RedisAddr")
	redisPassword := viper.GetString("RedisPassword")
	redisDB := viper.GetInt("RedisDB")
	redisMaxRetries := viper.GetInt("RedisMaxRetries")
	redisPoolSize := viper.GetInt("RedisPoolSize")

	log.WithFields(log.Fields{
		"addr": redisAddr,
		"db":   redisDB,
	}).Info("Connecting to Redis")

	client := redis.NewClient(&redis.Options{
		Addr:        redisAddr,
		Password:    redisPassword,
		DB:          redisDB,
		MaxRetries:  redisMaxRetries,
		PoolSize:    redisPoolSize,
		PoolTimeout: 4 * time.Second,
		ReadTimeout: 3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	globalRedis = client
	retryAttempt = 0
	lastSuccessfulConnect = time.Now()
	log.Info("Connected to Redis successfully")
	return client, nil
}

func ensureRedisConnection(ctx context.Context) error {
	redisConnMutex.RLock()
	if globalRedis != nil {
		// Test the connection
		testCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := globalRedis.Ping(testCtx).Err()
		cancel()
		if err == nil {
			redisConnMutex.RUnlock()
			return nil
		}
		log.Warnf("Redis connection test failed: %s", err)
	}
	redisConnMutex.RUnlock()

	// Connection is down, attempt reconnection with backoff
	for {
		// Check if context is cancelled
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Calculate backoff delay (exponential backoff with max of 60 seconds)
		backoffDelay := time.Duration(math.Min(math.Pow(2, float64(retryAttempt)), 60)) * time.Second
		log.Warnf("Redis connection lost. Retrying in %v (attempt %d)", backoffDelay, retryAttempt+1)

		// Wait for backoff period
		select {
		case <-time.After(backoffDelay):
			// Continue with retry
		case <-ctx.Done():
			return ctx.Err()
		}

		retryAttempt++

		// Attempt to reconnect
		_, err := redisConnect()
		if err == nil {
			log.Info("Redis reconnection successful")
			return nil
		}

		log.Errorf("Redis reconnection attempt %d failed: %s", retryAttempt, err)
	}
}

func redisPublish(ctx context.Context, channel string, message string) error {
	// Ensure we have a valid Redis connection
	if err := ensureRedisConnection(ctx); err != nil {
		return fmt.Errorf("failed to establish Redis connection: %w", err)
	}

	redisConnMutex.RLock()
	client := globalRedis
	redisConnMutex.RUnlock()

	if client == nil {
		return fmt.Errorf("Redis client not initialized")
	}

	// Get the configured list key
	listKey := viper.GetString("RedisListKey")

	// First, push the message to the Redis list
	err := client.RPush(ctx, listKey, message).Err()
	if err != nil {
		return fmt.Errorf("failed to push to Redis list %s: %w", listKey, err)
	}
	log.Debugf("Pushed message to Redis list '%s': %s", listKey, message)

	// Then publish notification to the channel
	err = client.Publish(ctx, channel, message).Err()
	if err != nil {
		return fmt.Errorf("failed to publish to Redis channel %s: %w", channel, err)
	}

	log.Debugf("Published message to Redis channel '%s': %s", channel, message)
	return nil
}