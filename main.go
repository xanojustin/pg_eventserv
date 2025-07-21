package main

import (

	// Core
	"context"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	// REST routing
	"github.com/gorilla/mux"

	// Pattern match channel names
	"github.com/komem3/glob"

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


func channelValid(checkChannel string) bool {
	validList := viper.GetStringSlice("Channels")
	for _, channelGlob := range validList {
		matcher, err := glob.Compile(channelGlob)
		if err != nil {
			log.Warnf("invalid channel pattern in Channels configuration: %s", channelGlob)
			return false
		}
		isValid := matcher.MatchString(checkChannel)
		if isValid {
			return true
		}
	}
	log.Infof("Web socket creation request invalid channel '%s'", checkChannel)
	return false
}


/**********************************************************************/

func init() {
	viper.SetDefault("DbConnection", "sslmode=disable")
	viper.SetDefault("HttpHost", "0.0.0.0")
	viper.SetDefault("HttpPort", 7700)
	viper.SetDefault("RedisAddr", "localhost:6379")
	viper.SetDefault("RedisPassword", "")
	viper.SetDefault("RedisDB", 0)
	viper.SetDefault("RedisMaxRetries", 3)
	viper.SetDefault("RedisPoolSize", 10)
	viper.SetDefault("UrlBase", "")
	viper.SetDefault("Debug", false)
	viper.SetDefault("AssetsPath", "./assets")
	// 1d, 1h, 1m, 1s, see https://golang.org/pkg/time/#ParseDuration
	viper.SetDefault("DbPoolMaxConnLifeTime", "1h")
	viper.SetDefault("DbPoolMaxConns", 4)
	viper.SetDefault("CORSOrigins", []string{"*"})
	viper.SetDefault("BasePath", "/")
	viper.SetDefault("Channels", []string{"*"})
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

	basePath := viper.GetString("BasePath")
	log.Infof("Serving HTTP  at %s/", formatBaseURL(fmt.Sprintf("http://%s:%d",
		viper.GetString("HttpHost"), viper.GetInt("HttpPort")), basePath))
	log.Infof("Channels available: %s", strings.Join(viper.GetStringSlice("Channels"), ", "))
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

	// HTTP router setup
	trimBasePath := strings.TrimLeft(basePath, "/")
	r := mux.NewRouter().
		StrictSlash(true).
		PathPrefix("/" + trimBasePath).
		Subrouter()
	// Return front page
	r.Handle("/", http.HandlerFunc(requestIndexHTML))
	r.Handle("/index.html", http.HandlerFunc(requestIndexHTML))
	// Initiate Redis channel subscription
	r.Handle("/listen/{channel}", redisChannelHandler(ctxCancel))

	// Prepare the HTTP server object
	// More "production friendly" timeouts
	// https://blog.simon-frey.eu/go-as-in-golang-standard-net-http-config-will-break-your-production/#You_should_at_least_do_this_The_easy_path
	s := &http.Server{
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		Addr:         fmt.Sprintf("%s:%d", viper.GetString("HttpHost"), viper.GetInt("HttpPort")),
		Handler:      r,
	}

	// Start http service
	go func() {
		// ListenAndServe returns http.ErrServerClosed when the server receives
		// a call to Shutdown(). Other errors are unexpected.
		if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

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
	log.Infof("Listening to the '%s' database channel\n", listenChannel)

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
		}
	}
}

// Handler to start listening to a PostgreSQL channel and publishing to Redis
func redisChannelHandler(ctx context.Context) http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		channel := mux.Vars(r)["channel"]
		log.Debugf("request to listen to channel '%s' received", channel)

		redisInfo := ctx.Value("redisCtx").(RedisContext)

		// Check the channel name against the allow channel names list/patterns
		if !channelValid(channel) {
			w.WriteHeader(403) // Forbidden
			errMsg := fmt.Sprintf("requested channel '%s' is not allowed", channel)
			log.Debug(errMsg)
			w.Write([]byte(errMsg))
			return
		}

		// Check if we're already listening to this channel
		redisInfo.listenChannelsMutex.Lock()
		alreadyListening := redisInfo.listenChannels[channel]
		redisInfo.listenChannelsMutex.Unlock()

		if !alreadyListening {
			// Start a new database listener for this channel
			go listenForNotify(ctx, channel)
			w.WriteHeader(200)
			w.Write([]byte(fmt.Sprintf("Started listening to channel '%s' and publishing to Redis\n", channel)))
		} else {
			w.WriteHeader(200)
			w.Write([]byte(fmt.Sprintf("Already listening to channel '%s'\n", channel)))
		}
	})
}

func requestIndexHTML(w http.ResponseWriter, r *http.Request) {
	log.WithFields(log.Fields{
		"event": "request",
		"topic": "root",
	}).Trace("requestIndexHTML")

	type IndexFields struct {
		BaseURL   string
		Channels  string
		RedisAddr string
	}

	indexFields := IndexFields{
		BaseURL:   serverBase(r),
		Channels:  strings.Join(viper.GetStringSlice("Channels"), ", "),
		RedisAddr: viper.GetString("RedisAddr"),
	}

	tmpl, err := template.ParseFiles(fmt.Sprintf("%s/index.html", viper.GetString("AssetsPath")))
	if err != nil {
		return
	}
	tmpl.Execute(w, indexFields)
	return
}
