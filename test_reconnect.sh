#!/bin/bash
# Test script to verify database reconnection behavior

echo "Testing pg_eventserv database reconnection..."
echo ""
echo "This script will:"
echo "1. Start pg_eventserv"
echo "2. You can manually stop/start your PostgreSQL to test reconnection"
echo "3. Check that the process continues running and reconnects"
echo ""
echo "Set your database connection:"
echo "export DATABASE_URL=postgresql://username:password@host/dbname"
echo "export ES_REDISADDR=localhost:6379"
echo ""
echo "Press Ctrl+C to stop the test"
echo ""

if [ -z "$DATABASE_URL" ]; then
    echo "ERROR: DATABASE_URL is not set"
    exit 1
fi

# Run with debug logging to see reconnection attempts
./pg_eventserv --debug