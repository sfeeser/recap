// --- recap-server/db/db.go ---
package db

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"recap-server/models"
)

// InitDB initializes the PostgreSQL database connection pool
func InitDB(connString string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(context.Background(), connString)
	if err != nil {
		return nil, fmt.Errorf("unable to create connection pool: %w", err)
	}

	// Ping the database to verify connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	log.Println("Successfully connected to PostgreSQL database!")
	return pool, nil
}

// CreateSchema sets up the necessary tables for RECAP.
// In a production environment, use a proper migration tool (e.g., golang-migrate).
func CreateSchema(pool *pgxpool.Pool) error {
	schemaSQL := `
	CREATE TABLE IF NOT EXISTS courses (
		id SERIAL PRIMARY KEY,
		name VARCHAR(255) NOT NULL,
		course_code VARCHAR(50) NOT NULL UNIQUE,
		duration_days INT,
		marketing_name TEXT,
		responsibility VARCHAR(255)
	);

	CREATE TABLE IF NOT EXISTS domains (
		id SERIAL PRIMARY KEY,
		course_id INT NOT NULL,
		name VARCHAR(255) NOT NULL,
		FOREIGN KEY (course_id) REFERENCES courses(id) ON DELETE CASCADE,
		UNIQUE (course_id, name) -- Ensure domain names are unique per course
	);

	CREATE TABLE IF NOT EXISTS questions (
		id SERIAL PRIMARY KEY,
		domain_id INT NOT NULL,
		question_text TEXT NOT NULL,
		explanation TEXT NOT NULL,
		question_type VARCHAR(50) NOT NULL CHECK (question_type IN ('single', 'multi', 'truefalse', 'fillblank')),
		image_url TEXT,
		code_block TEXT,
		input_method VARCHAR(50) CHECK (input_method IN ('text', 'terminal')), -- NULL implies 'text' for existing, but 'text' is better
		validity_score FLOAT DEFAULT NULL,
		flagged BOOLEAN DEFAULT FALSE,
		exam_bank_version VARCHAR(50) NOT NULL,
		FOREIGN KEY (domain_id) REFERENCES domains(id) ON DELETE CASCADE,
		UNIQUE (question_text, exam_bank_version) -- Ensure unique questions per version
	);

	CREATE TABLE IF NOT EXISTS choices (
		id SERIAL PRIMARY KEY,
		question_id INT NOT NULL,
		choice_text TEXT NOT NULL,
		is_correct BOOLEAN DEFAULT FALSE,
		explanation TEXT,
		FOREIGN KEY (question_id) REFERENCES questions(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS fill_blank_answers (
		id SERIAL PRIMARY KEY,
		question_id INT NOT NULL,
		acceptable_answer TEXT NOT NULL,
		FOREIGN KEY (question_id) REFERENCES questions(id) ON DELETE CASCADE,
		UNIQUE (question_id, acceptable_answer)
	);

	CREATE TABLE IF NOT EXISTS exams (
		id SERIAL PRIMARY KEY,
		course_id INT NOT NULL,
		title VARCHAR(255),
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		exam_bank_version VARCHAR(50) NOT NULL,
		min_questions INT NOT NULL,
		max_questions INT NOT NULL,
		exam_time INT NOT NULL,
		passing_score FLOAT NOT NULL,
		domain_weights JSONB NOT NULL, -- Store domain weights as JSONB
		FOREIGN KEY (course_id) REFERENCES courses(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS exam_questions (
		id SERIAL PRIMARY KEY,
		exam_id INT NOT NULL,
		question_id INT NOT NULL,
		question_order INT NOT NULL,
		exam_bank_version VARCHAR(50) NOT NULL, -- Redundant but useful for cross-referencing and checks
		FOREIGN KEY (exam_id) REFERENCES exams(id) ON DELETE CASCADE,
		FOREIGN KEY (question_id) REFERENCES questions(id) ON DELETE CASCADE,
		UNIQUE (exam_id, question_id), -- A question appears only once in an exam
		UNIQUE (exam_id, question_order) -- Order is unique within an exam
	);

	CREATE TABLE IF NOT EXISTS students (
		email VARCHAR(255) PRIMARY KEY
		-- FIRM handles the actual user management (e.g., account status, roles)
		-- FOREIGN KEY (email) REFERENCES users(email) ON DELETE CASCADE -- Assumes a 'users' table from FIRM
	);

	CREATE TABLE IF NOT EXISTS exam_attempts (
		id SERIAL PRIMARY KEY,
		exam_id INT NOT NULL,
		email VARCHAR(255) NOT NULL,
		started_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
		completed_at TIMESTAMP WITH TIME ZONE,
		score_percent INT,
		mode VARCHAR(50) NOT NULL CHECK (mode IN ('practice', 'simulation')),
		FOREIGN KEY (exam_id) REFERENCES exams(id) ON DELETE CASCADE,
		FOREIGN KEY (email) REFERENCES students(email) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS user_answers (
		id SERIAL PRIMARY KEY,
		attempt_id INT NOT NULL,
		exam_question_id INT NOT NULL,
		-- For MCQ, store choice_ids as an array of INT
		-- For Fill-in-the-blank, store text_answer
		choice_ids INT[],
		text_answer TEXT,
		FOREIGN KEY (attempt_id) REFERENCES exam_attempts(id) ON DELETE CASCADE,
		FOREIGN KEY (exam_question_id) REFERENCES exam_questions(id) ON DELETE CASCADE,
		UNIQUE (attempt_id, exam_question_id) -- User answers a question once per attempt
	);

	CREATE TABLE IF NOT EXISTS error_logs (
		id SERIAL PRIMARY KEY,
		timestamp TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
		source TEXT NOT NULL, -- e.g., "ingestion", "exam_generation"
		course_code VARCHAR(50),
		file_path TEXT,
		line_number INT,
		field_name TEXT,
		error_message TEXT NOT NULL,
		suggested_fix TEXT
	);

	CREATE TABLE IF NOT EXISTS admin_events (
		id SERIAL PRIMARY KEY,
		timestamp TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
		action VARCHAR(255),
		actor VARCHAR(255), -- User email or 'system'
		target TEXT,        -- e.g., course_code, question_id, user_email
		notes TEXT
	);

	CREATE TABLE IF NOT EXISTS settings (
		key VARCHAR(255) PRIMARY KEY,
		value TEXT NOT NULL,
		description TEXT,
		updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
		updated_by VARCHAR(255)
	);
	`
	_, err := pool.Exec(context.Background(), schemaSQL)
	if err != nil {
		return fmt.Errorf("error executing schema SQL: %w", err)
	}

	// Insert default settings if not already present
	defaultSettings := map[string]string{
		"rate_limit_api_per_hour":    "100",
		"rate_limit_admin_per_hour":  "50",
		"question_validity_threshold":"0.25", // Bottom 25% for low-scoring
	}

	for key, value := range defaultSettings {
		_, err := pool.Exec(context.Background(), `
			INSERT INTO settings (key, value, description)
			VALUES ($1, $2, $3)
			ON CONFLICT (key) DO NOTHING;
		`, key, value, fmt.Sprintf("Default setting for %s", key))
		if err != nil {
			log.Printf("Warning: Failed to insert default setting %s: %v", key, err)
		}
	}


	return nil
}

// LogError adds an entry to the error_logs table
func LogError(pool *pgxpool.Pool, source, courseCode, filePath string, lineNumber int, fieldName, errMsg, fixSug string) {
	_, err := pool.Exec(context.Background(), `
		INSERT INTO error_logs (source, course_code, file_path, line_number, field_name, error_message, suggested_fix)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, source, courseCode, filePath, lineNumber, fieldName, errMsg, fixSug)
	if err != nil {
		log.Printf("ERROR: Failed to log error to database: %v. Original error: %s", err, errMsg)
	}
}

// LogAdminEvent adds an entry to the admin_events table
func LogAdminEvent(pool *pgxpool.Pool, actor, action, target, notes string) {
	_, err := pool.Exec(context.Background(), `
		INSERT INTO admin_events (action, actor, target, notes)
		VALUES ($1, $2, $3, $4)
	`, action, actor, target, notes)
	if err != nil {
		log.Printf("ERROR: Failed to log admin event to database: %v. Event: %s by %s on %s", err, action, actor, target)
	}
}

// GetSetting fetches a setting value from the settings table
func GetSetting(pool *pgxpool.Pool, key string) (string, error) {
    var value string
    err := pool.QueryRow(context.Background(), "SELECT value FROM settings WHERE key = $1", key).Scan(&value)
    if err != nil {
        return "", fmt.Errorf("setting %s not found: %w", key, err)
    }
    return value, nil
}

// GetAllCourseCodes fetches all course codes from the courses table.
func GetAllCourseCodes(pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(context.Background(), "SELECT course_code FROM courses")
	if err != nil {
		return nil, fmt.Errorf("failed to query course codes: %w", err)
	}
	defer rows.Close()

	var courseCodes []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, fmt.Errorf("failed to scan course code: %w", err)
		}
		courseCodes = append(courseCodes, code)
	}
	return courseCodes, nil
}