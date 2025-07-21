package main

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/viper"
	log "github.com/sirupsen/logrus"
)

var globalRedis *redis.Client = nil

func redisConnect() (*redis.Client, error) {
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
	log.Info("Connected to Redis successfully")
	return client, nil
}

func redisPublish(ctx context.Context, channel string, message string) error {
	if globalRedis == nil {
		return fmt.Errorf("Redis client not initialized")
	}

	err := globalRedis.Publish(ctx, channel, message).Err()
	if err != nil {
		return fmt.Errorf("failed to publish to Redis channel %s: %w", channel, err)
	}

	log.Debugf("Published message to Redis channel '%s': %s", channel, message)
	return nil
}