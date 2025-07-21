package main

import (

	// Core
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"time"

	// Configuration utilities
	"github.com/pborman/getopt/v2"
	"github.com/spf13/viper"

	// Logging
	log "github.com/sirupsen/logrus"

	// PostgreSQL connection
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
)

var pool *pgxpool.Pool

// programName is the name string we use
const programName string = "pg_eventserv"

// programVersion is the version string we use
var programVersion string


// globalDb is a global database connection pointer
var globalDb *pgxpool.Pool = nil


type RedisContext struct {
	listenChannels      map[string]bool
	listenChannelsMutex *sync.Mutex
	db                  *pgxpool.Pool
}




/**********************************************************************/

func init() {
	viper.SetDefault("DbConnection", "sslmode=disable")
	viper.SetDefault("RedisAddr", "localhost:6379")
	viper.SetDefault("RedisPassword", "")
	viper.SetDefault("RedisDB", 0)
	viper.SetDefault("RedisMaxRetries", 3)
	viper.SetDefault("RedisPoolSize", 10)
	viper.SetDefault("UrlBase", "")
	viper.SetDefault("Debug", false)
	// 1d, 1h, 1m, 1s, see https://golang.org/pkg/time/#ParseDuration
	viper.SetDefault("DbPoolMaxConnLifeTime", "1h")
	viper.SetDefault("DbPoolMaxConns", 4)
	viper.SetDefault("ListenChannel", "channelname")
	viper.SetDefault("RedisListKey", "channelevents")
	viper.SetDefault("RedisQueueMaxSize", 10000)
	if programVersion == "" {
		programVersion = "latest"
	}
}

func main() {

	flagDebugOn := getopt.BoolLong("debug", 'd', "log debugging information")
	flagConfigFile := getopt.StringLong("config", 'c', "", "full path to config file", "config.toml")
	flagHelpOn := getopt.BoolLong("help", 'h', "display help output")
	flagVersionOn := getopt.BoolLong("version", 'v', "display version number")
	getopt.Parse()

	if *flagHelpOn {
		getopt.PrintUsage(os.Stdout)
		os.Exit(1)
	}

	if *flagVersionOn {
		fmt.Printf("%s %s\n", programName, programVersion)
		os.Exit(0)
	}

	viper.AutomaticEnv()
	viper.SetEnvPrefix("es")

	// Commandline over-rides config file for debugging
	if *flagDebugOn {
		viper.Set("Debug", true)
		log.SetLevel(log.TraceLevel)
	}

	if *flagConfigFile != "" {
		viper.SetConfigFile(*flagConfigFile)
	} else {
		viper.SetConfigName(programName)
		viper.SetConfigType("toml")
		viper.AddConfigPath("./config")
		viper.AddConfigPath("/config")
		viper.AddConfigPath("/etc")
	}

	// Report our status
	log.Infof("%s %s", programName, programVersion)
	log.Info("Run with --help parameter for commandline options")

	// Read environment configuration first
	if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" {
		viper.Set("DbConnection", dbURL)
		log.Info("Using database connection info from environment variable DATABASE_URL")
	}

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			log.Debugf("viper.ConfigFileNotFoundError: %s", err)
		} else {
			if _, ok := err.(viper.UnsupportedConfigError); ok {
				log.Debugf("viper.UnsupportedConfigError: %s", err)
			} else {
				log.Fatalf("Configuration file error: %s", err)
			}
		}
	} else {
		// Log location of filename we found...
		if cf := viper.ConfigFileUsed(); cf != "" {
			log.Infof("Using config file: %s", cf)
		} else {
			log.Info("Config file: none found, using defaults")
		}
	}

	listenChannel := viper.GetString("ListenChannel")
	log.Infof("Listening to channel: %s", listenChannel)
	log.Infof("Redis server: %s", viper.GetString("RedisAddr"))

	// Make a database connection pool
	dbPool, err := dbConnect()
	if err != nil {
		os.Exit(1)
	}

	// Connect to Redis
	_, err = redisConnect()
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %s", err)
	}

	redisCtx := RedisContext{
		listenChannels:      make(map[string]bool),
		listenChannelsMutex: &sync.Mutex{},
		db:                  dbPool,
	}

	ctxValue := context.WithValue(
		context.Background(),
		"redisCtx", redisCtx)
	ctxCancel, cancel := context.WithCancel(ctxValue)

	// Start listening to the configured channel
	go listenForNotify(ctxCancel, listenChannel)

	// Start database health check goroutine
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				// Try to ping the database
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := dbPool.Ping(ctx)
				cancel()

				if err != nil {
					log.Fatalf("Database health check failed: %s", err)
				}
				log.Trace("Database health check passed")
			case <-ctxCancel.Done():
				return
			}
		}
	}()

	// Wait here for interrupt signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	// Shut down everything attached to this context before exit
	cancel()
	time.Sleep(100 * time.Millisecond)
}

// Goroutine to watch a PgSQL LISTEN/NOTIFY channel, and
// publish the notification to Redis
func listenForNotify(ctx context.Context, listenChannel string) {

	redisInfo := ctx.Value("redisCtx").(RedisContext)

	// Mark this channel as being listened to
	redisInfo.listenChannelsMutex.Lock()
	redisInfo.listenChannels[listenChannel] = true
	redisInfo.listenChannelsMutex.Unlock()

	// Draw a connection from the pool to use for listening
	conn, err := redisInfo.db.Acquire(ctx)
	if err != nil {
		// Check if this is a context cancellation (normal shutdown)
		if ctx.Err() != nil {
			log.Debugf("Connection acquisition cancelled: %s", err)
			return
		}
		// Otherwise, this is a database connection error
		log.Fatalf("Error acquiring database connection: %s", err)
	}
	defer conn.Release()
	log.Infof("Listening to the '%s' database channel", listenChannel)

	// Send the LISTEN command to the connection
	listenSQL := fmt.Sprintf("LISTEN %s", pgx.Identifier{listenChannel}.Sanitize())
	_, err = conn.Exec(ctx, listenSQL)
	if err != nil {
		log.Fatalf("Error listening to '%s' channel: %s", listenChannel, err)
	}

	// Wait for notifications to come off the connection
	for {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			// Check if this is a context cancellation (normal shutdown)
			if ctx.Err() != nil {
				log.Debugf("WaitForNotification context cancelled: %s", err)
				return
			}
			// Otherwise, this is a database connection error
			log.Fatalf("Database connection lost on channel '%s': %s", listenChannel, err)
		}

		log.Debugf("NOTIFY received, channel '%s', payload '%s'",
			notification.Channel,
			notification.Payload)

		// Publish the notification to Redis
		err = redisPublish(ctx, listenChannel, notification.Payload)
		if err != nil {
			log.Errorf("Failed to publish to Redis: %s", err)
			// Continue processing rather than exiting - Redis will reconnect automatically
		}
	}
}

