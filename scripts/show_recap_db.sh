#!/bin/bash

# This script connects to the 'recap_db' PostgreSQL database
# and displays the first 5 rows of each table.
#
# It uses the 'recap_user' and 'recap_pass' credentials.
#
# Configuration:
# Ensure the DATABASE_URL environment variable is set or modify it directly below.
# Example: export RECAP_DATABASE_URL="postgresql://recap_user:recap_password@localhost:5432/recap_db?sslmode=disable"

# --- Configuration Start ---
# Use the RECAP_DATABASE_URL environment variable if set, otherwise use a default.
DATABASE_URL="${RECAP_DATABASE_URL:-postgresql://recap_user:recap_password@localhost:5432/recap_db?sslmode=disable}"

# Extract components from DATABASE_URL for psql commands
DB_USER=$(echo "$DATABASE_URL" | sed -n 's/.*:\/\/\(.*\):.*@.*/\1/p')
DB_PASSWORD=$(echo "$DATABASE_URL" | sed -n 's/.*:\/\/[^:]*:\(.*\?)@.*/\1/p')
DB_HOST=$(echo "$DATABASE_URL" | sed -n 's/.*@\(.*\):.*/\1/p')
DB_PORT=$(echo "$DATABASE_URL" | sed -n 's/.*:\([0-9]*\)\/.*/\1/p')
DB_NAME=$(echo "$DATABASE_URL" | sed -n 's/.*\/\(.*\)?.*$/\1/p')

# Fallback for simple DB_USER/DB_PASSWORD extraction if regex fails
if [ -z "$DB_USER" ]; then DB_USER="recap_user"; fi
if [ -z "$DB_PASSWORD" ]; then DB_PASSWORD="recap_pass"; fi
if [ -z "$DB_HOST" ]; then DB_HOST="localhost"; fi
if [ -z "$DB_PORT" ]; then DB_PORT="5432"; fi
if [ -z "$DB_NAME" ]; then DB_NAME="recap_db"; fi


# Set PGHOST and PGPASSWORD environment variables for psql commands
export PGPASSWORD="$DB_PASSWORD"
export PGHOST="$DB_HOST"
export PGPORT="$DB_PORT"

# --- Script Logic ---

echo "Connecting to database: $DB_NAME as user: $DB_USER"
echo "Fetching table list and showing up to 5 rows per table..."
echo "---------------------------------------------------------"

# Get all table names in the current database, excluding internal PostgreSQL tables
TABLES=$(psql -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d "$DB_NAME" -t -c "SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename;")

if [ -z "$TABLES" ]; then
    echo "No tables found in '$DB_NAME' or failed to connect."
    echo "Please ensure the database exists and user '$DB_USER' has access."
    unset PGPASSWORD PGHOST PGPORT
    exit 1
fi

for TABLE in $TABLES; do
    echo -e "\n--- Table: $TABLE (First 5 rows) ---"
    # Use psql -x for expanded output (one column per line) for better readability
    # for tables with many columns, or just remove -x for standard table format.
    # -c for command, -P pager=off to prevent paging output.
    # -A for unaligned output, -F for field separator (space by default)
    # --csv or -x are usually best for formatted output
    psql -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d "$DB_NAME" -P pager=off -c "SELECT * FROM \"$TABLE\" LIMIT 5;"

    if [ $? -ne 0 ]; then
        echo "WARNING: Could not retrieve data for table '$TABLE'. Check table permissions."
    fi
done

echo -e "\n---------------------------------------------------------"
echo "Database inspection complete."

# Unset environment variables for security
unset PGPASSWORD
unset PGHOST
unset PGPORT