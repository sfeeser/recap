# recap
A RECAP Server

Example configuration file for RECAP Go Backend
Copy this to config.yaml and adjust values as needed.

```
# Server settings
SERVER_PORT: ":8080"
GIN_MODE: "debug" # Options: debug, release, test

# PostgreSQL Database connection string
# Format: postgresql://user:password@host:port/database_name?sslmode=disable
DATABASE_URL: "postgresql://recap_user:recap_password@localhost:5432/recap_db?sslmode=disable"

# FIRM Protocol (Authentication) settings
# This is used for JWT validation. The key MUST match the one used by your FIRM server.
FIRM:
  JWT_SIGNING_KEY: "your-super-secret-firm-jwt-key" # CHANGE THIS IN PRODUCTION!
  ISSUER: "firm.example.com" # This MUST match the 'iss' claim in JWTs issued by your FIRM server.

# GitHub repository settings
# This is the local path to your cloned 'alta3/labs' repository.
# The RECAP server will read course.yaml and exam_bank.csv from here.
GITHUB:
  LABS_REPO_PATH: "./alta3_labs"

# Ingestion interval for periodic check and re-ingestion of exam data.
# In a production setup, this would typically be triggered by GitHub webhooks.
# Valid time units: "ns", "us" (or "µs"), "ms", "s", "m", "h"
INGESTION_INTERVAL: "5m"
```
