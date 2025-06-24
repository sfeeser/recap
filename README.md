## RECAP Server
The Remote Exam Content and Assessment Protocol (RECAP) Server is a backend application designed to standardize the creation, delivery, and management of practice exams. It leverages CSV files stored in GitHub repositories for exam content, ingests this content into a PostgreSQL backend, and dynamically generates exams. Authentication is handled by the First Identity Requires Mail (FIRM) protocol using JWTs.

This server provides both an API for student interaction with exams and an administrative interface for managing courses, questions, and reviewing system logs and user activity.

### Table of Contents

- Features
- Project Structure
- Prerequisites
- Setup Guide
  - Clone the Repository      Step 1
  - PostgreSQL Database Setup Step 2-7
  - Create config.yaml        Step 8
  - Prepare Exam Content Dir  Step 9
  - Initialize Go Module      Step 10 - 11


### Features
Dynamic Exam Generation: Creates unique practice exams from a pool of questions, adhering to domain weighting rules and ensuring no question reuse within a given exam bank version.

Multiple Question Types: Supports single-choice, multiple-choice (select all), and fill-in-the-blank questions (with text or terminal input options).

CSV-based Content Management: Exam questions and course metadata are defined in exam_bank.csv and course.yaml files, respectively, stored in a designated GitHub repository.

PostgreSQL Backend: Stores all exam data, user attempts, and administrative logs.

FIRM Authentication Integration: Secures API and admin access using FIRM JWTs for email-based identity.

Comprehensive Admin Interface: Provides a server-rendered web UI for managing courses, reviewing error logs, tracking user activity, and analyzing question performance.

Automated Ingestion & Validation: Periodically syncs with the GitHub repository, validates content, and regenerates exams.

Question Validity Scoring: Calculates a validity score for questions based on student performance.

### Project Structure
The RECAP server codebase is organized into the following directories (Go packages):

recap-server/
├── main.go               # Main application entry point and server setup
├── config/               # Configuration loading and structures
│   └── config.go
├── db/                   # Database connection, schema creation, and common DB operations
│   └── db.go
├── models/               # Go structs defining application data models
│   └── models.go
├── ingestion/            # Logic for parsing, validating, and ingesting CSV/YAML data
│   └── ingestion.go
├── exam/                 # Core exam generation algorithms and related logic
│   └── generator.go
├── handlers/             # HTTP API and Admin UI request handlers
│   ├── api_handlers.go
│   └── admin_handlers.go
├── middleware/           # Gin middleware for authentication, authorization, and logging
│   └── auth.go
├── utils/                # General utility functions (e.g., string manipulation, parsing)
│   └── utils.go
├── templates/            # HTML templates for the server-rendered Admin UI
│   ├── layout.html
│   └── admin_dashboard.html
├── scripts/              # Bash scripts for database setup and management
│   ├── reset_db.sh
│   ├── setup_recap_user.sh
│   └── show_recap_db.sh
└── config.yaml           # Application configuration file (create from example below)

### Prerequisites
Before you begin, ensure you have the following installed:

- Go: Version 1.20 or later.
- PostgreSQL: A running PostgreSQL server.
- Git: For cloning the alta3/labs repository (or equivalent content source).
- psql client: Command-line tool for interacting with PostgreSQL.

### Setup Guide
Follow these steps to get the RECAP server up and running on your local machine.

1. Clone the Repository - First, clone the RECAP server repository to your local machine:

  ```
  git clone https://github.com/your-repo/recap-server.git # Replace with actual repo URL
  cd recap-server

2. PostgreSQL Database Setup You'll need a PostgreSQL database named recap_db and a user named recap_user with password recap_pass that has privileges to create/drop databases. We'll use provided helper scripts for this. Edit scripts/setup_recap_user.sh and SUPERUSER_DB_USER, SUPERUSER_DB_PASSWORD, DB_HOST, and DB_PORT with your actual PostgreSQL superuser credentials and server details.

  ```
  vim scripts/setup_recap_user.sh
  ```

3. Make that file executable

  ```
  chmod +x scripts/setup_recap_user.sh
  ```

4. Run the script to creat the recap_user

  ```
  ./scripts/setup_recap_user.sh
  ```

5. Now prepare the next bash script to set up the recap databse.  This script will delete the dataabase if it is present, so you have been warned! 

  ```
  vim scripts/reset_db.sh
  ```

6. Make that file executable

  ```
  chmod +x cripts/reset_db.sh
  ```

7. Run the script to creat the recap_user

  ```
  ./scripts/reset_db.sh
  ```


8. Now edit the config.yaml file. Create a file named config.yaml in the recap-server root directory. This file will hold your application's configuration.

  ```
  vim config.yaml
  ```

  ```
  # Server settings
  SERVER_PORT: ":8080"
  GIN_MODE: "debug" # Options: debug, release, test

  # PostgreSQL Database connection string
  # Format: postgresql://user:password@host:port/database_name?sslmode=disable
  DATABASE_URL: "postgresql://recap_user:recap_pass@localhost:5432/recap_db?sslmode=disable"

  # FIRM Protocol (Authentication) settings
  # This is used for JWT validation. The key MUST match the one used by your FIRM server.
  FIRM:
    JWT_SIGNING_KEY: "your-super-secret-firm-jwt-key" # IMPORTANT: CHANGE THIS IN PRODUCTION!
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

  > Important:  
  > - Ensure DATABASE_URL matches the user (recap_user) and database (recap_db) created in Step 2.  
  > - Change FIRM.JWT_SIGNING_KEY to a strong, unique secret. This key must match the key used by your FIRM server to sign JWTs.  
  > - Adjust FIRM.ISSUER to match the issuer claim in JWTs from your FIRM server.
  > - GITHUB.LABS_REPO_PATH should point to a local directory where you will store your exam content.

9. Prepare Exam Content Directory. The RECAP server expects exam content (course metadata and questions) to be available locally in a specific structure, mimicking a GitHub repository.

  a. Create a github repo called `labs`  Inside that directory, create the following directory structure:

    ```
    mkdir -p courses/unique-directory-for-each-exam
    ```

  b. for instance, given course `AA-ANS100` 

    ```
    mkdir -p courses/AA-AMS100
    touch courses/AA-AMS100/course.yaml
    touch courses/AA-AMS100/exam_bank.csv
    ```

  c. yaml format

    ```
    marketing_name: Introduction to Ansible
    course_code: AA-ANS100
    duration_days: 3
    responsibility: your_github_username # Your GitHub username or maintainer's
    ```

  d. Example alta3_labs/courses/AA-ANS100/exam_bank.csv:

    ```
    schema_version,1.0,,,,,,,,,,,,,,,
    min_questions,10,,,,,,,,,,,,,,,
    max_questions,10,,,,,,,,,,,,,,,
    exam_time,15,,,,,,,,,,,,,,,
    passing_score,70,,,,,,,,,,,,,,,
    domains,Command Line:0.5|YAML:0.5,,,,,,,,,,,,,
    single,Which command runs a playbook?,Use ansible-playbook.,Command Line,,,,,ansible-playbook,TRUE,,ansible,FALSE,,ansible-doc,FALSE,,,,,,,,
    fillblank,What is the file extension for Ansible playbooks?,YAML files use .yaml or .yml extensions.,YAML,,text,yaml|yml,,,,,,,,,,
    fillblank,Which command displays system facts in Ansible?,ansible -m setup hostname,Command Line,,terminal,ansible -m setup hostname,,,,,,,,,,
    single,What is YAML typically used for in Ansible?,Data serialization and configuration.,YAML,,,,,Data serialization,TRUE,,Command execution,FALSE,,Scripting,FALSE,,,,,,,,
    ```

    > Note: Ensure your exam_bank.csv file has exactly 17 columns as specified by the protocol, even if some are empty (use empty placeholders ,,,,).

10. Initialize Go Module. From the recap-server root directory, initialize your Go module and download dependencies:

  ```
  go mod init recap-server
  go mod tidy
  ```

11. Compiling and Running the Server

  ```
  go run main.go
  ```

The server will start on http://localhost:8080 (or the port you configured in config.yaml). It will attempt to connect to the database, create the schema (if it doesn't exist), and then periodically trigger ingestion of exam content.

Docker (Coming Soon)
Docker images and compose files for easier deployment will be provided in a future update.

Usage and Testing
Admin UI Access
The RECAP server's admin interface is accessible via your web browser.

Navigate: Open your browser to http://localhost:8080/admin/dashboard.

Authentication: The admin UI is protected by FIRM JWTs. To access it, you need to provide a valid JWT with admin or instructor roles in your request headers.

How to get a JWT (for testing): Since the FIRM server integration is mocked for local testing, you will need to manually generate a JWT for development purposes. Use a tool like jwt.io with the following details:

Header: {"alg": "HS256", "typ": "JWT"}

Payload: {"sub": "your_admin_email@example.com", "roles": ["admin"], "iss": "firm.example.com", "exp": <future_timestamp_in_seconds>, "iat": <current_timestamp_in_seconds>, "jti": "<unique_uuid>"}

Verify Signature: Use the FIRM.JWT_SIGNING_KEY you configured in config.yaml.

Once you have the JWT, you'll typically configure your browser's developer tools or use a client like Postman/Insomnia to add an Authorization: Bearer <YOUR_JWT> header to your requests when accessing admin routes.

Triggering Ingestion
The server includes a periodic ingestion process. However, you can also manually trigger ingestion for a specific course via the admin API. This is useful during development after making changes to your course.yaml or exam_bank.csv files.

Method: POST request
URL: http://localhost:8080/admin/ingest/:course_code
Example: http://localhost:8080/admin/ingest/AA-ANS100
Headers: Authorization: Bearer <YOUR_ADMIN_JWT>

After successful ingestion, you can view logs in the /admin/error_logs section of the admin UI.

API Endpoints
You can interact with the RECAP server's public API endpoints using tools like Postman, Insomnia, or a frontend application. All API endpoints require a valid FIRM JWT (e.g., with a user role) in the Authorization: Bearer <YOUR_JWT> header.

Common API Endpoints:

- GET /api/v1/courses: List available courses.
- GET /api/v1/courses/:course_code/exams: List exams for a specific course.
- POST /api/v1/exam_sessions: Start a new exam session.
- POST /api/v1/exam_sessions/:session_id/answer: Record an answer for a question.
- GET /api/v1/exam_sessions/:session_id/status: Check exam progress.
- POST /api/v1/exam_sessions/:session_id/submit: Finalize an exam session and get results.
- GET /api/v1/students/:email/history: View a student's past exam attempts.

Refer to the RECAP Protocol Specification for detailed request/response examples.

Database Inspection - To quickly inspect the contents of your recap_db and verify data during development, you can use the show_recap_db.sh script.

```
chmod +x scripts/show_recap_db.sh

./scripts/show_recap_db.sh
```

Important: This script uses the DATABASE_URL specified in your config.yaml. It will display the first 5 rows of each table in the recap_db.
