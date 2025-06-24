#!/bin/bash

# This script drops the 'recap_db' PostgreSQL database if it exists,
# and then recreates it.
# It assumes you have 'psql', 'dropdb', and 'createdb' commands available.
#
# IMPORTANT: Ensure the PostgreSQL user running this script has privileges
# to drop and create databases (e.g., 'postgres' superuser).
#
# Configuration:
# You can set the DATABASE_URL environment variable, or modify it directly below.
# Example: export RECAP_DATABASE_URL="postgresql://recap_user:recap_password@localhost:5432/recap_db?sslmode=disable"

# --- Configuration Start ---
# Use the RECAP_DATABASE_URL environment variable if set, otherwise use a default.
# Ensure this matches your recap-server/config.yaml DATABASE_URL
DATABASE_URL="${RECAP_DATABASE_URL:-postgresql://recap_user:recap_password@localhost:5432/recap_db?sslmode=disable}"

# Extract components from DATABASE_URL for psql commands
DB_USER=$(echo "$DATABASE_URL" | sed -n 's/.*:\/\/\(.*\):.*@.*/\1/p')
DB_PASSWORD=$(echo "$DATABASE_URL" | sed -n 's/.*:\/\/[^:]*:\(.*\?)@.*/\1/p')
DB_HOST=$(echo "$DATABASE_URL" | sed -n 's/.*@\(.*\):.*/\1/p')
DB_PORT=$(echo "$DATABASE_URL" | sed -n 's/.*:\([0-9]*\)\/.*/\1/p')
DB_NAME=$(echo "$DATABASE_URL" | sed -n 's/.*\/\(.*\)?.*$/\1/p')

# Fallback for simple DB_USER/DB_PASSWORD extraction if regex fails
if [ -z "$DB_USER" ]; then DB_USER="recap_user"; fi
if [ -z "$DB_PASSWORD" ]; then DB_PASSWORD="recap_password"; fi
if [ -z "$DB_HOST" ]; then DB_HOST="localhost"; fi
if [ -z "$DB_PORT" ]; then DB_PORT="5432"; fi
if [ -z "$DB_NAME" ]; then DB_NAME="recap_db"; fi


# Set PGHOST and PGPASSWORD environment variables for psql commands
# This avoids putting password directly in command line (more secure)
export PGPASSWORD="$DB_PASSWORD"
export PGHOST="$DB_HOST"
export PGPORT="$DB_PORT"

# --- Script Logic ---

echo "Attempting to drop database: $DB_NAME on $DB_HOST:$DB_PORT for user $DB_USER"

# Drop the database if it exists
# We use || true to prevent the script from exiting if the database doesn't exist
dropdb -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" --if-exists "$DB_NAME"
if [ $? -eq 0 ]; then
    echo "Database '$DB_NAME' dropped successfully (or did not exist)."
else
    echo "ERROR: Failed to drop database '$DB_NAME'. Check permissions or database activity."
    echo "Hint: Ensure no active connections to '$DB_NAME'. You might need to use a superuser like 'postgres'."
    exit 1
fi

echo "Creating new database: $DB_NAME"

# Create the new database
createdb -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" "$DB_NAME"
if [ $? -eq 0 ]; then
    echo "Database '$DB_NAME' created successfully."
else
    echo "ERROR: Failed to create database '$DB_NAME'. Check permissions or a database with that name already exists."
    echo "Hint: Ensure the user '$DB_USER' has CREATE DATABASE privileges or the 'postgres' user is used."
    exit 1
fi

echo "Granting privileges on $DB_NAME to user $DB_USER"

# Grant all privileges on the new database to the specified user
# Connect to the newly created database to grant privileges on it.
# We explicitly connect as the database owner (usually the user who created it, $DB_USER)
# or as 'postgres' if $DB_USER doesn't have initial connection rights to a fresh db.
# For simplicity, we assume $DB_USER can connect to the freshly created DB.
psql -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d "$DB_NAME" -c "GRANT ALL PRIVILEGES ON DATABASE \"$DB_NAME\" TO \"$DB_USER\";"
if [ $? -eq 0 ]; then
    echo "Privileges granted successfully."
else
    echo "WARNING: Failed to grant privileges on database '$DB_NAME' to user '$DB_USER'."
    echo "This might indicate a permission issue. Your application might not be able to write to the database."
    # We don't exit here as the database itself was created, but it's a warning.
fi

echo "Database '$DB_NAME' is now reset and ready for use by your RECAP server."

# Unset environment variables for security
unset PGPASSWORD
unset PGHOST
unset PGPORT