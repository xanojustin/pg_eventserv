# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

pg_eventserv is a PostgreSQL event server written in Go that bridges PostgreSQL's NOTIFY/LISTEN functionality with Redis pub/sub channels, allowing any Redis client to receive PostgreSQL notifications.

## Development Commands

### Building
```bash
# Build local binary
make build

# Build using Docker golang image
make bin-docker

# Build Docker container
make build-docker

# Full release (docs, binary, container)
make release
```

### Running
```bash
# Set database and Redis connections
export DATABASE_URL=postgresql://username:password@host/dbname
export ES_REDISADDR=localhost:6379

# Run the server
./pg_eventserv

# Run with debug logging
./pg_eventserv --debug
```

### Testing
The project has no automated tests. Use the built-in web interface at http://localhost:7700/ to start channel listeners, and use any Redis client to subscribe to channels for testing.

## Architecture

### Core Components

1. **main.go**: HTTP server setup, channel listener management, and signal handling
2. **db.go**: PostgreSQL connection pool management using pgx
3. **redis.go**: Redis client and pub/sub functionality
4. **util.go**: URL formatting and proxy header utilities

### Request Flow

1. Client makes HTTP request to: `http://host:7700/listen/{channel}`
2. Channel name validated against allowed patterns (glob matching)
3. Database listener goroutine created for new channels
4. PostgreSQL NOTIFY events are published to Redis channels
5. Redis subscribers receive events via pub/sub

### Key Dependencies

- `gorilla/mux`: HTTP routing
- `jackc/pgx/v4`: PostgreSQL driver with connection pooling
- `redis/go-redis/v9`: Redis client library
- `spf13/viper`: Configuration management (TOML files, env vars, flags)
- `komem3/glob`: Channel pattern matching

### Configuration Hierarchy

1. Command-line flags (highest priority)
2. Environment variables (prefix: `ES_`)
3. TOML configuration file
4. Built-in defaults

### Database Integration

Events are typically generated using PostgreSQL triggers:

```sql
-- Example trigger function
CREATE OR REPLACE FUNCTION notify_change() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('channel_name', to_jsonb(NEW)::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
```

### Redis Integration

- Events published to Redis channels with same name as PostgreSQL channel
- No authentication built-in (relies on Redis security and channel pattern access control)
- Messages are typically JSON but can be any text
- Supports all Redis pub/sub patterns and features

## Code Conventions

- Error handling uses log.Fatal for startup failures
- Goroutines for each database listener with proper cleanup
- Mutex protection for shared state (RelayPool)
- Configuration keys use PascalCase
- HTTP handlers return JSON errors with appropriate status codes