# pg_eventserv

A [PostgreSQL](https://postgis.net/)-only event server in [Go](https://golang.org/) that bridges PostgreSQL `NOTIFY` events to Redis pub/sub channels. Listens to a single configured PostgreSQL channel and publishes all notifications to the corresponding Redis channel.

# Setup and Installation

## Download

Builds of the latest code:

* [Linux](https://postgisftw.s3.amazonaws.com/pg_eventserv_latest_linux.zip)
* [Windows](https://postgisftw.s3.amazonaws.com/pg_eventserv_latest_windows.zip)
* [MacOS](https://postgisftw.s3.amazonaws.com/pg_eventserv_latest_macos.zip)
* [Docker](https://hub.docker.com/r/pramsey/pg_eventserv)

### Source

Download the source code and build:
```
make build
```

## Basic Operation

The executable will read PostgreSQL connection information from the `DATABASE_URL` environment variable and Redis connection information from configuration or environment variables. It connects to both databases and automatically starts listening to the configured channel, bridging all NOTIFY events to Redis.

### Linux/MacOS

```sh
export DATABASE_URL=postgresql://username:password@host/dbname
export ES_REDISADDR=localhost:6379
./pg_eventserv
```

### Windows

```
SET DATABASE_URL=postgresql://username:password@host/dbname
SET ES_REDISADDR=localhost:6379
pg_eventserv.exe
```

### Client Side

Once the service is running, it automatically listens to the configured PostgreSQL channel. You can subscribe to the corresponding Redis channel using any Redis client.

#### Redis Client Example

```bash
# Using redis-cli
redis-cli subscribe people

# Using Node.js with redis package
const redis = require('redis');
const client = redis.createClient();

client.on('message', (channel, message) => {
  console.log(`Received on ${channel}: ${message}`);
});

client.subscribe('people');
```

#### Python Example

```python
import redis

r = redis.Redis(host='localhost', port=6379)
pubsub = r.pubsub()
pubsub.subscribe('people')

for message in pubsub.listen():
    if message['type'] == 'message':
        print(f"Received: {message['data']}")
```

### Channel Configuration

The server listens to a single PostgreSQL channel that can be configured through:
- Command-line flag: `--channel channelname`
- Environment variable: `ES_LISTENCHANNEL=channelname`
- Configuration file: `ListenChannel = "channelname"`
- Default value: `events`

All PostgreSQL notifications on this channel are published to the same Redis channel name. This allows you to customize the channel name to match your application's needs.

### Raising a Notification

To send a message from the database to Redis subscribers, connect to your database and run the `NOTIFY` command:

```sql
NOTIFY people, 'message to send';
```

You can also raise a notification by running the `pg_notify()` function:

```sql
SELECT pg_notify('people', 'message to send');
```

## Configuration

### Environment Variables

- `DATABASE_URL`: PostgreSQL connection string
- `ES_REDISADDR`: Redis server address (default: localhost:6379)
- `ES_REDISPASSWORD`: Redis password (if required)
- `ES_REDISDB`: Redis database number (default: 0)
- `ES_LISTENCHANNEL`: PostgreSQL channel to listen to (default: events)
- `ES_DEBUG`: Enable debug logging

### Configuration File

Create a `pg_eventserv.toml` file:

```toml
DbConnection = "postgresql://user:pass@localhost/dbname"
RedisAddr = "localhost:6379"
RedisPassword = ""
RedisDB = 0
ListenChannel = "events"
Debug = false
```

## Trouble-shooting

To get more information about what is going on behind the scenes, run with the `--debug` commandline parameter:
```sh
./pg_eventserv --debug
```

# Operation

The purpose of `pg_eventserv` is to bridge PostgreSQL events to Redis pub/sub, making database events accessible to any application that can connect to Redis. This allows you to:

- Decouple event consumers from PostgreSQL connections
- Scale event processing across multiple applications
- Use Redis pub/sub patterns and features
- Integrate with existing Redis-based infrastructure

## Architecture

```
PostgreSQL NOTIFY → pg_eventserv → Redis PUBLISH → Redis Subscribers
```

## Simple Data Notification

The most basic form of event is a change of data. Here's an example that generates a new event for every insert and update on a table:

```sql
CREATE TABLE people (
    pk serial primary key,
    ts timestamptz DEFAULT now(),
    name text,
    age integer,
    height real
);

CREATE OR REPLACE FUNCTION data_change() RETURNS trigger AS
$$
    DECLARE
        js jsonb;
    BEGIN
        SELECT to_jsonb(NEW.*) INTO js;
        js := jsonb_set(js, '{dml_action}', to_jsonb(TG_OP));
        PERFORM (
            SELECT pg_notify('people', js::text)
        );
        RETURN NEW;
    END;
$$ LANGUAGE 'plpgsql';

DROP TRIGGER IF EXISTS data_change_trigger
  ON people;
CREATE TRIGGER data_change_trigger
    BEFORE INSERT OR UPDATE ON people
    FOR EACH ROW
        EXECUTE FUNCTION data_change();
```

Install these functions, configure the server to listen to the 'people' channel, subscribe to the Redis channel, then run some data modifications:

```sql
INSERT INTO people (name, age, height) VALUES ('Paul', 51, 1.9);
INSERT INTO people (name, age, height) VALUES ('Colin', 65, 1.5);
```

## Filtered Data Notification

Send events only when specific conditions are met:

```sql
CREATE OR REPLACE FUNCTION data_change() RETURNS trigger AS
$$
    DECLARE
        js jsonb;
    BEGIN
        IF NEW.height >= 2.0
        THEN
            SELECT to_jsonb(NEW.*) INTO js;
            PERFORM (
                SELECT pg_notify('people', js::text)
            );
        END IF;
        RETURN NEW;
    END;
$$ LANGUAGE 'plpgsql';
```

Note: Ensure your trigger function uses the same channel name that pg_eventserv is configured to listen to.

Then send in some updates that pass the filter and others that don't:

```sql
UPDATE people SET name = 'Shorty', height = 1.5 WHERE age = 51;
UPDATE people SET name = 'Bozo', height = 2.1 WHERE age = 51;
INSERT INTO people (name, age, height) VALUES ('Stretch', 33, 2.8);
```