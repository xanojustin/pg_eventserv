package main

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	// Data
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/log/logrusadapter"
	"github.com/jackc/pgx/v4/pgxpool"

	// Config
	"github.com/spf13/viper"

	// Logging
	log "github.com/sirupsen/logrus"
)

var (
	dbConnMutex       sync.RWMutex
	dbRetryAttempt    int
	lastDbConnect     time.Time
)

func dbConnect() (*pgxpool.Pool, error) {
	dbConnMutex.Lock()
	defer dbConnMutex.Unlock()
	
	if globalDb == nil {
		dbConnection := viper.GetString("DbConnection")
		config, err := pgxpool.ParseConfig(dbConnection)
		if err != nil {
			return nil, fmt.Errorf("failed to parse database config: %w", err)
		}

		// Read and parse connection lifetime
		dbPoolMaxLifeTime, errt := time.ParseDuration(viper.GetString("DbPoolMaxConnLifeTime"))
		if errt != nil {
			return nil, fmt.Errorf("failed to parse DbPoolMaxConnLifeTime: %w", errt)
		}
		config.MaxConnLifetime = dbPoolMaxLifeTime

		// Read and parse max connections
		dbPoolMaxConns := viper.GetInt32("DbPoolMaxConns")
		if dbPoolMaxConns > 0 {
			config.MaxConns = dbPoolMaxConns
		}

		// Read current log level and use one less-fine level
		// below that
		config.ConnConfig.Logger = logrusadapter.NewLogger(log.New())
		levelString, _ := (log.GetLevel() - 1).MarshalText()
		pgxLevel, _ := pgx.LogLevelFromString(string(levelString))
		config.ConnConfig.LogLevel = pgxLevel

		// Connect!
		globalDb, err = pgxpool.ConnectConfig(context.Background(), config)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to database: %w", err)
		}
		dbName := config.ConnConfig.Config.Database
		dbUser := config.ConnConfig.Config.User
		dbHost := config.ConnConfig.Config.Host
		log.Infof("Connected as '%s' to '%s' @ '%s'", dbUser, dbName, dbHost)
		
		dbRetryAttempt = 0
		lastDbConnect = time.Now()

		return globalDb, nil
	}
	return globalDb, nil
}

func ensureDbConnection(ctx context.Context) error {
	dbConnMutex.RLock()
	if globalDb != nil {
		// Test the connection
		testCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := globalDb.Ping(testCtx)
		cancel()
		if err == nil {
			dbConnMutex.RUnlock()
			return nil
		}
		log.Warnf("Database connection test failed: %s", err)
	}
	dbConnMutex.RUnlock()

	// Connection is down, attempt reconnection with backoff
	for {
		// Check if context is cancelled
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Calculate backoff delay (exponential backoff with max of 60 seconds)
		backoffDelay := time.Duration(math.Min(math.Pow(2, float64(dbRetryAttempt)), 60)) * time.Second
		log.Warnf("Database connection lost. Retrying in %v (attempt %d)", backoffDelay, dbRetryAttempt+1)

		// Wait for backoff period
		select {
		case <-time.After(backoffDelay):
			// Continue with retry
		case <-ctx.Done():
			return ctx.Err()
		}

		dbRetryAttempt++

		// Close existing pool if it exists
		dbConnMutex.Lock()
		if globalDb != nil {
			globalDb.Close()
			globalDb = nil
		}
		dbConnMutex.Unlock()

		// Attempt to reconnect
		_, err := dbConnect()
		if err == nil {
			log.Info("Database reconnection successful")
			return nil
		}

		log.Errorf("Database reconnection attempt %d failed: %s", dbRetryAttempt, err)
	}
}
