package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/viper"
	log "github.com/sirupsen/logrus"
)

type QueuedMessage struct {
	Channel string
	Message string
	Timestamp time.Time
}

// prefixHook adds a prefix to all Redis keys
type prefixHook struct {
	prefix string
}

func (h prefixHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (h prefixHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if h.prefix != "" {
			h.addPrefixToCmd(cmd)
		}
		return next(ctx, cmd)
	}
}

func (h prefixHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if h.prefix != "" {
			for _, cmd := range cmds {
				h.addPrefixToCmd(cmd)
			}
		}
		return next(ctx, cmds)
	}
}

func (h prefixHook) addPrefixToCmd(cmd redis.Cmder) {
	switch cmd.Name() {
	// Commands with key as first argument
	case "rpush", "lpush", "llen", "lrange", "ltrim", "get", "set", "del", "exists", "expire", "ttl", "incr", "decr":
		args := cmd.Args()
		if len(args) > 1 {
			if key, ok := args[1].(string); ok {
				args[1] = h.prefix + key
			}
		}
	// PUBLISH has channel as first argument
	case "publish":
		args := cmd.Args()
		if len(args) > 1 {
			if channel, ok := args[1].(string); ok {
				args[1] = h.prefix + channel
			}
		}
	}
}

var (
	globalRedis *redis.Client = nil
	redisConnMutex sync.RWMutex
	retryAttempt int
	lastSuccessfulConnect time.Time
	
	// Message queue for when Redis is down
	messageQueue []QueuedMessage
	queueMutex sync.Mutex
	maxQueueSize int
)

func redisConnect() (*redis.Client, error) {
	redisConnMutex.Lock()
	defer redisConnMutex.Unlock()

	redisAddr := viper.GetString("RedisAddr")
	redisPassword := viper.GetString("RedisPassword")
	redisPrefix := viper.GetString("RedisPrefix")
	redisDB := viper.GetInt("RedisDB")
	redisMaxRetries := viper.GetInt("RedisMaxRetries")
	redisPoolSize := viper.GetInt("RedisPoolSize")
	maxQueueSize = viper.GetInt("RedisQueueMaxSize")

	// Check if the address has tls:// prefix
	useTLS := false
	if strings.HasPrefix(redisAddr, "tls://") {
		useTLS = true
		redisAddr = strings.TrimPrefix(redisAddr, "tls://")
	}

	log.WithFields(log.Fields{
		"addr": redisAddr,
		"db":   redisDB,
		"prefix": redisPrefix,
		"tls": useTLS,
	}).Info("Connecting to Redis")

	options := &redis.Options{
		Addr:        redisAddr,
		Password:    redisPassword,
		DB:          redisDB,
		MaxRetries:  redisMaxRetries,
		PoolSize:    redisPoolSize,
		PoolTimeout: 4 * time.Second,
		ReadTimeout: 3 * time.Second,
		WriteTimeout: 3 * time.Second,
	}

	// Configure TLS if the prefix was present
	if useTLS {
		options.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}

	client := redis.NewClient(options)

	// Add prefix hook if prefix is configured
	if redisPrefix != "" {
		client.AddHook(prefixHook{prefix: redisPrefix})
		log.Infof("Redis key prefix enabled: %s", redisPrefix)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	globalRedis = client
	retryAttempt = 0
	lastSuccessfulConnect = time.Now()
	log.Info("Connected to Redis successfully")
	
	// Process any queued messages when reconnecting
	go flushQueuedMessages()
	
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

func queueMessage(channel, message string) {
	queueMutex.Lock()
	defer queueMutex.Unlock()
	
	// Add message to queue
	queuedMsg := QueuedMessage{
		Channel:   channel,
		Message:   message,
		Timestamp: time.Now(),
	}
	
	messageQueue = append(messageQueue, queuedMsg)
	log.Warnf("Queued message for channel '%s' (queue size: %d)", channel, len(messageQueue))
	
	// Prevent unbounded growth by removing oldest messages if needed
	if len(messageQueue) > maxQueueSize {
		removed := len(messageQueue) - maxQueueSize
		messageQueue = messageQueue[removed:]
		log.Warnf("Message queue full, removed %d oldest messages", removed)
	}
}

func flushQueuedMessages() {
	queueMutex.Lock()
	queuedMessages := make([]QueuedMessage, len(messageQueue))
	copy(queuedMessages, messageQueue)
	messageQueue = messageQueue[:0] // Clear the queue
	queueMutex.Unlock()
	
	if len(queuedMessages) == 0 {
		return
	}
	
	log.Infof("Flushing %d queued messages to Redis", len(queuedMessages))
	
	ctx := context.Background()
	successCount := 0
	
	for _, msg := range queuedMessages {
		err := redisPublishDirect(ctx, msg.Channel, msg.Message)
		if err != nil {
			log.Errorf("Failed to flush queued message: %s", err)
			// Re-queue failed messages
			queueMessage(msg.Channel, msg.Message)
		} else {
			successCount++
		}
	}
	
	log.Infof("Successfully flushed %d/%d queued messages", successCount, len(queuedMessages))
}

func redisPublishDirect(ctx context.Context, channel string, message string) error {
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

func redisPublish(ctx context.Context, channel string, message string) error {
	// Quick check if Redis is available
	redisConnMutex.RLock()
	client := globalRedis
	redisConnMutex.RUnlock()
	
	// If Redis client is nil, queue the message immediately
	if client == nil {
		log.Debugf("Redis not connected, queuing message for channel '%s'", channel)
		queueMessage(channel, message)
		return nil
	}
	
	// Test if Redis is actually working with a quick ping
	testCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	err := client.Ping(testCtx).Err()
	cancel()
	
	if err != nil {
		log.Debugf("Redis ping failed (%s), queuing message for channel '%s'", err, channel)
		queueMessage(channel, message)
		return nil
	}
	
	// Try to publish directly
	err = redisPublishDirect(ctx, channel, message)
	if err != nil {
		log.Debugf("Redis publish failed (%s), queuing message for channel '%s'", err, channel)
		queueMessage(channel, message)
		return nil
	}
	
	return nil
}