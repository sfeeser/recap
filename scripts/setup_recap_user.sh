#!/bin/bash

# This script sets up a PostgreSQL user ('recap_user') with a specified password
# and grants them the necessary privileges to create, modify, and drop databases.
#
# It connects using a PostgreSQL superuser (e.g., 'postgres').
#
# IMPORTANT:
# - Ensure the superuser ('roadmaster' in this case) has privileges to create roles.
# - Replace placeholder values with your actual superuser host/port/password.

# --- Configuration Start ---
SUPERUSER_DB_USER="roadmaster"         # Example, PLEASE use your own
SUPERUSER_DB_PASSWORD="roadmaster-4d"  # Superuser's password
DB_HOST="localhost"                    # Your PostgreSQL host
DB_PORT="5432"                         # Your PostgreSQL port

RECAP_DB_USER="recap_user"
RECAP_DB_PASSWORD="recap_pass"         # USE YOUR OWN Password for the new recap_user

# --- Script Logic ---

echo "Connecting to PostgreSQL as superuser: $SUPERUSER_DB_USER@$DB_HOST:$DB_PORT"

# Export PGPASSWORD for the superuser for security (avoids password in command line)
export PGPASSWORD="$SUPERUSER_DB_PASSWORD"
export PGHOST="$DB_HOST"
export PGPORT="$DB_PORT"

# Check if recap_user already exists and drop it if requested, or just proceed
# For simplicity, this script will attempt to create/update the user.
# If you want to explicitly drop and recreate, uncomment the block below.
#
# echo "Checking if user '$RECAP_DB_USER' exists..."
# EXISTS_USER=$(psql -U "$SUPERUSER_DB_USER" -tAc "SELECT 1 FROM pg_roles WHERE rolname='$RECAP_DB_USER'")
# if [ "$EXISTS_USER" = "1" ]; then
#     echo "User '$RECAP_DB_USER' already exists. Attempting to drop and recreate..."
#     psql -U "$SUPERUSER_DB_USER" -c "DROP ROLE IF EXISTS \"$RECAP_DB_USER\";"
#     if [ $? -ne 0 ]; then
#         echo "ERROR: Failed to drop existing user '$RECAP_DB_USER'. Check permissions or active connections."
#         unset PGPASSWORD PGHOST PGPORT
#         exit 1
#     fi
# fi

echo "Creating or updating user '$RECAP_DB_USER' with CREATEDB and LOGIN privileges..."

# Create the user with password, CREATEDB, and LOGIN privileges
# If the user already exists, ALTER ROLE will update the password and privileges.
psql -U "$SUPERUSER_DB_USER" -c "
    CREATE ROLE \"$RECAP_DB_USER\" WITH LOGIN PASSWORD '$RECAP_DB_PASSWORD' CREATEDB;
    ALTER ROLE \"$RECAP_DB_USER\" WITH PASSWORD '$RECAP_DB_PASSWORD';
    GRANT CREATEDB ON ROLE \"$RECAP_DB_USER\" TO \"$RECAP_DB_USER\";
"

if [ $? -eq 0 ]; then
    echo "User '$RECAP_DB_USER' created/updated successfully with CREATEDB and LOGIN privileges."
    echo "This user can now create, drop, and manage their own databases (e.g., 'recap_db')."
else
    echo "ERROR: Failed to create or update user '$RECAP_DB_USER'."
    echo "Please check the superuser's privileges and PostgreSQL server status."
    unset PGPASSWORD PGHOST PGPORT
    exit 1
fi

echo "PostgreSQL user setup complete."

# Unset environment variables for security
unset PGPASSWORD
unset PGHOST
unset PGPORT