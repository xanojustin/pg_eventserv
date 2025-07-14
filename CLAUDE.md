# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

pg_eventserv is a PostgreSQL event server written in Go that bridges PostgreSQL's NOTIFY/LISTEN functionality with WebSocket clients for real-time applications.

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
# Set database connection
export DATABASE_URL=postgresql://username:password@host/dbname

# Run the server
./pg_eventserv

# Run with debug logging
./pg_eventserv --debug
```

### Testing
The project has no automated tests. Use the built-in web interface at http://localhost:7700/ or the example applications in `/examples/` for manual testing.

## Architecture

### Core Components

1. **main.go**: HTTP server setup, WebSocket routing, and signal handling
2. **db.go**: PostgreSQL connection pool management using pgx
3. **util.go**: URL formatting and proxy header utilities

### Request Flow

1. Client connects to WebSocket endpoint: `ws://host:7700/listen/{channel}`
2. Channel name validated against allowed patterns (glob matching)
3. Database listener goroutine created for new channels
4. PostgreSQL NOTIFY events forwarded to all WebSocket clients on that channel
5. Broadcast relay manages multiple clients per channel

### Key Dependencies

- `gorilla/mux` & `gorilla/websocket`: HTTP routing and WebSocket handling
- `jackc/pgx/v4`: PostgreSQL driver with connection pooling
- `spf13/viper`: Configuration management (TOML files, env vars, flags)
- `teivah/broadcast`: Thread-safe message broadcasting
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

### WebSocket Protocol

- Clients receive raw NOTIFY payloads
- Connection kept alive with 2-second ping/pong
- No authentication built-in (relies on channel pattern access control)
- Messages are typically JSON but can be any text

## Code Conventions

- Error handling uses log.Fatal for startup failures
- Goroutines for each database listener with proper cleanup
- Mutex protection for shared state (RelayPool)
- Configuration keys use PascalCase
- HTTP handlers return JSON errors with appropriate status codes