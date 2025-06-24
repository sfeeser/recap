// --- recap-server/main.go ---
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-contrib/multitemplate"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/viper"

	"recap-server/config"
	"recap-server/db"
	"recap-server/handlers"
	"recap-server/ingestion"
	"recap-server/middleware"
	"recap-server/exam" // Import the exam package for generator logic
)

func main() {
	// Load configuration
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Error loading configuration: %v", err)
	}

	// Initialize database connection pool
	pool, err := db.InitDB(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Unable to connect to database: %v", err)
	}
	defer pool.Close()

	// Ensure database schema is set up (simple creation for demo)
	if err := db.CreateSchema(pool); err != nil {
		log.Fatalf("Error creating database schema: %v", err)
	}

	// Set Gin mode
	gin.SetMode(cfg.GinMode)

	// Initialize Gin router
	router := gin.Default()

	// Load HTML templates for admin UI
	renderer := multitemplate.NewRenderer()
	renderer.AddFromFiles("admin_layout", "templates/layout.html")
	renderer.AddFromFiles("admin_dashboard", "templates/admin_dashboard.html")
	// Add other admin templates here as they are created
	router.HTMLRender = renderer

	// Middleware
	router.Use(middleware.Logger()) // Custom logger middleware
	// FIRM JWT authentication middleware for API and Admin routes
	authMiddleware := middleware.AuthMiddleware(cfg.FIRM.JWTSigningKey, cfg.FIRM.Issuer)

	// API Routes (version 1)
	apiV1 := router.Group("/api/v1")
	apiV1.Use(authMiddleware) // Apply auth to all API routes
	{
		apiV1.GET("/courses", handlers.GetCourses(pool))
		apiV1.GET("/courses/:course_code/exams", handlers.GetExamsForCourse(pool))
		apiV1.POST("/exam_sessions", handlers.StartExamSession(pool))
		apiV1.POST("/exam_sessions/:session_id/answer", handlers.RecordAnswer(pool))
		apiV1.GET("/exam_sessions/:session_id/status", handlers.GetExamSessionStatus(pool))
		apiV1.POST("/exam_sessions/:session_id/submit", handlers.SubmitExamSession(pool))
		apiV1.GET("/students/:email/history", handlers.GetStudentHistory(pool))
	}

	// Admin UI Routes
	admin := router.Group("/admin")
	admin.Use(authMiddleware) // Apply auth to all admin routes
	admin.Use(middleware.RoleCheckMiddleware([]string{"admin", "instructor"})) // Role-based access control for admin routes
	{
		admin.GET("/dashboard", handlers.AdminDashboard(pool))
		// Admin CRUD routes for courses
		admin.GET("/courses", handlers.AdminListCourses(pool))
		admin.POST("/courses", handlers.AdminCreateCourse(pool))
		admin.PUT("/courses/:course_code", handlers.AdminUpdateCourse(pool))
		admin.DELETE("/courses/:course_code", handlers.AdminDeleteCourse(pool))

		admin.GET("/error_logs", handlers.AdminErrorLogs(pool))
		admin.GET("/user_activity", handlers.AdminUserActivity(pool))
		admin.GET("/question_stats", handlers.AdminQuestionStats(pool))
		admin.GET("/settings", handlers.AdminSettings(pool))
		admin.POST("/settings", handlers.AdminUpdateSettings(pool)) // Placeholder for updating settings

		// Admin trigger for CSV ingestion
		admin.POST("/ingest/:course_code", handlers.TriggerIngestion(pool, cfg.GitHub.LabsRepoPath))
	}

	// Start background ingestion/exam generation service
	go func() {
		// This is a simplified periodic check. In a real system, you'd use webhooks from GitHub
		// or a more sophisticated change detection mechanism.
		ticker := time.NewTicker(cfg.IngestionInterval) // e.g., 5 minutes
		defer ticker.Stop()
		for range ticker.C {
			log.Println("Running scheduled ingestion and exam regeneration...")
			// Ingest all courses defined in the system
			courseCodes, err := db.GetAllCourseCodes(pool)
			if err != nil {
				log.Printf("Error getting course codes for scheduled ingestion: %v", err)
				continue
			}
			for _, courseCode := range courseCodes {
				log.Printf("Ingesting and regenerating exams for course: %s", courseCode)
				err := ingestion.ProcessCourseData(pool, courseCode, cfg.GitHub.LabsRepoPath)
				if err != nil {
					log.Printf("Error during scheduled ingestion for %s: %v", courseCode, err)
					// Log to admin_events table as well
					db.LogAdminEvent(pool, "system", "ingestion_failed", courseCode, fmt.Sprintf("Error: %v", err))
				} else {
					log.Printf("Successfully ingested and regenerated exams for %s", courseCode)
					db.LogAdminEvent(pool, "system", "ingestion_success", courseCode, "Ingestion and exam regeneration completed.")
				}
			}
		}
	}()

	// Start background job for validity score calculation
	go func() {
		ticker := time.NewTicker(24 * time.Hour) // Daily job
		defer ticker.Stop()
		for range ticker.C {
			log.Println("Running daily validity score calculation...")
			if err := exam.UpdateQuestionValidityScores(pool); err != nil {
				log.Printf("Error updating validity scores: %v", err)
				db.LogAdminEvent(pool, "system", "validity_score_update_failed", "all_questions", fmt.Sprintf("Error: %v", err))
			} else {
				log.Println("Successfully updated validity scores.")
				db.LogAdminEvent(pool, "system", "validity_score_update_success", "all_questions", "Validity scores updated.")
			}
		}
	}()


	// Start the server
	srv := &http.Server{
		Addr:    cfg.ServerPort,
		Handler: router,
	}

	// Goroutine to gracefully shut down the server
	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit
		log.Println("Shutting down server...")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := srv.Shutdown(ctx); err != nil {
			log.Fatalf("Server forced to shutdown: %v", err)
		}
	}()

	log.Printf("RECAP Server starting on %s", cfg.ServerPort)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server startup error: %v", err)
	}

	log.Println("Server exited gracefully.")
}


// --- recap-server/config/config.go ---
package config

import (
	"log"
	"time"

	"github.com/spf13/viper"
)

// Config holds all application configuration
type Config struct {
	ServerPort        string        `mapstructure:"SERVER_PORT"`
	GinMode           string        `mapstructure:"GIN_MODE"`
	DatabaseURL       string        `mapstructure:"DATABASE_URL"`
	FIRM              FIRMConfig    `mapstructure:"FIRM"`
	GitHub            GitHubConfig  `mapstructure:"GITHUB"`
	IngestionInterval time.Duration `mapstructure:"INGESTION_INTERVAL"`
}

// FIRMConfig holds FIRM protocol-related configuration
type FIRMConfig struct {
	JWTSigningKey string `mapstructure:"JWT_SIGNING_KEY"`
	Issuer        string `mapstructure:"ISSUER"`
	// In a real scenario, you might also have FIRM API endpoints here
	// FIRMAPIURL string `mapstructure:"FIRM_API_URL"`
}

// GitHubConfig holds GitHub-related configuration
type GitHubConfig struct {
	LabsRepoPath string `mapstructure:"LABS_REPO_PATH"` // Local path to the cloned alta3/labs repo
}

// LoadConfig loads configuration from environment variables and config.yaml
func LoadConfig() (*Config, error) {
	viper.SetConfigName("config") // config.yaml
	viper.SetConfigType("yaml")   // yaml
	viper.AddConfigPath(".")      // Search for config in current directory

	// Set defaults
	viper.SetDefault("SERVER_PORT", ":8080")
	viper.SetDefault("GIN_MODE", "debug") // gin.DebugMode, gin.ReleaseMode, gin.TestMode
	viper.SetDefault("DATABASE_URL", "postgresql://user:password@localhost:5432/recap_db")
	viper.SetDefault("FIRM.JWT_SIGNING_KEY", "your-super-secret-firm-jwt-key") // IMPORTANT: Change this in production
	viper.SetDefault("FIRM.ISSUER", "firm.example.com")
	viper.SetDefault("GITHUB.LABS_REPO_PATH", "./alta3_labs") // Default path for cloned repo
	viper.SetDefault("INGESTION_INTERVAL", "5m")              // Default every 5 minutes

	// Read from config file
	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			log.Println("config.yaml not found, using environment variables and defaults")
		} else {
			return nil, fmt.Errorf("fatal error config file: %w", err)
		}
	}

	// Override with environment variables (e.g., RECAP_SERVER_PORT)
	viper.SetEnvPrefix("RECAP") // Look for RECAP_SERVER_PORT, RECAP_DATABASE_URL etc.
	viper.AutomaticEnv()

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unable to decode into struct: %w", err)
	}

	return &cfg, nil
}


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


// --- recap-server/models/models.go ---
package models

import (
	"encoding/json"
	"time"
)

// Course struct represents a course
type Course struct {
	ID            int        `json:"id"`
	Name          string     `json:"name"`
	CourseCode    string     `json:"course_code"`
	DurationDays  int        `json:"duration_days"`
	MarketingName string     `json:"marketing_name"`
	Responsibility string    `json:"responsibility"`
	ExamCount     int        `json:"exam_count,omitempty"` // For API response
}

// Domain struct represents a topic domain within a course
type Domain struct {
	ID       int    `json:"id"`
	CourseID int    `json:"course_id"`
	Name     string `json:"name"`
}

// Question struct represents a question
type Question struct {
	ID              int     `json:"id"`
	DomainID        int     `json:"domain_id"`
	QuestionText    string  `json:"question_text"`
	Explanation     string  `json:"explanation"`
	QuestionType    string  `json:"question_type"`
	ImageURL        *string `json:"image_url"` // Pointer to allow NULL
	CodeBlock       *string `json:"code_block"`
	InputMethod     *string `json:"input_method"` // For fillblank
	ValidityScore   *float64 `json:"validity_score"`
	Flagged         bool    `json:"flagged"`
	ExamBankVersion string  `json:"exam_bank_version"`
	// For API responses, might also contain choices/acceptable answers
	Choices          []Choice `json:"choices,omitempty"`
	AcceptableAnswers []string `json:"acceptable_answers,omitempty"`
}

// Choice struct represents an answer choice for MCQ
type Choice struct {
	ID          int    `json:"choice_id"`
	QuestionID  int    `json:"question_id"`
	ChoiceText  string `json:"text"`
	IsCorrect   bool   `json:"is_correct"`
	Explanation string `json:"explanation"`
	Order       string `json:"order"` // 'A', 'B', 'C' for frontend
}

// FillBlankAnswer struct represents an acceptable answer for fill-in-the-blank
type FillBlankAnswer struct {
	ID             int    `json:"id"`
	QuestionID     int    `json:"question_id"`
	AcceptableAnswer string `json:"acceptable_answer"`
}

// Exam struct represents a generated exam
type Exam struct {
	ID              int                  `json:"exam_id"`
	CourseID        int                  `json:"course_id"`
	Title           string               `json:"title"`
	CreatedAt       time.Time            `json:"created_at"`
	ExamBankVersion string               `json:"exam_bank_version"`
	MinQuestions    int                  `json:"min_questions"`
	MaxQuestions    int                  `json:"max_questions"`
	ExamTime        int                  `json:"exam_time"` // in minutes
	PassingScore    float64              `json:"passing_score"`
	DomainWeights   map[string]float64 `json:"domain_weights"`
}

// ExamQuestion struct links a question to an exam and its order
type ExamQuestion struct {
	ID              int    `json:"exam_question_id"`
	ExamID          int    `json:"exam_id"`
	QuestionID      int    `json:"question_id"`
	QuestionOrder   int    `json:"question_order"`
	ExamBankVersion string `json:"exam_bank_version"`
}

// ExamAttempt struct represents a student's attempt at an exam
type ExamAttempt struct {
	ID          int        `json:"id"`
	ExamID      int        `json:"exam_id"`
	Email       string     `json:"email"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"` // Pointer to allow NULL
	ScorePercent *int      `json:"score_percent"` // Pointer to allow NULL
	Mode        string     `json:"mode"`
}

// UserAnswer struct represents a student's answer to a specific exam question
type UserAnswer struct {
	ID             int    `json:"id"`
	AttemptID      int    `json:"attempt_id"`
	ExamQuestionID int    `json:"exam_question_id"`
	ChoiceIDs      []int  `json:"choice_ids"`  // For MCQ
	TextAnswer     *string `json:"text_answer"` // For fill-in-the-blank
}

// ExamSessionRequest for starting an exam
type ExamSessionRequest struct {
	ExamID int    `json:"exam_id" binding:"required"`
	Mode   string `json:"mode" binding:"required,oneof=practice simulation"`
}

// ExamSessionResponse for starting an exam
type ExamSessionResponse struct {
	SessionID        string     `json:"session_id"` // This is the exam_attempt.id as a string
	ExamTitle        string     `json:"exam_title"`
	Mode             string     `json:"mode"`
	TimeLimitMinutes int        `json:"time_limit_minutes"`
	Questions        []Question `json:"questions"` // Questions for the session (abridged)
}

// AnswerRequest for submitting an answer
type AnswerRequest struct {
	ExamQuestionID int   `json:"exam_question_id" binding:"required"`
	ChoiceIDs      []int `json:"choice_ids"`   // For single/multi-choice
	CommandText    string `json:"command_text"` // For fill-in-the-blank (maps to text_answer)
}

// AnswerResponse for practice mode feedback
type AnswerResponse struct {
	Correct        bool         `json:"correct"`
	Explanation    string       `json:"explanation"`
	Hint           *string      `json:"hint,omitempty"` // For fuzzy logic in fillblank
	ChoiceFeedback []ChoiceFeedback `json:"choice_feedback,omitempty"`
}

// ChoiceFeedback provides per-choice explanation in practice mode
type ChoiceFeedback struct {
	ChoiceID    int    `json:"choice_id"`
	IsCorrect   bool   `json:"is_correct"`
	Explanation string `json:"explanation"`
}

// ExamStatusResponse for checking progress
type ExamStatusResponse struct {
	Completed      bool   `json:"completed"`
	AnsweredCount  int    `json:"answered_count"`
	RemainingCount int    `json:"remaining_count"`
	TimeRemaining  string `json:"time_remaining"` // Formatted as "HH:MM:SS"
}

// ExamSubmissionResponse for finalizing the session
type ExamSubmissionResponse struct {
	ScorePercent   int                  `json:"score_percent"`
	Pass           bool                 `json:"pass"`
	DomainBreakdown map[string]int     `json:"domain_breakdown"`
	DetailedReport []DetailedQuestionReport `json:"detailed_report"`
}

// DetailedQuestionReport provides per-question results
type DetailedQuestionReport struct {
	Question       string   `json:"question"`
	YourAnswer     []string `json:"your_answer"` // Text representation of chosen choices or fill-in-blank
	CorrectAnswer  []string `json:"correct_answer"` // Text representation
	Result         string   `json:"result"` // "correct", "incorrect", "skipped"
	Explanation    string   `json:"explanation"`
}

// StudentHistoryEntry represents a past exam attempt for a student
type StudentHistoryEntry struct {
	ExamTitle      string           `json:"exam_title"`
	ScorePercent   int              `json:"score_percent"`
	Timestamp      time.Time        `json:"timestamp"`
	DomainBreakdown map[string]int `json:"domain_breakdown"`
}

// AdminCourseCreateRequest for admin UI
type AdminCourseCreateRequest struct {
	Name           string `form:"name" binding:"required"`
	CourseCode     string `form:"course_code" binding:"required"`
	DurationDays   int    `form:"duration_days" binding:"required"`
	MarketingName  string `form:"marketing_name" binding:"required"`
	Responsibility string `form:"responsibility"`
}

// ErrorLog represents an entry in the error_logs table
type ErrorLog struct {
	ID          int       `json:"id"`
	Timestamp   time.Time `json:"timestamp"`
	Source      string    `json:"source"`
	CourseCode  string    `json:"course_code"`
	FilePath    *string   `json:"file_path"`
	LineNumber  *int      `json:"line_number"`
	FieldName   *string   `json:"field_name"`
	ErrorMessage string   `json:"error_message"`
	SuggestedFix *string  `json:"suggested_fix"`
}

// AdminEvent represents an entry in the admin_events table
type AdminEvent struct {
	ID        int       `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Action    string    `json:"action"`
	Actor     string    `json:"actor"`
	Target    string    `json:"target"`
	Notes     string    `json:"notes"`
}

// QuestionStats for admin question_stats page
type QuestionStats struct {
	QuestionID    int       `json:"question_id"`
	QuestionText  string    `json:"question_text"`
	QuestionType  string    `json:"question_type"`
	Domain        string    `json:"domain"`
	ValidityScore *float64  `json:"validity_score"`
	Flagged       bool      `json:"flagged"`
	TimesAttempted int      `json:"times_attempted"`
	CorrectCount  int       `json:"correct_count"`
}

// Setting represents an entry in the settings table
type Setting struct {
	Key         string    `json:"key"`
	Value       string    `json:"value"`
	Description string    `json:"description"`
	UpdatedAt   time.Time `json:"updated_at"`
	UpdatedBy   string    `json:"updated_by"`
}

// CourseYAML for parsing course.yaml
type CourseYAML struct {
	MarketingName string `yaml:"marketing_name"`
	CourseCode    string `yaml:"course_code"`
	DurationDays  int    `yaml:"duration_days"`
	Responsibility string `yaml:"responsibility"`
}

// ExamBankMetadata for parsing exam_bank.csv metadata rows
type ExamBankMetadata struct {
	SchemaVersion string             `csv:"schema_version"`
	MinQuestions  int                `csv:"min_questions"`
	MaxQuestions  int                `csv:"max_questions"`
	ExamTime      int                `csv:"exam_time"`
	PassingScore  float64            `csv:"passing_score"`
	Domains       map[string]float64 `csv:"domains"` // Will be parsed from string
}

// ExamBankQuestion for parsing exam_bank.csv question rows
type ExamBankQuestion struct {
	QuestionType    string `csv:"question_type"`
	Domain          string `csv:"domain"`
	QuestionText    string `csv:"question_text"`
	Explanation     string `csv:"explanation"`
	ImageURL        string `csv:"image_url"`
	CodeBlock       string `csv:"code_block"`
	InputMethod     string `csv:"input_method"` // For fillblank
	Choice1         string `csv:"choice_1"`
	Correct1        string `csv:"correct_1"`
	Explain1        string `csv:"explain_1"`
	Choice2         string `csv:"choice_2"`
	Correct2        string `csv:"correct_2"`
	Explain2        string `csv:"explain_2"`
	Choice3         string `csv:"choice_3"`
	Correct3        string `csv:"correct_3"`
	Explain3        string `csv:"explain_3"`
	Choice4         string `csv:"choice_4"`
	Correct4        string `csv:"correct_4"`
	Explain4        string `csv:"explain_4"`
	Choice5         string `csv:"choice_5"`
	Correct5        string `csv:"correct_5"`
	Explain5        string `csv:"explain_5"`
	Choice6         string `csv:"choice_6"`
	Correct6        string `csv:"correct_6"`
	Explain6        string `csv:"explain_6"`
	AcceptableAnswers string `csv:"acceptable_answers"` // Pipe-separated for fillblank
}


// --- recap-server/ingestion/ingestion.go ---
package ingestion

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"

	"recap-server/db"
	"recap-server/exam"
	"recap-server/models"
	"recap-server/utils"
)

const (
	csvColumnCount = 17 // Fixed number of columns as per spec
	sourceName     = "ingestion"
)

// ProcessCourseData reads course.yaml and exam_bank.csv, validates, and ingests data
func ProcessCourseData(pool *pgxpool.Pool, courseCode, labsRepoPath string) error {
	coursePath := filepath.Join(labsRepoPath, "courses", courseCode)
	courseYAMLPath := filepath.Join(coursePath, "course.yaml")
	examBankCSVPath := filepath.Join(coursePath, "exam_bank.csv")

	// 1. Read course.yaml
	courseYAMLData, err := os.ReadFile(courseYAMLPath)
	if err != nil {
		db.LogError(pool, sourceName, courseCode, courseYAMLPath, 0, "", "Failed to read course.yaml", fmt.Sprintf("Ensure file exists and is readable: %v", err))
		return fmt.Errorf("failed to read course.yaml for %s: %w", courseCode, err)
	}

	var courseMeta models.CourseYAML
	if err := yaml.Unmarshal(courseYAMLData, &courseMeta); err != nil {
		db.LogError(pool, sourceName, courseCode, courseYAMLPath, 0, "", "Failed to parse course.yaml", fmt.Sprintf("Ensure YAML format is correct: %v", err))
		return fmt.Errorf("failed to unmarshal course.yaml for %s: %w", courseCode, err)
	}

	// Validate course_code matches directory
	if courseMeta.CourseCode != courseCode {
		db.LogError(pool, sourceName, courseCode, courseYAMLPath, 0, "course_code", "Mismatch between course.yaml and directory name", fmt.Sprintf("course_code in YAML (%s) must match directory name (%s)", courseMeta.CourseCode, courseCode))
		return fmt.Errorf("course code mismatch in course.yaml for %s", courseCode)
	}

	// Upsert Course into DB
	var courseID int
	err = pool.QueryRow(context.Background(), `
		INSERT INTO courses (name, course_code, duration_days, marketing_name, responsibility)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (course_code) DO UPDATE SET
			name = EXCLUDED.name,
			duration_days = EXCLUDED.duration_days,
			marketing_name = EXCLUDED.marketing_name,
			responsibility = EXCLUDED.responsibility
		RETURNING id
	`, courseMeta.MarketingName, courseMeta.CourseCode, courseMeta.DurationDays, courseMeta.MarketingName, courseMeta.Responsibility).Scan(&courseID)
	if err != nil {
		db.LogError(pool, sourceName, courseCode, "", 0, "", "Failed to upsert course data", fmt.Sprintf("Database error: %v", err))
		return fmt.Errorf("failed to upsert course %s: %w", courseCode, err)
	}

	// 2. Read exam_bank.csv
	csvFile, err := os.Open(examBankCSVPath)
	if err != nil {
		db.LogError(pool, sourceName, courseCode, examBankCSVPath, 0, "", "Failed to open exam_bank.csv", fmt.Sprintf("Ensure file exists and is readable: %v", err))
		return fmt.Errorf("failed to open exam_bank.csv for %s: %w", courseCode, err)
	}
	defer csvFile.Close()

	reader := csv.NewReader(csvFile)
	rows, err := reader.ReadAll()
	if err != nil {
		db.LogError(pool, sourceName, courseCode, examBankCSVPath, 0, "", "Failed to read exam_bank.csv", fmt.Sprintf("Ensure CSV format is correct: %v", err))
		return fmt.Errorf("failed to read all CSV rows for %s: %w", courseCode, err)
	}

	if len(rows) < 6 { // At least 5 metadata rows + 1 question row
		db.LogError(pool, sourceName, courseCode, examBankCSVPath, 0, "", "Insufficient rows in exam_bank.csv", "Minimum 5 metadata rows and at least one question row required.")
		return fmt.Errorf("insufficient rows in exam_bank.csv for %s", courseCode)
	}

	// Process metadata and questions in a transaction
	tx, err := pool.Begin(context.Background())
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(context.Background()) // Rollback on error

	// Clear existing questions and exams for this course to prepare for fresh ingestion
	// This ensures "no question reuse" enforcement works correctly when the exam bank updates.
	_, err = tx.Exec(context.Background(), `
		DELETE FROM exam_questions WHERE exam_id IN (SELECT id FROM exams WHERE course_id = $1);
		DELETE FROM exams WHERE course_id = $1;
		DELETE FROM questions WHERE domain_id IN (SELECT id FROM domains WHERE course_id = $1);
		DELETE FROM domains WHERE course_id = $1;
	`, courseID)
	if err != nil {
		db.LogError(pool, sourceName, courseCode, "", 0, "", "Failed to clear existing exam data", fmt.Sprintf("Database error during pre-ingestion cleanup: %v", err))
		return fmt.Errorf("failed to clear existing exam data for %s: %w", courseCode, err)
	}

	var (
		metadata        models.ExamBankMetadata
		questionsToSave []models.Question // To collect questions for bulk insert/validation
		domainMap       = make(map[string]int) // domain name -> domain ID
		examBankVersion = "1.0.0" // Default version
		questionTexts   = make(map[string]bool) // To check for duplicate question_text within this version
		lineOffset      = 0 // For header and metadata rows
	)

	// Process metadata rows first
	for i := 0; i < len(rows); i++ {
		row := rows[i]
		if len(row) != csvColumnCount {
			db.LogError(pool, sourceName, courseCode, examBankCSVPath, i+1, "", "Incorrect column count", fmt.Sprintf("Expected %d columns, got %d", csvColumnCount, len(row)))
			return fmt.Errorf("incorrect column count in exam_bank.csv at line %d for %s", i+1, courseCode)
		}

		firstCol := strings.TrimSpace(row[0])
		secondCol := strings.TrimSpace(row[1])

		if !isMetadataRow(firstCol) {
			lineOffset = i // Found first question row, all preceding are metadata
			break
		}

		switch firstCol {
		case "schema_version":
			if secondCol != "" {
				examBankVersion = secondCol
			} else {
				db.LogError(pool, sourceName, courseCode, examBankCSVPath, i+1, "schema_version", "Missing schema_version value", "Defaulting to 1.0.0. Provide a version like '1.0.0'")
			}
			metadata.SchemaVersion = examBankVersion
		case "min_questions":
			val, err := strconv.Atoi(secondCol)
			if err != nil || val <= 0 {
				db.LogError(pool, sourceName, courseCode, examBankCSVPath, i+1, "min_questions", "Invalid value", "Must be a positive integer.")
				return fmt.Errorf("invalid min_questions at line %d for %s", i+1, courseCode)
			}
			metadata.MinQuestions = val
		case "max_questions":
			val, err := strconv.Atoi(secondCol)
			if err != nil || val <= 0 {
				db.LogError(pool, sourceName, courseCode, examBankCSVPath, i+1, "max_questions", "Invalid value", "Must be a positive integer.")
				return fmt.Errorf("invalid max_questions at line %d for %s", i+1, courseCode)
			}
			metadata.MaxQuestions = val
		case "exam_time":
			val, err := strconv.Atoi(secondCol)
			if err != nil || val <= 0 {
				db.LogError(pool, sourceName, courseCode, examBankCSVPath, i+1, "exam_time", "Invalid value", "Must be a positive integer (minutes).")
				return fmt.Errorf("invalid exam_time at line %d for %s", i+1, courseCode)
			}
			metadata.ExamTime = val
		case "passing_score":
			val, err := strconv.ParseFloat(secondCol, 64)
			if err != nil || val < 0 || val > 100 {
				db.LogError(pool, sourceName, courseCode, examBankCSVPath, i+1, "passing_score", "Invalid value", "Must be a float between 0 and 100.")
				return fmt.Errorf("invalid passing_score at line %d for %s", i+1, courseCode)
			}
			metadata.PassingScore = val
		case "domains":
			parsedDomains, err := utils.ParseDomainWeights(secondCol)
			if err != nil {
				db.LogError(pool, sourceName, courseCode, examBankCSVPath, i+1, "domains", "Invalid domain format or weights", fmt.Sprintf("Format: 'Name:Weight|Name:Weight'. Weights must sum to 1.0. Error: %v", err))
				return fmt.Errorf("invalid domains at line %d for %s: %w", i+1, courseCode, err)
			}
			metadata.Domains = parsedDomains

			// Insert domains into DB
			for domainName := range parsedDomains {
				var id int
				err := tx.QueryRow(context.Background(), `
					INSERT INTO domains (course_id, name) VALUES ($1, $2)
					ON CONFLICT (course_id, name) DO UPDATE SET name = EXCLUDED.name
					RETURNING id
				`, courseID, domainName).Scan(&id)
				if err != nil {
					db.LogError(pool, sourceName, courseCode, examBankCSVPath, i+1, "domain_db_insert", "Failed to insert domain", fmt.Sprintf("Database error: %v", err))
					return fmt.Errorf("failed to upsert domain %s for %s: %w", domainName, courseCode, err)
				}
				domainMap[domainName] = id
			}
		default:
			// If not a recognized metadata row, it must be the start of questions.
			// This break will leave lineOffset at the current row index.
			lineOffset = i
			break
		}
	}

	if metadata.MinQuestions == 0 || metadata.MaxQuestions == 0 || metadata.ExamTime == 0 || metadata.PassingScore == 0 || metadata.Domains == nil {
		db.LogError(pool, sourceName, courseCode, examBankCSVPath, 0, "", "Missing critical exam metadata", "Ensure min_questions, max_questions, exam_time, passing_score, and domains are defined.")
		return fmt.Errorf("missing critical exam metadata for %s", courseCode)
	}

	// Process question rows
	for i := lineOffset; i < len(rows); i++ {
		row := rows[i]
		lineNum := i + 1 // CSV line number

		// Parse into ExamBankQuestion struct for easier access
		csvHeaders := []string{
			"question_type", "domain", "question_text", "explanation", "image_url", "code_block", "input_method",
			"choice_1", "correct_1", "explain_1",
			"choice_2", "correct_2", "explain_2",
			"choice_3", "correct_3", "explain_3",
			"choice_4", "correct_4", "explain_4",
			"choice_5", "correct_5", "explain_5",
			"choice_6", "correct_6", "explain_6",
			"acceptable_answers",
		}
		// Create a map from header to value
		rowMap := make(map[string]string)
		for j, header := range csvHeaders {
			if j < len(row) {
				rowMap[header] = strings.TrimSpace(row[j])
			}
		}

		qType := rowMap["question_type"]
		qText := rowMap["question_text"]
		explanation := rowMap["explanation"]
		domainName := rowMap["domain"]
		imageURL := utils.StringPtr(rowMap["image_url"])
		codeBlock := utils.StringPtr(rowMap["code_block"])
		inputMethod := utils.StringPtr(rowMap["input_method"])
		acceptableAnswers := rowMap["acceptable_answers"]

		// Basic validation for required fields
		if qText == "" || explanation == "" || domainName == "" {
			db.LogError(pool, sourceName, courseCode, examBankCSVPath, lineNum, "", "Missing required field", "question_text, explanation, and domain are required for all question types.")
			return fmt.Errorf("missing required field at line %d for %s", lineNum, courseCode)
		}

		if questionTexts[qText] {
			db.LogError(pool, sourceName, courseCode, examBankCSVPath, lineNum, "question_text", "Duplicate question text", "Question text must be unique within an exam bank version.")
			return fmt.Errorf("duplicate question text at line %d for %s: %s", lineNum, courseCode, qText)
		}
		questionTexts[qText] = true

		domainID, ok := domainMap[domainName]
		if !ok {
			db.LogError(pool, sourceName, courseCode, examBankCSVPath, lineNum, "domain", "Domain not defined in metadata", fmt.Sprintf("Domain '%s' must be specified in the 'domains' metadata row.", domainName))
			return fmt.Errorf("invalid domain '%s' at line %d for %s", domainName, lineNum, courseCode)
		}

		question := models.Question{
			DomainID:        domainID,
			QuestionText:    qText,
			Explanation:     explanation,
			QuestionType:    qType,
			ImageURL:        imageURL,
			CodeBlock:       codeBlock,
			ExamBankVersion: examBankVersion,
		}

		var hasCorrectAnswer bool
		switch qType {
		case "single", "multi", "truefalse":
			var choices []models.Choice
			for j := 1; j <= 6; j++ {
				choiceText := rowMap[fmt.Sprintf("choice_%d", j)]
				correctFlag := rowMap[fmt.Sprintf("correct_%d", j)]
				explainChoice := rowMap[fmt.Sprintf("explain_%d", j)]

				if choiceText != "" {
					isCorrect := strings.ToLower(correctFlag) == "true"
					if isCorrect {
						hasCorrectAnswer = true
					}
					choices = append(choices, models.Choice{
						ChoiceText:  choiceText,
						IsCorrect:   isCorrect,
						Explanation: explainChoice,
						Order:       string('A' + j - 1), // Assign A, B, C...
					})
				}
			}
			if len(choices) == 0 {
				db.LogError(pool, sourceName, courseCode, examBankCSVPath, lineNum, "choices", "No choices provided for MCQ", "Single/Multi-choice questions require at least one choice.")
				return fmt.Errorf("no choices for MCQ at line %d for %s", lineNum, courseCode)
			}
			if !hasCorrectAnswer {
				db.LogError(pool, sourceName, courseCode, examBankCSVPath, lineNum, "correct_flag", "No correct answer marked for MCQ", "At least one choice must be marked TRUE for correctness.")
				return fmt.Errorf("no correct answer for MCQ at line %d for %s", lineNum, courseCode)
			}
			question.Choices = choices

		case "fillblank":
			if acceptableAnswers == "" {
				db.LogError(pool, sourceName, courseCode, examBankCSVPath, lineNum, "acceptable_answers", "Missing acceptable answers for fill-in-the-blank", "Fill-in-the-blank questions require pipe-separated acceptable answers.")
				return fmt.Errorf("missing acceptable_answers at line %d for %s", lineNum, courseCode)
			}
			question.AcceptableAnswers = strings.Split(acceptableAnswers, "|")
			hasCorrectAnswer = true // Fillblank always has "correct" answers if acceptable_answers is not empty

			if inputMethod != nil && *inputMethod != "" {
				lowerInputMethod := strings.ToLower(*inputMethod)
				if lowerInputMethod != "text" && lowerInputMethod != "terminal" {
					db.LogError(pool, sourceName, courseCode, examBankCSVPath, lineNum, "input_method", "Invalid input_method", "Must be 'text', 'terminal', or empty (defaults to 'text').")
					return fmt.Errorf("invalid input_method '%s' at line %d for %s", *inputMethod, lineNum, courseCode)
				}
				question.InputMethod = &lowerInputMethod
			} else {
				// Default to 'text' if empty or omitted in CSV
				defaultMethod := "text"
				question.InputMethod = &defaultMethod
			}

		default:
			db.LogError(pool, sourceName, courseCode, examBankCSVPath, lineNum, "question_type", "Unknown question type", "Must be 'single', 'multi', 'truefalse', or 'fillblank'.")
			return fmt.Errorf("unknown question type '%s' at line %d for %s", qType, lineNum, courseCode)
		}

		if !hasCorrectAnswer {
			db.LogError(pool, sourceName, courseCode, examBankCSVPath, lineNum, "", "Question has no valid correct answer definition", "Ensure at least one choice is TRUE for MCQ or acceptable_answers is present for fillblank.")
			return fmt.Errorf("question at line %d has no correct answer definition for %s", lineNum, courseCode)
		}

		// Add image_url and code_block validation (e.g., HTTP HEAD for image_url)
		if imageURL != nil && *imageURL != "" {
			// In a real system: Perform HTTP HEAD request to validate image URL
			// For now, simple URL format check
			if !strings.HasPrefix(*imageURL, "http://") && !strings.HasPrefix(*imageURL, "https://") {
				db.LogError(pool, sourceName, courseCode, examBankCSVPath, lineNum, "image_url", "Invalid image URL format", "Must be a valid HTTP/S URL.")
				return fmt.Errorf("invalid image_url '%s' at line %d for %s", *imageURL, lineNum, courseCode)
			}
		}

		questionsToSave = append(questionsToSave, question)
	}

	// Persist questions and choices/answers within the transaction
	for _, q := range questionsToSave {
		var questionID int
		err := tx.QueryRow(context.Background(), `
			INSERT INTO questions (domain_id, question_text, explanation, question_type, image_url, code_block, input_method, exam_bank_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (question_text, exam_bank_version) DO UPDATE SET -- Update if duplicate question_text for same version
				domain_id = EXCLUDED.domain_id,
				explanation = EXCLUDED.explanation,
				question_type = EXCLUDED.question_type,
				image_url = EXCLUDED.image_url,
				code_block = EXCLUDED.code_block,
				input_method = EXCLUDED.input_method
			RETURNING id
		`, q.DomainID, q.QuestionText, q.Explanation, q.QuestionType, q.ImageURL, q.CodeBlock, q.InputMethod, q.ExamBankVersion).Scan(&questionID)
		if err != nil {
			db.LogError(pool, sourceName, courseCode, "", 0, "", "Failed to insert/update question", fmt.Sprintf("Database error: %v, Question: %s", err, q.QuestionText))
			return fmt.Errorf("failed to insert/update question '%s': %w", q.QuestionText, err)
		}

		// Delete existing choices/answers for this question before re-inserting
		_, err = tx.Exec(context.Background(), `DELETE FROM choices WHERE question_id = $1`, questionID)
		if err != nil {
			return fmt.Errorf("failed to clear old choices for question %d: %w", questionID, err)
		}
		_, err = tx.Exec(context.Background(), `DELETE FROM fill_blank_answers WHERE question_id = $1`, questionID)
		if err != nil {
			return fmt.Errorf("failed to clear old fill_blank_answers for question %d: %w", questionID, err)
		}

		if q.QuestionType == "single" || q.QuestionType == "multi" || q.QuestionType == "truefalse" {
			for _, choice := range q.Choices {
				_, err := tx.Exec(context.Background(), `
					INSERT INTO choices (question_id, choice_text, is_correct, explanation)
					VALUES ($1, $2, $3, $4)
				`, questionID, choice.ChoiceText, choice.IsCorrect, choice.Explanation)
				if err != nil {
					db.LogError(pool, sourceName, courseCode, "", 0, "", "Failed to insert choice", fmt.Sprintf("Database error: %v, Choice: %s", err, choice.ChoiceText))
					return fmt.Errorf("failed to insert choice '%s' for question %d: %w", choice.ChoiceText, questionID, err)
				}
			}
		} else if q.QuestionType == "fillblank" {
			for _, answer := range q.AcceptableAnswers {
				_, err := tx.Exec(context.Background(), `
					INSERT INTO fill_blank_answers (question_id, acceptable_answer)
					VALUES ($1, $2)
				`, questionID, strings.ToLower(answer)) // Store in lowercase for case-insensitive comparison
				if err != nil {
					db.LogError(pool, sourceName, courseCode, "", 0, "", "Failed to insert acceptable answer", fmt.Sprintf("Database error: %v, Answer: %s", err, answer))
					return fmt.Errorf("failed to insert acceptable answer '%s' for question %d: %w", answer, questionID, err)
				}
			}
		}
	}

	// Commit transaction
	if err := tx.Commit(context.Background()); err != nil {
		db.LogError(pool, sourceName, courseCode, "", 0, "", "Failed to commit ingestion transaction", fmt.Sprintf("Database error: %v", err))
		return fmt.Errorf("failed to commit ingestion transaction for %s: %w", courseCode, err)
	}

	// Regenerate exams after successful ingestion
	err = exam.GenerateExamsForCourse(pool, courseID, courseMeta.MarketingName, examBankVersion, metadata)
	if err != nil {
		db.LogError(pool, sourceName, courseCode, "", 0, "", "Failed to regenerate exams after ingestion", fmt.Sprintf("Error: %v", err))
		return fmt.Errorf("failed to regenerate exams for %s: %w", courseCode, err)
	}

	return nil
}

func isMetadataRow(firstCol string) bool {
	switch firstCol {
	case "schema_version", "min_questions", "max_questions", "exam_time", "passing_score", "domains":
		return true
	default:
		return false
	}
}


// --- recap-server/exam/generator.go ---
package exam

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"recap-server/db"
	"recap-server/models"
	"recap-server/utils"
)

// GenerateExamsForCourse orchestrates the exam generation process for a specific course.
func GenerateExamsForCourse(pool *pgxpool.Pool, courseID int, courseMarketingName, examBankVersion string, metadata models.ExamBankMetadata) error {
	log.Printf("Starting exam generation for course ID: %d, Version: %s", courseID, examBankVersion)

	// Fetch all questions for this course and exam_bank_version
	questions, err := GetQuestionsByCourseAndVersion(pool, courseID, examBankVersion)
	if err != nil {
		return fmt.Errorf("failed to get questions for exam generation: %w", err)
	}
	if len(questions) == 0 {
		return fmt.Errorf("no questions available for course ID %d and version %s to generate exams", courseID, examBankVersion)
	}

	// Determine the optimal exam plan
	plan, err := GenerateExamPlan(questions, metadata.MinQuestions, metadata.MaxQuestions, metadata.Domains)
	if err != nil {
		return fmt.Errorf("failed to generate exam plan: %w", err)
	}

	log.Printf("Generated Exam Plan: NumExams=%d, QuestionsPerExam=%d, PerDomainPerExam=%v",
		plan.NumExams, plan.QuestionsPerExam, plan.PerDomainPerExam)

	// Clear existing exams and exam_questions for this course and exam_bank_version
	// This prevents old exam data from interfering and ensures fresh generation.
	_, err = pool.Exec(context.Background(), `
		DELETE FROM exam_questions WHERE exam_id IN (SELECT id FROM exams WHERE course_id = $1 AND exam_bank_version = $2);
		DELETE FROM exams WHERE course_id = $1 AND exam_bank_version = $2;
	`, courseID, examBankVersion)
	if err != nil {
		return fmt.Errorf("failed to clear existing exams and exam_questions for course %d, version %s: %w", courseID, examBankVersion, err)
	}

	// Generate individual exams
	for i := 0; i < plan.NumExams; i++ {
		examTitle := fmt.Sprintf("%s Practice Exam %d", courseMarketingName, i+1)

		// Create a deterministic seed for this exam based on version, course, and exam index
		seedStr := fmt.Sprintf("%s:%s:%d", examBankVersion, courseMarketingName, i)
		hasher := sha256.New()
		hasher.Write([]byte(seedStr))
		seed := int64(utils.BytesToInt(hasher.Sum(nil)))

		log.Printf("Generating exam '%s' with seed %d", examTitle, seed)

		selectedQuestions, err := selectQuestionsForExam(questions, plan.PerDomainPerExam, seed)
		if err != nil {
			db.LogError(pool, "exam_generation", courseMarketingName, "", 0, "", "Failed to select questions for exam", fmt.Sprintf("Exam: %s, Error: %v", examTitle, err))
			return fmt.Errorf("failed to select questions for exam %s: %w", examTitle, err)
		}

		if len(selectedQuestions) != plan.QuestionsPerExam {
			db.LogError(pool, "exam_generation", courseMarketingName, "", 0, "", "Generated exam question count mismatch", fmt.Sprintf("Expected %d, got %d for exam %s", plan.QuestionsPerExam, len(selectedQuestions), examTitle))
			return fmt.Errorf("generated exam question count mismatch for %s", examTitle)
		}

		// Insert the exam into the database
		domainWeightsJSON, err := json.Marshal(metadata.Domains)
		if err != nil {
			return fmt.Errorf("failed to marshal domain weights for exam %s: %w", examTitle, err)
		}

		var examID int
		err = pool.QueryRow(context.Background(), `
			INSERT INTO exams (course_id, title, exam_bank_version, min_questions, max_questions, exam_time, passing_score, domain_weights)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id
		`, courseID, examTitle, examBankVersion, metadata.MinQuestions, metadata.MaxQuestions, metadata.ExamTime, metadata.PassingScore, domainWeightsJSON).Scan(&examID)
		if err != nil {
			db.LogError(pool, "exam_generation", courseMarketingName, "", 0, "", "Failed to insert exam", fmt.Sprintf("Exam: %s, Error: %v", examTitle, err))
			return fmt.Errorf("failed to insert exam %s: %w", examTitle, err)
		}

		// Insert exam_questions
		// Randomize order within the exam after selection
		r := rand.New(rand.NewSource(seed)) // Use the same seed for reproducibility for order within exam
		r.Shuffle(len(selectedQuestions), func(i, j int) {
			selectedQuestions[i], selectedQuestions[j] = selectedQuestions[j], selectedQuestions[i]
		})

		for qOrder, q := range selectedQuestions {
			_, err := pool.Exec(context.Background(), `
				INSERT INTO exam_questions (exam_id, question_id, question_order, exam_bank_version)
				VALUES ($1, $2, $3, $4)
			`, examID, q.ID, qOrder+1, examBankVersion) // question_order starts from 1
			if err != nil {
				db.LogError(pool, "exam_generation", courseMarketingName, "", 0, "", "Failed to insert exam question", fmt.Sprintf("Exam: %s, Question ID: %d, Error: %v", examTitle, q.ID, err))
				return fmt.Errorf("failed to insert exam question %d for exam %d: %w", q.ID, examID, err)
			}
		}
		log.Printf("Successfully generated exam '%s' with %d questions.", examTitle, len(selectedQuestions))
	}

	log.Printf("Finished exam generation for course ID: %d, Version: %s", courseID, examBankVersion)
	return nil
}

// GenerateExamPlan determines the optimal number of questions per exam and number of exams.
func GenerateExamPlan(questions []models.Question, minQ, maxQ int, domainWeights map[string]float64) (models.ExamPlan, error) {
	domainCounts := make(map[string]int)
	for _, q := range questions {
		domainCounts[q.QuestionDomainName]++ // Assumes Question struct has a field QuestionDomainName
	}

	totalQuestions := len(questions)
	var bestPlan models.ExamPlan
	bestRemainder := totalQuestions // Initialize with worst case
	bestNumExams := 0

	for qPerExam := minQ; qPerExam <= maxQ; qPerExam++ {
		currentPerDomainPerExam := make(map[string]int)
		isValidPlan := true
		actualQuestionsInPlan := 0

		for domain, weight := range domainWeights {
			required := int(math.Round(float64(qPerExam) * weight))
			if required == 0 && weight > 0 { // Ensure at least 1 question if weight > 0 and qPerExam > 0
				required = 1
			}

			if domainCounts[domain] < required {
				// This 'qPerExam' value is not possible due to insufficient questions in this domain.
				// This scenario should reduce the range of qPerExam or indicate failure.
				// For simplicity, if this specific qPerExam is invalid, skip it.
				// A more complex logic could adjust qPerExam downwards based on constraints.
				isValidPlan = false
				break
			}
			currentPerDomainPerExam[domain] = required
			actualQuestionsInPlan += required
		}

		if !isValidPlan {
			continue
		}

		// If the actual number of questions based on weights is less than qPerExam, use actualQuestionsInPlan
		// This handles cases where rounding might sum to less than qPerExam, or domain weights don't perfectly add up.
		questionsUsedForThisQ := actualQuestionsInPlan
		if questionsUsedForThisQ == 0 { // Avoid division by zero
			continue
		}


		numExamsForThisQ := totalQuestions / questionsUsedForThisQ
		remainderForThisQ := totalQuestions % questionsUsedForThisQ

		// Criteria: lowest remainder, then highest numExams
		if remainderForThisQ < bestRemainder || (remainderForThisQ == bestRemainder && numExamsForThisQ > bestNumExams) {
			bestRemainder = remainderForThisQ
			bestQuestionsPerExam = questionsUsedForThisQ // Use the actual sum of required questions
			bestNumExams = numExamsForThisQ
			bestPlan = models.ExamPlan{
				NumExams:         bestNumExams,
				QuestionsPerExam: bestQuestionsPerExam,
				PerDomainPerExam: currentPerDomainPerExam,
			}
		}
	}

	if bestPlan.QuestionsPerExam == 0 {
		return models.ExamPlan{}, fmt.Errorf("insufficient questions to form any valid exam based on min/max questions and domain weights")
	}

	return bestPlan, nil
}


// selectQuestionsForExam selects a set of questions for a single exam, ensuring no reuse within the exam.
func selectQuestionsForExam(allQuestions []models.Question, perDomainRequired map[string]int, seed int64) ([]models.Question, error) {
	selected := make([]models.Question, 0, len(allQuestions))
	usedQuestionIDs := make(map[int]bool)

	// Group questions by domain
	questionsByDomain := make(map[string][]models.Question)
	for _, q := range allQuestions {
		questionsByDomain[q.QuestionDomainName] = append(questionsByDomain[q.QuestionDomainName], q)
	}

	r := rand.New(rand.NewSource(seed)) // Use the deterministic seed

	for domain, count := range perDomainRequired {
		available := questionsByDomain[domain]
		currentDomainSelections := make([]models.Question, 0, count)

		// Filter out already used questions and shuffle available questions for this domain
		shuffledAvailable := make([]models.Question, 0, len(available))
		for _, q := range available {
			if !usedQuestionIDs[q.ID] {
				shuffledAvailable = append(shuffledAvailable, q)
			}
		}

		// Shuffle the filtered list using the exam-specific random source
		r.Shuffle(len(shuffledAvailable), func(i, j int) {
			shuffledAvailable[i], shuffledAvailable[j] = shuffledAvailable[j], shuffledAvailable[i]
		})

		if len(shuffledAvailable) < count {
			return nil, fmt.Errorf("not enough unique questions in domain '%s' (available: %d, required: %d)", domain, len(shuffledAvailable), count)
		}

		// Select the required number of questions
		for i := 0; i < count; i++ {
			q := shuffledAvailable[i]
			currentDomainSelections = append(currentDomainSelections, q)
			usedQuestionIDs[q.ID] = true // Mark as used for this specific exam instance
		}
		selected = append(selected, currentDomainSelections...)
	}

	return selected, nil
}

// GetQuestionsByCourseAndVersion fetches questions for a given course ID and exam bank version.
// This is crucial for the exam generation process to operate on the correct set of questions.
func GetQuestionsByCourseAndVersion(pool *pgxpool.Pool, courseID int, examBankVersion string) ([]models.Question, error) {
	query := `
		SELECT
			q.id, q.question_text, q.explanation, q.question_type, q.image_url, q.code_block, q.input_method, q.exam_bank_version,
			d.name AS domain_name -- Join to get domain name
		FROM questions q
		JOIN domains d ON q.domain_id = d.id
		WHERE d.course_id = $1 AND q.exam_bank_version = $2
	`
	rows, err := pool.Query(context.Background(), query, courseID, examBankVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to query questions for course %d, version %s: %w", courseID, examBankVersion, err)
	}
	defer rows.Close()

	var questions []models.Question
	for rows.Next() {
		var q models.Question
		var domainName string
		// Scan directly into question struct and domain name
		if err := rows.Scan(
			&q.ID, &q.QuestionText, &q.Explanation, &q.QuestionType, &q.ImageURL, &q.CodeBlock, &q.InputMethod, &q.ExamBankVersion,
			&domainName,
		); err != nil {
			return nil, fmt.Errorf("failed to scan question row: %w", err)
		}
		q.QuestionDomainName = domainName // Set the domain name for internal use
		questions = append(questions, q)
	}
	return questions, nil
}

// UpdateQuestionValidityScores calculates and updates the validity_score for questions.
// This is a daily background job.
func UpdateQuestionValidityScores(pool *pgxpool.Pool) error {
    log.Println("Starting validity score calculation...")

    // Get the threshold for low-scoring students from settings
    thresholdStr, err := db.GetSetting(pool, "question_validity_threshold")
    if err != nil {
        log.Printf("Warning: Could not get validity threshold setting, defaulting to 0.25: %v", err)
        thresholdStr = "0.25"
    }
    threshold, err := strconv.ParseFloat(thresholdStr, 64)
    if err != nil {
        log.Printf("Warning: Invalid validity threshold setting '%s', defaulting to 0.25: %v", thresholdStr, err)
        threshold = 0.25
    }

    // Step 1: Identify high-scoring (top 75%) and low-scoring (bottom 25%) attempts
    // This is a simplified approach. A more robust system would define cohorts
    // based on full exam scores or other criteria.
    // Here, we define high/low score based on the overall exam attempt score_percent.

    // Get all completed exam attempts with their scores
    attemptsQuery := `
        SELECT id, score_percent, email
        FROM exam_attempts
        WHERE completed_at IS NOT NULL AND score_percent IS NOT NULL
        ORDER BY score_percent;
    `
    rows, err := pool.Query(context.Background(), attemptsQuery)
    if err != nil {
        return fmt.Errorf("failed to query exam attempts for validity score: %w", err)
    }
    defer rows.Close()

    var allAttempts []models.ExamAttempt
    for rows.Next() {
        var attempt models.ExamAttempt
        if err := rows.Scan(&attempt.ID, &attempt.ScorePercent, &attempt.Email); err != nil {
            return fmt.Errorf("failed to scan exam attempt: %w", err)
        }
        allAttempts = append(allAttempts, attempt)
    }

    if len(allAttempts) < 10 { // Need a minimum number of attempts to calculate meaningful stats
        log.Println("Not enough exam attempts to calculate validity scores. Skipping.")
        return nil
    }

    // Sort by score percent to determine quartiles
    // allAttempts is already ordered by score_percent from the query

    // Calculate quartile indices
    numAttempts := len(allAttempts)
    // top75PercentIndex := int(float64(numAttempts) * 0.25) // Top 25% of scores (indices from end)
    bottom25PercentIndex := int(float64(numAttempts) * threshold) // Bottom N% of scores

    // Collect IDs of high and low scoring attempts
    lowScoringAttemptIDs := make([]int, 0, bottom25PercentIndex)
    highScoringAttemptIDs := make([]int, 0, numAttempts-bottom25PercentIndex) // Using all above bottom 25% as 'high'
    
    for i, attempt := range allAttempts {
        if i < bottom25PercentIndex {
            lowScoringAttemptIDs = append(lowScoringAttemptIDs, attempt.ID)
        } else {
            highScoringAttemptIDs = append(highScoringAttemptIDs, attempt.ID)
        }
    }

    if len(lowScoringAttemptIDs) == 0 || len(highScoringAttemptIDs) == 0 {
        log.Println("Insufficient high/low scoring attempts to calculate validity scores. Skipping.")
        return nil
    }

    // Calculate correctness for each question for high/low scoring groups
    // This is a complex query to get correctness per question for high/low scorers
    // Correctness is 1 if all correct choices are selected and no incorrect choices are selected (MCQ)
    // or if text_answer matches acceptable_answers (Fill-in-the-Blank).

    // For simplicity and performance, this query will calculate
    // (correct_count_high - correct_count_low) / total_attempts_high_low

    log.Printf("Calculating validity for %d questions...", len(allQuestions))

    updateQuery := `
        WITH QuestionCorrectness AS (
            SELECT
                eq.question_id,
                ua.attempt_id,
                CASE
                    WHEN q.question_type IN ('single', 'multi', 'truefalse') THEN
                        -- Check if user selected all correct choices and no incorrect choices
                        (SELECT COUNT(c.id) FROM choices c WHERE c.question_id = q.id AND c.is_correct = TRUE) = CARDINALITY(ua.choice_ids) AND
                        (SELECT COUNT(c.id) FROM choices c WHERE c.question_id = q.id AND c.is_correct = FALSE AND c.id = ANY(ua.choice_ids)) = 0
                    WHEN q.question_type = 'fillblank' THEN
                        EXISTS (SELECT 1 FROM fill_blank_answers fba WHERE fba.question_id = q.id AND LOWER(fba.acceptable_answer) = LOWER(ua.text_answer))
                    ELSE FALSE
                END AS is_correct
            FROM user_answers ua
            JOIN exam_questions eq ON ua.exam_question_id = eq.id
            JOIN questions q ON eq.question_id = q.id
        ),
        QuestionPerformance AS (
            SELECT
                qc.question_id,
                SUM(CASE WHEN qc.is_correct AND ea.id = ANY($1::int[]) THEN 1 ELSE 0 END) AS high_correct_count,
                SUM(CASE WHEN qc.is_correct AND ea.id = ANY($2::int[]) THEN 1 ELSE 0 END) AS low_correct_count,
                COUNT(CASE WHEN ea.id = ANY($1::int[]) THEN 1 ELSE NULL END) AS high_attempt_count,
                COUNT(CASE WHEN ea.id = ANY($2::int[]) THEN 1 ELSE NULL END) AS low_attempt_count
            FROM QuestionCorrectness qc
            JOIN exam_attempts ea ON qc.attempt_id = ea.id
            GROUP BY qc.question_id
        )
        UPDATE questions q
        SET validity_score = (
            COALESCE(qp.high_correct_count, 0.0) / NULLIF(COALESCE(qp.high_attempt_count, 0.0), 0) -
            COALESCE(qp.low_correct_count, 0.0) / NULLIF(COALESCE(qp.low_attempt_count, 0.0), 0)
        )
        FROM QuestionPerformance qp
        WHERE q.id = qp.question_id;
    `
    // Convert []int to pgx-compatible array
    lowScoringIDs := "{" + strings.Trim(strings.Join(strings.Fields(fmt.Sprint(lowScoringAttemptIDs)), ","), "[]") + "}"
    highScoringIDs := "{" + strings.Trim(strings.Join(strings.Fields(fmt.Sprint(highScoringAttemptIDs)), ","), "[]") + "}"


    _, err = pool.Exec(context.Background(), updateQuery, highScoringIDs, lowScoringIDs)
    if err != nil {
        return fmt.Errorf("failed to update question validity scores: %w", err)
    }

    log.Println("Validity score calculation completed.")
    return nil
}

// Dummy struct for domain names in Question. This should ideally be handled by fetching the domain name during retrieval from DB.
type QuestionWithDomain struct {
    models.Question
    QuestionDomainName string `json:"question_domain_name"` // For internal use in exam generation
}


// --- recap-server/handlers/api_handlers.go ---
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"recap-server/db"
	"recap-server/models"
	"recap-server/utils"
)

// GetCourses lists available courses with exam counts.
// GET /api/v1/courses
func GetCourses(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		query := `
			SELECT
				c.id, c.course_code, c.marketing_name, c.duration_days, c.responsibility,
				COUNT(e.id) AS exam_count
			FROM courses c
			LEFT JOIN exams e ON c.id = e.course_id
			GROUP BY c.id
			ORDER BY c.marketing_name
		`
		rows, err := pool.Query(context.Background(), query)
		if err != nil {
			log.Printf("Error querying courses: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve courses"})
			return
		}
		defer rows.Close()

		var courses []models.Course
		for rows.Next() {
			var course models.Course
			if err := rows.Scan(
				&course.ID,
				&course.CourseCode,
				&course.MarketingName,
				&course.DurationDays,
				&course.Responsibility,
				&course.ExamCount,
			); err != nil {
				log.Printf("Error scanning course row: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process course data"})
				return
			}
			courses = append(courses, course)
		}
		c.JSON(http.StatusOK, courses)
	}
}

// GetExamsForCourse lists exams available for a specific course.
// GET /api/v1/courses/:course_code/exams
func GetExamsForCourse(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		courseCode := c.Param("course_code")

		query := `
			SELECT
				e.id, e.title, e.domain_weights, e.min_questions, e.max_questions, e.exam_time, e.passing_score
			FROM exams e
			JOIN courses c ON e.course_id = c.id
			WHERE c.course_code = $1
			ORDER BY e.title
		`
		rows, err := pool.Query(context.Background(), query, courseCode)
		if err != nil {
			log.Printf("Error querying exams for course %s: %v", courseCode, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve exams"})
			return
		}
		defer rows.Close()

		var exams []models.Exam
		for rows.Next() {
			var exam models.Exam
			var domainWeightsJSON []byte
			if err := rows.Scan(
				&exam.ID,
				&exam.Title,
				&domainWeightsJSON,
				&exam.MinQuestions,
				&exam.MaxQuestions,
				&exam.ExamTime,
				&exam.PassingScore,
			); err != nil {
				log.Printf("Error scanning exam row for course %s: %v", courseCode, err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process exam data"})
				return
				}
			if err := json.Unmarshal(domainWeightsJSON, &exam.DomainWeights); err != nil {
				log.Printf("Error unmarshaling domain weights for exam %d: %v", exam.ID, err)
				// Continue without domain weights or handle as appropriate
			}
			exams = append(exams, exam)
		}
		if len(exams) == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("No exams found for course code: %s", courseCode)})
			return
		}
		c.JSON(http.StatusOK, exams)
	}
}

// StartExamSession initiates a new exam attempt.
// POST /api/v1/exam_sessions
func StartExamSession(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req models.ExamSessionRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		userEmail := c.GetString("user_email") // Set by JWT middleware

		// Check if student exists, if not, create a basic record
		_, err := pool.Exec(context.Background(), `
			INSERT INTO students (email) VALUES ($1) ON CONFLICT (email) DO NOTHING
		`, userEmail)
		if err != nil {
			log.Printf("Error upserting student %s: %v", userEmail, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare student record"})
			return
		}

		// Fetch exam details
		var exam models.Exam
		var domainWeightsJSON []byte
		err = pool.QueryRow(context.Background(), `
			SELECT id, title, exam_time, exam_bank_version, domain_weights
			FROM exams WHERE id = $1
		`, req.ExamID).Scan(&exam.ID, &exam.Title, &exam.ExamTime, &exam.ExamBankVersion, &domainWeightsJSON)
		if err != nil {
			log.Printf("Error fetching exam %d: %v", req.ExamID, err)
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Exam with ID %d not found", req.ExamID)})
			return
		}
		if err := json.Unmarshal(domainWeightsJSON, &exam.DomainWeights); err != nil {
			log.Printf("Error unmarshaling domain weights for exam %d: %v", exam.ID, err)
			// Decide how to handle this, maybe return error or proceed without domain breakdown
		}

		// Create a new exam attempt
		var attemptID int
		err = pool.QueryRow(context.Background(), `
			INSERT INTO exam_attempts (exam_id, email, mode)
			VALUES ($1, $2, $3) RETURNING id
		`, req.ExamID, userEmail, req.Mode).Scan(&attemptID)
		if err != nil {
			log.Printf("Error creating exam attempt for exam %d, user %s: %v", req.ExamID, userEmail, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start exam session"})
			return
		}

		// Fetch questions for this exam
		questionsQuery := `
			SELECT
				eq.exam_question_id, q.question_text, q.question_type, q.image_url, q.code_block, q.input_method,
				ARRAY_AGG(jsonb_build_object('choice_id', ch.id, 'text', ch.choice_text, 'order', CASE WHEN ch.id IS NOT NULL THEN (64 + (ROW_NUMBER() OVER (PARTITION BY ch.question_id ORDER BY ch.id)))::text ELSE NULL END)) AS choices_json
			FROM exam_questions eq
			JOIN questions q ON eq.question_id = q.id
			LEFT JOIN choices ch ON q.id = ch.question_id
			WHERE eq.exam_id = $1
			GROUP BY eq.exam_question_id, q.question_text, q.question_type, q.image_url, q.code_block, q.input_method
			ORDER BY eq.question_order
		`
		rows, err := pool.Query(context.Background(), questionsQuery, req.ExamID)
		if err != nil {
			log.Printf("Error fetching questions for exam %d: %v", req.ExamID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load exam questions"})
			return
		}
		defer rows.Close()

		var sessionQuestions []models.Question
		for rows.Next() {
			var q models.Question
			var choicesJSON []byte
			var examQuestionID int // Use a local var for eq.exam_question_id
			if err := rows.Scan(
				&examQuestionID, &q.QuestionText, &q.QuestionType, &q.ImageURL, &q.CodeBlock, &q.InputMethod, &choicesJSON,
			); err != nil {
				log.Printf("Error scanning question for exam %d: %v", req.ExamID, err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process question data"})
				return
			}
			q.ExamQuestionID = examQuestionID // Assign the scanned exam_question_id

			if choicesJSON != nil {
				if err := json.Unmarshal(choicesJSON, &q.Choices); err != nil {
					log.Printf("Error unmarshaling choices for question %d: %v", q.ID, err)
					// Proceed without choices or handle error
				}
			}
			sessionQuestions = append(sessionQuestions, q)
		}

		resp := models.ExamSessionResponse{
			SessionID:        strconv.Itoa(attemptID), // Convert attempt ID to string for session_id
			ExamTitle:        exam.Title,
			Mode:             req.Mode,
			TimeLimitMinutes: exam.ExamTime,
			Questions:        sessionQuestions,
		}

		c.JSON(http.StatusOK, resp)
	}
}

// RecordAnswer records a student's answer for a question in a session.
// POST /api/v1/exam_sessions/:session_id/answer
func RecordAnswer(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionIDStr := c.Param("session_id")
		sessionID, err := strconv.Atoi(sessionIDStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid session ID"})
			return
		}

		var req models.AnswerRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		userEmail := c.GetString("user_email") // From JWT middleware

		// Verify session belongs to user and is not completed
		var attempt models.ExamAttempt
		err = pool.QueryRow(context.Background(), `
			SELECT id, email, mode, completed_at FROM exam_attempts WHERE id = $1
		`, sessionID).Scan(&attempt.ID, &attempt.Email, &attempt.Mode, &attempt.CompletedAt)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Exam session not found or accessible"})
			return
		}
		if attempt.Email != userEmail {
			c.JSON(http.StatusForbidden, gin.H{"error": "Access denied to this session"})
			return
		}
		if attempt.CompletedAt != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Session already completed"})
			return
		}

		// Get question details via exam_question_id
		var question models.Question
		var examQID int
		err = pool.QueryRow(context.Background(), `
			SELECT eq.id, q.id, q.question_type, q.explanation, q.input_method
			FROM exam_questions eq
			JOIN questions q ON eq.question_id = q.id
			WHERE eq.id = $1
		`, req.ExamQuestionID).Scan(&examQID, &question.ID, &question.QuestionType, &question.Explanation, &question.InputMethod)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Question not found in this exam session"})
			return
		}

		// Store the answer
		var pgChoiceIDs []int32 // pgx requires int32 for arrays
		for _, id := range req.ChoiceIDs {
			pgChoiceIDs = append(pgChoiceIDs, int32(id))
		}

		_, err = pool.Exec(context.Background(), `
			INSERT INTO user_answers (attempt_id, exam_question_id, choice_ids, text_answer)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (attempt_id, exam_question_id) DO UPDATE SET
				choice_ids = EXCLUDED.choice_ids,
				text_answer = EXCLUDED.text_answer
		`, sessionID, req.ExamQuestionID, pgChoiceIDs, utils.StringPtr(req.CommandText))
		if err != nil {
			log.Printf("Error recording answer for session %d, question %d: %v", sessionID, req.ExamQuestionID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to record answer"})
			return
		}

		// Provide immediate feedback in Practice Mode
		if attempt.Mode == "practice" {
			resp := models.AnswerResponse{
				Explanation: question.Explanation,
			}
			isCorrect := false

			if question.QuestionType == "single" || question.QuestionType == "multi" || question.QuestionType == "truefalse" {
				// Fetch correct choices and user's choices for comparison
				correctChoices := make(map[int]bool)
				rows, err := pool.Query(context.Background(), `
					SELECT id, is_correct, explanation FROM choices WHERE question_id = $1
				`, question.ID)
				if err != nil {
					log.Printf("Error fetching choices for question %d: %v", question.ID, err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get choice feedback"})
					return
				}
				defer rows.Close()

				var choiceFeedback []models.ChoiceFeedback
				allUserCorrect := true
				userSelectedAnyIncorrect := false

				for rows.Next() {
					var choiceID int
					var isCorrectChoice bool
					var explanation string
					if err := rows.Scan(&choiceID, &isCorrectChoice, &explanation); err != nil {
						log.Printf("Error scanning choice for question %d: %v", question.ID, err)
						continue
					}
					if isCorrectChoice {
						correctChoices[choiceID] = true
					}
					// Check if this choice was selected by the user
					userSelected := utils.ContainsInt(req.ChoiceIDs, choiceID)

					if isCorrectChoice && !userSelected {
						allUserCorrect = false // Missed a correct answer
					}
					if !isCorrectChoice && userSelected {
						userSelectedAnyIncorrect = true // Selected an incorrect answer
					}

					choiceFeedback = append(choiceFeedback, models.ChoiceFeedback{
						ChoiceID:    choiceID,
						IsCorrect:   isCorrectChoice,
						Explanation: explanation,
					})
				}
				resp.ChoiceFeedback = choiceFeedback

				// Determine overall correctness for MCQ
				if question.QuestionType == "single" || question.QuestionType == "truefalse" {
					isCorrect = allUserCorrect && !userSelectedAnyIncorrect && len(req.ChoiceIDs) == 1 && len(correctChoices) == 1
				} else { // Multi-choice (select all)
					isCorrect = allUserCorrect && !userSelectedAnyIncorrect && len(req.ChoiceIDs) == len(correctChoices)
				}


			} else if question.QuestionType == "fillblank" {
				// Fetch acceptable answers
				var acceptableAnswers []string
				rows, err := pool.Query(context.Background(), `
					SELECT acceptable_answer FROM fill_blank_answers WHERE question_id = $1
				`, question.ID)
				if err != nil {
					log.Printf("Error fetching acceptable answers for question %d: %v", question.ID, err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get fill-in-the-blank feedback"})
					return
				}
				defer rows.Close()

				for rows.Next() {
					var ans string
					if err := rows.Scan(&ans); err != nil {
						log.Printf("Error scanning acceptable answer: %v", err)
						continue
					}
					acceptableAnswers = append(acceptableAnswers, strings.ToLower(ans))
				}

				// Compare user's answer
				userAnswerLower := strings.ToLower(strings.TrimSpace(req.CommandText))
				isCorrect = utils.ContainsString(acceptableAnswers, userAnswerLower)

				if !isCorrect {
					// Apply fuzzy logic for hints
					if question.InputMethod != nil && *question.InputMethod == "terminal" {
						// Simple example: suggest common flags if a command is close
						if strings.HasPrefix(userAnswerLower, "ls") && !strings.Contains(userAnswerLower, "-l") {
							hint := "Did you mean `ls -l`? Check the flag."
							resp.Hint = &hint
						} else if strings.HasPrefix(userAnswerLower, "cat") && !strings.Contains(userAnswerLower, ".txt") {
							hint := "Are you looking for a file? Try specifying the file extension, e.g., `filename.txt`."
							resp.Hint = &hint
						}
					} else { // 'text' input
						// Simple example: suggest based on Levenshtein distance
						for _, accAns := range acceptableAnswers {
							if utils.LevenshteinDistance(userAnswerLower, accAns) <= 2 && len(userAnswerLower) > 0 { // Small edit distance
								hint := fmt.Sprintf("Did you mean `%s`?", accAns)
								resp.Hint = &hint
								break
							}
						}
					}
				}
			}
			resp.Correct = isCorrect
			c.JSON(http.StatusOK, resp)
		} else { // Simulation Mode
			c.JSON(http.StatusOK, gin.H{"saved": true})
		}
	}
}

// GetExamSessionStatus checks the progress of an exam session.
// GET /api/v1/exam_sessions/:session_id/status
func GetExamSessionStatus(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionIDStr := c.Param("session_id")
		sessionID, err := strconv.Atoi(sessionIDStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid session ID"})
			return
		}

		userEmail := c.GetString("user_email") // From JWT middleware

		var attempt models.ExamAttempt
		var examID int
		err = pool.QueryRow(context.Background(), `
			SELECT ea.id, ea.email, ea.completed_at, e.exam_time
			FROM exam_attempts ea
			JOIN exams e ON ea.exam_id = e.id
			WHERE ea.id = $1
		`, sessionID).Scan(&attempt.ID, &attempt.Email, &attempt.CompletedAt, &examID, &attempt.StartedAt) // need started_at for time_remaining
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Exam session not found or accessible"})
			return
		}
		if attempt.Email != userEmail {
			c.JSON(http.StatusForbidden, gin.H{"error": "Access denied to this session"})
			return
		}

		statusResp := models.ExamStatusResponse{
			Completed: attempt.CompletedAt != nil,
		}

		// Count answered and total questions
		var totalQuestions int
		err = pool.QueryRow(context.Background(), `
			SELECT COUNT(eq.id) FROM exam_questions eq JOIN exams e ON eq.exam_id = e.id WHERE e.id = $1
		`, examID).Scan(&totalQuestions)
		if err != nil {
			log.Printf("Error counting total questions for exam %d: %v", examID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get exam progress"})
			return
		}

		var answeredCount int
		err = pool.QueryRow(context.Background(), `
			SELECT COUNT(ua.id) FROM user_answers ua WHERE ua.attempt_id = $1
		`, sessionID).Scan(&answeredCount)
		if err != nil {
			log.Printf("Error counting answered questions for attempt %d: %v", sessionID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get exam progress"})
			return
		}

		statusResp.AnsweredCount = answeredCount
		statusResp.RemainingCount = totalQuestions - answeredCount

		// Calculate time remaining (only if not completed and in simulation mode)
		if !statusResp.Completed { // Only calculate if not completed
			var examTimeMinutes int
			err := pool.QueryRow(context.Background(), `SELECT exam_time FROM exams WHERE id = $1`, examID).Scan(&examTimeMinutes)
			if err != nil {
				log.Printf("Error fetching exam time for exam %d: %v", examID, err)
				// Continue without time remaining if error
			} else {
				elapsed := time.Since(attempt.StartedAt)
				timeLimit := time.Duration(examTimeMinutes) * time.Minute
				remaining := timeLimit - elapsed

				if remaining < 0 {
					remaining = 0 // Time's up
					// In a real app, you might auto-submit here
				}
				statusResp.TimeRemaining = fmt.Sprintf("%02d:%02d:%02d", int(remaining.Hours()), int(remaining.Minutes())%60, int(remaining.Seconds())%60)
			}
		} else {
			statusResp.TimeRemaining = "00:00:00" // Exam completed
		}

		c.JSON(http.StatusOK, statusResp)
	}
}

// SubmitExamSession finalizes an exam session and calculates the score.
// POST /api/v1/exam_sessions/:session_id/submit
func SubmitExamSession(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionIDStr := c.Param("session_id")
		sessionID, err := strconv.Atoi(sessionIDStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid session ID"})
			return
		}

		userEmail := c.GetString("user_email") // From JWT middleware

		// Verify session belongs to user and is not completed
		var attempt models.ExamAttempt
		var examID int
		var passingScore float64
		var domainWeightsJSON []byte
		err = pool.QueryRow(context.Background(), `
			SELECT ea.id, ea.email, ea.completed_at, e.id, e.passing_score, e.domain_weights
			FROM exam_attempts ea
			JOIN exams e ON ea.exam_id = e.id
			WHERE ea.id = $1
		`, sessionID).Scan(&attempt.ID, &attempt.Email, &attempt.CompletedAt, &examID, &passingScore, &domainWeightsJSON)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Exam session not found or accessible"})
			return
		}
		if attempt.Email != userEmail {
			c.JSON(http.StatusForbidden, gin.H{"error": "Access denied to this session"})
			return
		}
		if attempt.CompletedAt != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Session already completed"})
			return
		}

		var domainWeights map[string]float64
		if err := json.Unmarshal(domainWeightsJSON, &domainWeights); err != nil {
			log.Printf("Error unmarshaling domain weights for exam %d: %v", examID, err)
			domainWeights = make(map[string]float64) // Fallback to empty map
		}

		// Calculate score and domain breakdown
		var totalQuestions int
		err = pool.QueryRow(context.Background(), `
			SELECT COUNT(id) FROM exam_questions WHERE exam_id = $1
		`, examID).Scan(&totalQuestions)
		if err != nil {
			log.Printf("Error counting total questions for exam %d: %v", examID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to calculate score"})
			return
		}

		if totalQuestions == 0 {
			c.JSON(http.StatusOK, models.ExamSubmissionResponse{
				ScorePercent:   0,
				Pass:           false,
				DomainBreakdown: make(map[string]int),
				DetailedReport: []models.DetailedQuestionReport{},
			})
			return
		}

		correctCount := 0
		detailedReport := []models.DetailedQuestionReport{}
		domainCorrectCounts := make(map[string]int)
		domainTotalCounts := make(map[string]int)

		// Fetch all exam questions for this exam
		examQuestionsRows, err := pool.Query(context.Background(), `
			SELECT
				eq.id AS exam_question_id,
				q.id AS question_id,
				q.question_text,
				q.question_type,
				q.explanation,
				q.input_method,
				d.name AS domain_name,
				ua.choice_ids,
				ua.text_answer
			FROM exam_questions eq
			JOIN questions q ON eq.question_id = q.id
			JOIN domains d ON q.domain_id = d.id
			LEFT JOIN user_answers ua ON ua.exam_question_id = eq.id AND ua.attempt_id = $1
			WHERE eq.exam_id = $2
			ORDER BY eq.question_order
		`, sessionID, examID)
		if err != nil {
			log.Printf("Error fetching exam questions for scoring: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve exam questions for scoring"})
			return
		}
		defer examQuestionsRows.Close()

		for examQuestionsRows.Next() {
			var eq models.ExamQuestion
			var q models.Question
			var domainName string
			var userChoiceIDs []int32 // From DB array type
			var userTextAnswer *string

			if err := examQuestionsRows.Scan(
				&eq.ID, &q.ID, &q.QuestionText, &q.QuestionType, &q.Explanation, &q.InputMethod, &domainName,
				&userChoiceIDs, &userTextAnswer,
			); err != nil {
				log.Printf("Error scanning exam question for scoring: %v", err)
				continue
			}
			domainTotalCounts[domainName]++

			reportEntry := models.DetailedQuestionReport{
				Question:    q.QuestionText,
				Explanation: q.Explanation,
			}

			// Get correct answers for comparison
			isCorrect := false
			correctAnswerTexts := []string{}
			yourAnswerTexts := []string{}

			if q.QuestionType == "single" || q.QuestionType == "multi" || q.QuestionType == "truefalse" {
				correctChoicesMap := make(map[int]bool)
				var choicesFromDB []struct {
					ID int
					Text string
					IsCorrect bool
				}
				choicesRows, err := pool.Query(context.Background(), `
					SELECT id, choice_text, is_correct FROM choices WHERE question_id = $1
				`, q.ID)
				if err != nil {
					log.Printf("Error fetching choices for question %d during scoring: %v", q.ID, err)
					continue
				}
				for choicesRows.Next() {
					var cID int
					var cText string
					var cIsCorrect bool
					if err := choicesRows.Scan(&cID, &cText, &cIsCorrect); err != nil {
						log.Printf("Error scanning choice for question %d during scoring: %v", q.ID, err)
						continue
					}
					choicesFromDB = append(choicesFromDB, struct{ID int; Text string; IsCorrect bool}{cID, cText, cIsCorrect})
					if cIsCorrect {
						correctChoicesMap[cID] = true
						correctAnswerTexts = append(correctAnswerTexts, cText)
					}
				}
				choicesRows.Close()

				// Convert userChoiceIDs from int32 to int for comparison with int-based map
				userSelectedChoicesInt := make([]int, len(userChoiceIDs))
				for i, v := range userChoiceIDs {
					userSelectedChoicesInt[i] = int(v)
				}

				// Check correctness
				allUserChoicesCorrect := true
				userSelectedAnyIncorrect := false
				userSelectedCorrectCount := 0

				for _, choice := range choicesFromDB {
					userSelectedThisChoice := utils.ContainsInt(userSelectedChoicesInt, choice.ID)
					if choice.IsCorrect {
						if userSelectedThisChoice {
							userSelectedCorrectCount++
						} else {
							allUserChoicesCorrect = false // Missed a correct choice
						}
					} else { // Is incorrect choice
						if userSelectedThisChoice {
							userSelectedAnyIncorrect = true // Selected an incorrect choice
						}
					}

					if userSelectedThisChoice {
						yourAnswerTexts = append(yourAnswerTexts, choice.Text)
					}
				}

				if q.QuestionType == "single" || q.QuestionType == "truefalse" {
					isCorrect = allUserChoicesCorrect && !userSelectedAnyIncorrect && userSelectedCorrectCount == 1 && len(userSelectedChoicesInt) == 1
				} else if q.QuestionType == "multi" { // "select all"
					isCorrect = allUserChoicesCorrect && !userSelectedAnyIncorrect && userSelectedCorrectCount == len(correctChoicesMap) && len(userSelectedChoicesInt) == len(correctChoicesMap)
				}

			} else if q.QuestionType == "fillblank" {
				var acceptableAnswers []string
				ansRows, err := pool.Query(context.Background(), `
					SELECT acceptable_answer FROM fill_blank_answers WHERE question_id = $1
				`, q.ID)
				if err != nil {
					log.Printf("Error fetching acceptable answers for fillblank question %d: %v", q.ID, err)
					continue
				}
				for ansRows.Next() {
					var ans string
					if err := ansRows.Scan(&ans); err != nil {
						log.Printf("Error scanning acceptable answer for fillblank: %v", err)
						continue
					}
					acceptableAnswers = append(acceptableAnswers, strings.ToLower(ans))
				}
				ansRows.Close()

				if userTextAnswer != nil {
					yourAnswerTexts = []string{*userTextAnswer}
					isCorrect = utils.ContainsString(acceptableAnswers, strings.ToLower(strings.TrimSpace(*userTextAnswer)))
				} else {
					isCorrect = false
				}
				correctAnswerTexts = acceptableAnswers // Show all acceptable answers
			}

			if isCorrect {
				correctCount++
				domainCorrectCounts[domainName]++
				reportEntry.Result = "correct"
			} else {
				reportEntry.Result = "incorrect"
			}
			// If no answer provided, it's skipped/incorrect depending on interpretation
			if len(yourAnswerTexts) == 0 && userTextAnswer == nil {
				reportEntry.Result = "skipped"
			}

			reportEntry.YourAnswer = yourAnswerTexts
			reportEntry.CorrectAnswer = correctAnswerTexts
			detailedReport = append(detailedReport, reportEntry)
		}

		finalScorePercent := int(math.Round(float64(correctCount) / float64(totalQuestions) * 100))
		passed := finalScorePercent >= int(passingScore)

		// Calculate domain breakdown percentage
		domainBreakdown := make(map[string]int)
		for domain, correct := range domainCorrectCounts {
			total := domainTotalCounts[domain]
			if total > 0 {
				domainBreakdown[domain] = int(math.Round(float64(correct) / float64(total) * 100))
			} else {
				domainBreakdown[domain] = 0
			}
		}

		// Update exam_attempts record
		completedAt := time.Now()
		_, err = pool.Exec(context.Background(), `
			UPDATE exam_attempts SET completed_at = $1, score_percent = $2 WHERE id = $3
		`, completedAt, finalScorePercent, sessionID)
		if err != nil {
			log.Printf("Error updating exam attempt %d completion: %v", sessionID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to finalize exam session"})
			return
		}

		c.JSON(http.StatusOK, models.ExamSubmissionResponse{
			ScorePercent:   finalScorePercent,
			Pass:           passed,
			DomainBreakdown: domainBreakdown,
			DetailedReport: detailedReport,
		})
	}
}

// GetStudentHistory lists past exam attempts for a student.
// GET /api/v1/students/:email/history
func GetStudentHistory(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		studentEmail := c.Param("email")
		userEmail := c.GetString("user_email") // From JWT middleware

		// Ensure user can only view their own history (or admin can view all)
		userRoles := c.GetStringSlice("user_roles") // From JWT middleware
		isAdmin := utils.ContainsString(userRoles, "admin")

		if studentEmail != userEmail && !isAdmin {
			c.JSON(http.StatusForbidden, gin.H{"error": "Access denied. You can only view your own history."})
			return
		}

		query := `
			SELECT
				e.title,
				ea.score_percent,
				ea.completed_at,
				e.domain_weights -- To recalculate domain breakdown
			FROM exam_attempts ea
			JOIN exams e ON ea.exam_id = e.id
			WHERE ea.email = $1 AND ea.completed_at IS NOT NULL
			ORDER BY ea.completed_at DESC
		`
		rows, err := pool.Query(context.Background(), query, studentEmail)
		if err != nil {
			log.Printf("Error querying student history for %s: %v", studentEmail, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve student history"})
			return
		}
		defer rows.Close()

		var history []models.StudentHistoryEntry
		for rows.Next() {
			var entry models.StudentHistoryEntry
			var scorePercent sql.NullInt32 // Use NullInt32 for potentially NULL score_percent
			var completedAt time.Time
			var domainWeightsJSON []byte

			if err := rows.Scan(
				&entry.ExamTitle,
				&scorePercent,
				&completedAt,
				&domainWeightsJSON,
			); err != nil {
				log.Printf("Error scanning student history row for %s: %v", studentEmail, err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process history data"})
				return
			}
			if scorePercent.Valid {
				entry.ScorePercent = int(scorePercent.Int32)
			}
			entry.Timestamp = completedAt

			var domainWeights map[string]float64
			if err := json.Unmarshal(domainWeightsJSON, &domainWeights); err != nil {
				log.Printf("Error unmarshaling domain weights for history entry: %v", err)
				domainWeights = make(map[string]float64) // Fallback
			}

			// For domain breakdown in history, we need to re-calculate based on saved answers.
			// This is an expensive operation and typically done at submission or pre-calculated.
			// For simplicity here, we'll return an empty breakdown or just the overall score.
			// If full domain breakdown is strictly needed for history API, it should be stored
			// in exam_attempts directly, or this endpoint needs to be more complex.
			// For now, let's just make a dummy breakdown.
			entry.DomainBreakdown = make(map[string]int) // Placeholder

			// If domain breakdown is needed here, fetch user answers for this attempt,
			// compare against correct answers, and aggregate per domain, similar to SubmitExamSession.
			// This is left as an exercise to avoid excessive query complexity for a demo.

			history = append(history, entry)
		}
		c.JSON(http.StatusOK, history)
	}
}


// --- recap-server/handlers/admin_handlers.go ---
package handlers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"recap-server/db"
	"recap-server/ingestion"
	"recap-server/models"
	"recap-server/utils"
)

// AdminDashboard renders the admin dashboard with metrics and recent activity.
// GET /admin/dashboard
func AdminDashboard(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Fetch metrics
		var totalVerifiedUsers int
		_ = pool.QueryRow(context.Background(), `SELECT COUNT(DISTINCT email) FROM exam_attempts WHERE completed_at IS NOT NULL`).Scan(&totalVerifiedUsers)

		var totalExamsTaken int
		_ = pool.QueryRow(context.Background(), `SELECT COUNT(id) FROM exam_attempts`).Scan(&totalExamsTaken)

		var validationFailures int
		_ = pool.QueryRow(context.Background(), `SELECT COUNT(id) FROM error_logs WHERE source = 'ingestion'`).Scan(&validationFailures)

		// Recent activity: admin events
		adminEventsQuery := `SELECT id, timestamp, action, actor, target, notes FROM admin_events ORDER BY timestamp DESC LIMIT 5`
		adminEventsRows, err := pool.Query(context.Background(), adminEventsQuery)
		var recentAdminEvents []models.AdminEvent
		if err == nil {
			for adminEventsRows.Next() {
				var ae models.AdminEvent
				_ = adminEventsRows.Scan(&ae.ID, &ae.Timestamp, &ae.Action, &ae.Actor, &ae.Target, &ae.Notes)
				recentAdminEvents = append(recentAdminEvents, ae)
			}
			adminEventsRows.Close()
		} else {
			log.Printf("Error fetching recent admin events: %v", err)
		}

		// Recent activity: latest ingested courses
		recentCoursesQuery := `SELECT id, course_code, marketing_name FROM courses ORDER BY id DESC LIMIT 5`
		recentCoursesRows, err := pool.Query(context.Background(), recentCoursesQuery)
		var recentCourses []models.Course
		if err == nil {
			for recentCoursesRows.Next() {
				var course models.Course
				_ = recentCoursesRows.Scan(&course.ID, &course.CourseCode, &course.MarketingName)
				recentCourses = append(recentCourses, course)
			}
			recentCoursesRows.Close()
		} else {
			log.Printf("Error fetching recent courses: %v", err)
		}

		c.HTML(http.StatusOK, "admin_dashboard", gin.H{
			"Title":              "FIRM Admin Dashboard",
			"TotalVerifiedUsers": totalVerifiedUsers,
			"TotalExamsTaken":    totalExamsTaken,
			"ValidationFailures": validationFailures,
			"RecentAdminEvents":  recentAdminEvents,
			"RecentCourses":      recentCourses,
			"UserEmail":          c.GetString("user_email"),
		})
	}
}

// AdminListCourses lists courses for admin.
// GET /admin/courses
func AdminListCourses(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Pagination parameters
		pageStr := c.DefaultQuery("page", "1")
		page, _ := strconv.Atoi(pageStr)
		if page < 1 {
			page = 1
		}
		pageSize := 25
		offset := (page - 1) * pageSize

		searchQuery := c.Query("search")
		orderBy := c.DefaultQuery("order_by", "course_code")
		orderDir := c.DefaultQuery("order_dir", "asc")

		// Validate order_by and order_dir to prevent SQL injection
		validOrderBy := map[string]bool{"course_code": true, "marketing_name": true, "exams_taken": true}
		if !validOrderBy[orderBy] {
			orderBy = "course_code"
		}
		if orderDir != "asc" && orderDir != "desc" {
			orderDir = "asc"
		}

		query := fmt.Sprintf(`
			SELECT
				c.id, c.course_code, c.marketing_name, c.duration_days, c.responsibility,
				COUNT(ea.id) AS exams_taken
			FROM courses c
			LEFT JOIN exams e ON c.id = e.course_id
			LEFT JOIN exam_attempts ea ON e.id = ea.exam_id
			WHERE c.course_code ILIKE $1 OR c.marketing_name ILIKE $1
			GROUP BY c.id
			ORDER BY %s %s
			LIMIT $2 OFFSET $3
		`, orderBy, orderDir)

		rows, err := pool.Query(context.Background(), query, "%"+searchQuery+"%", pageSize, offset)
		if err != nil {
			log.Printf("Error querying courses for admin: %v", err)
			c.HTML(http.StatusInternalServerError, "admin_courses", gin.H{"error": "Failed to retrieve courses"})
			return
		}
		defer rows.Close()

		var courses []struct {
			models.Course
			ExamsTaken int `json:"exams_taken"`
		}
		for rows.Next() {
			var course struct {
				models.Course
				ExamsTaken int
			}
			if err := rows.Scan(
				&course.ID, &course.CourseCode, &course.MarketingName, &course.DurationDays, &course.Responsibility, &course.ExamsTaken,
			); err != nil {
				log.Printf("Error scanning course row for admin: %v", err)
				c.HTML(http.StatusInternalServerError, "admin_courses", gin.H{"error": "Failed to process course data"})
				return
			}
			courses = append(courses, course)
		}

		// Count total records for pagination
		var totalCourses int
		countQuery := `SELECT COUNT(DISTINCT c.id) FROM courses c WHERE c.course_code ILIKE $1 OR c.marketing_name ILIKE $1`
		pool.QueryRow(context.Background(), countQuery, "%"+searchQuery+"%").Scan(&totalCourses)
		totalPages := int(math.Ceil(float64(totalCourses) / float64(pageSize)))

		c.HTML(http.StatusOK, "admin_courses", gin.H{
			"Title":       "Manage Courses",
			"Courses":     courses,
			"CurrentPage": page,
			"TotalPages":  totalPages,
			"SearchQuery": searchQuery,
			"OrderBy":     orderBy,
			"OrderDir":    orderDir,
			"UserEmail":   c.GetString("user_email"),
		})
	}
}

// AdminCreateCourse handles creating a new course.
// POST /admin/courses
func AdminCreateCourse(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req models.AdminCourseCreateRequest
		if err := c.ShouldBind(&req); err != nil { // Use ShouldBind for form data
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// Basic validation: Check if course_code already exists
		var existingID int
		err := pool.QueryRow(context.Background(), `SELECT id FROM courses WHERE course_code = $1`, req.CourseCode).Scan(&existingID)
		if err == nil {
			c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("Course with code %s already exists", req.CourseCode)})
			return
		}

		_, err = pool.Exec(context.Background(), `
			INSERT INTO courses (name, course_code, duration_days, marketing_name, responsibility)
			VALUES ($1, $2, $3, $4, $5)
		`, req.Name, req.CourseCode, req.DurationDays, req.MarketingName, req.Responsibility)
		if err != nil {
			log.Printf("Error creating course: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create course"})
			return
		}

		db.LogAdminEvent(pool, c.GetString("user_email"), "create_course", req.CourseCode, fmt.Sprintf("New course: %s (%s)", req.Name, req.CourseCode))
		c.JSON(http.StatusCreated, gin.H{"message": "Course created successfully", "course_code": req.CourseCode})
	}
}

// AdminUpdateCourse handles updating an existing course.
// PUT /admin/courses/:course_code
func AdminUpdateCourse(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		courseCode := c.Param("course_code")
		var req models.AdminCourseCreateRequest // Reuse struct for update fields
		if err := c.ShouldBindJSON(&req); err != nil { // Assuming JSON for PUT
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		res, err := pool.Exec(context.Background(), `
			UPDATE courses SET
				name = $1,
				duration_days = $2,
				marketing_name = $3,
				responsibility = $4
			WHERE course_code = $5
		`, req.Name, req.DurationDays, req.MarketingName, req.Responsibility, courseCode)
		if err != nil {
			log.Printf("Error updating course %s: %v", courseCode, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update course"})
			return
		}

		if res.RowsAffected() == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Course with code %s not found", courseCode)})
			return
		}

		db.LogAdminEvent(pool, c.GetString("user_email"), "update_course", courseCode, fmt.Sprintf("Updated course: %s", req.MarketingName))
		c.JSON(http.StatusOK, gin.H{"message": "Course updated successfully", "course_code": courseCode})
	}
}

// AdminDeleteCourse handles deleting a course.
// DELETE /admin/courses/:course_code
func AdminDeleteCourse(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		courseCode := c.Param("course_code")

		res, err := pool.Exec(context.Background(), `DELETE FROM courses WHERE course_code = $1`, courseCode)
		if err != nil {
			log.Printf("Error deleting course %s: %v", courseCode, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete course"})
			return
		}

		if res.RowsAffected() == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("Course with code %s not found", courseCode)})
			return
		}

		db.LogAdminEvent(pool, c.GetString("user_email"), "delete_course", courseCode, fmt.Sprintf("Deleted course: %s", courseCode))
		c.JSON(http.StatusOK, gin.H{"message": "Course deleted successfully", "course_code": courseCode})
	}
}

// AdminErrorLogs displays validation error logs.
// GET /admin/error_logs
func AdminErrorLogs(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		searchQuery := c.Query("search")
		searchSource := c.Query("source") // e.g., "ingestion", "exam_generation"

		query := `
			SELECT id, timestamp, source, course_code, file_path, line_number, field_name, error_message, suggested_fix
			FROM error_logs
			WHERE (course_code ILIKE $1 OR error_message ILIKE $1)
			AND ($2 = '' OR source = $2)
			ORDER BY timestamp DESC
		`
		rows, err := pool.Query(context.Background(), query, "%"+searchQuery+"%", searchSource)
		if err != nil {
			log.Printf("Error querying error logs: %v", err)
			c.HTML(http.StatusInternalServerError, "admin_error_logs", gin.H{"error": "Failed to retrieve error logs"})
			return
		}
		defer rows.Close()

		var logs []models.ErrorLog
		for rows.Next() {
			var logEntry models.ErrorLog
			if err := rows.Scan(
				&logEntry.ID, &logEntry.Timestamp, &logEntry.Source, &logEntry.CourseCode,
				&logEntry.FilePath, &logEntry.LineNumber, &logEntry.FieldName, &logEntry.ErrorMessage, &logEntry.SuggestedFix,
			); err != nil {
				log.Printf("Error scanning error log row: %v", err)
				continue
			}
			logs = append(logs, logEntry)
		}

		c.HTML(http.StatusOK, "admin_error_logs", gin.H{
			"Title":        "Error Logs",
			"ErrorLogs":    logs,
			"SearchQuery":  searchQuery,
			"SearchSource": searchSource,
			"UserEmail":    c.GetString("user_email"),
		})
	}
}

// AdminUserActivity displays student exam attempts.
// GET /admin/user_activity
func AdminUserActivity(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		searchEmail := c.Query("search") // Filter by email

		query := `
			SELECT
				ea.id, ea.email, e.title, ea.score_percent, ea.started_at, ea.completed_at
			FROM exam_attempts ea
			JOIN exams e ON ea.exam_id = e.id
			WHERE ea.email ILIKE $1
			ORDER BY ea.started_at DESC
		`
		rows, err := pool.Query(context.Background(), query, "%"+searchEmail+"%")
		if err != nil {
			log.Printf("Error querying user activity: %v", err)
			c.HTML(http.StatusInternalServerError, "admin_user_activity", gin.H{"error": "Failed to retrieve user activity"})
			return
		}
		defer rows.Close()

		var attempts []struct {
			ID          int
			Email       string
			ExamTitle   string
			ScorePercent *int // Can be null
			StartedAt   time.Time
			CompletedAt *time.Time // Can be null
		}
		for rows.Next() {
			var attempt struct {
				ID          int
				Email       string
				ExamTitle   string
				ScorePercent *int
				StartedAt   time.Time
				CompletedAt *time.Time
			}
			if err := rows.Scan(
				&attempt.ID, &attempt.Email, &attempt.ExamTitle, &attempt.ScorePercent, &attempt.StartedAt, &attempt.CompletedAt,
			); err != nil {
				log.Printf("Error scanning user activity row: %v", err)
				continue
			}
			attempts = append(attempts, attempt)
		}

		c.HTML(http.StatusOK, "admin_user_activity", gin.H{
			"Title":       "User Activity",
			"Attempts":    attempts,
			"SearchEmail": searchEmail,
			"UserEmail":   c.GetString("user_email"),
		})
	}
}

// AdminQuestionStats displays question performance and allows flagging.
// GET /admin/question_stats
func AdminQuestionStats(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		searchQuery := c.Query("search")
		searchDomain := c.Query("domain")

		query := `
			SELECT
				q.id, q.question_text, q.question_type, d.name AS domain_name, q.validity_score, q.flagged,
				COUNT(ua.id) AS times_attempted,
				SUM(CASE WHEN
					(q.question_type IN ('single', 'multi', 'truefalse') AND
						(SELECT COUNT(c.id) FROM choices c WHERE c.question_id = q.id AND c.is_correct = TRUE) = CARDINALITY(ua.choice_ids) AND
						(SELECT COUNT(c.id) FROM choices c WHERE c.question_id = q.id AND c.is_correct = FALSE AND c.id = ANY(ua.choice_ids)) = 0)
					OR
					(q.question_type = 'fillblank' AND
						EXISTS (SELECT 1 FROM fill_blank_answers fba WHERE fba.question_id = q.id AND LOWER(fba.acceptable_answer) = LOWER(ua.text_answer)))
				THEN 1 ELSE 0 END) AS correct_count
			FROM questions q
			JOIN domains d ON q.domain_id = d.id
			LEFT JOIN exam_questions eq ON q.id = eq.question_id
			LEFT JOIN user_answers ua ON eq.id = ua.exam_question_id
			WHERE (q.question_text ILIKE $1 OR d.name ILIKE $1)
			AND ($2 = '' OR d.name ILIKE $2)
			GROUP BY q.id, d.name
			ORDER BY q.id
		`
		rows, err := pool.Query(context.Background(), query, "%"+searchQuery+"%", "%"+searchDomain+"%")
		if err != nil {
			log.Printf("Error querying question stats: %v", err)
			c.HTML(http.StatusInternalServerError, "admin_question_stats", gin.H{"error": "Failed to retrieve question stats"})
			return
		}
		defer rows.Close()

		var stats []models.QuestionStats
		for rows.Next() {
			var qs models.QuestionStats
			if err := rows.Scan(
				&qs.QuestionID, &qs.QuestionText, &qs.QuestionType, &qs.Domain, &qs.ValidityScore, &qs.Flagged,
				&qs.TimesAttempted, &qs.CorrectCount,
			); err != nil {
				log.Printf("Error scanning question stats row: %v", err)
				continue
			}
			stats = append(stats, qs)
		}

		c.HTML(http.StatusOK, "admin_question_stats", gin.H{
			"Title":        "Question Statistics",
			"Stats":        stats,
			"SearchQuery":  searchQuery,
			"SearchDomain": searchDomain,
			"UserEmail":    c.GetString("user_email"),
		})
	}
}

// AdminSettings displays and handles updates for server settings.
// GET/POST /admin/settings
func AdminSettings(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == "POST" {
			AdminUpdateSettings(pool)(c) // Delegate to update handler
			return
		}

		rows, err := pool.Query(context.Background(), `SELECT key, value, description FROM settings ORDER BY key`)
		if err != nil {
			log.Printf("Error querying settings: %v", err)
			c.HTML(http.StatusInternalServerError, "admin_settings", gin.H{"error": "Failed to retrieve settings"})
			return
		}
		defer rows.Close()

		var settings []models.Setting
		for rows.Next() {
			var s models.Setting
			if err := rows.Scan(&s.Key, &s.Value, &s.Description); err != nil {
				log.Printf("Error scanning setting row: %v", err)
				continue
			}
			settings = append(settings, s)
		}

		c.HTML(http.StatusOK, "admin_settings", gin.H{
			"Title":     "Manage Server Settings",
			"Settings":  settings,
			"UserEmail": c.GetString("user_email"),
		})
	}
}

// AdminUpdateSettings handles updating server settings.
// POST /admin/settings
func AdminUpdateSettings(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		// This handler assumes form submission with key-value pairs
		// For a more robust solution, validate each setting based on its type (int, bool, duration)
		updates := make(map[string]string)
		for key, values := range c.Request.PostForm {
			if len(values) > 0 {
				updates[key] = values[0]
			}
		}

		tx, err := pool.Begin(context.Background())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start transaction for settings update"})
			return
		}
		defer tx.Rollback(context.Background())

		actor := c.GetString("user_email")
		var failedUpdates []string

		for key, value := range updates {
			_, err := tx.Exec(context.Background(), `
				UPDATE settings SET value = $1, updated_at = NOW(), updated_by = $2 WHERE key = $3
			`, value, actor, key)
			if err != nil {
				log.Printf("Error updating setting %s: %v", key, err)
				failedUpdates = append(failedUpdates, key)
			}
			db.LogAdminEvent(pool, actor, "update_setting", key, fmt.Sprintf("Set to: %s", value))
		}

		if len(failedUpdates) > 0 {
			tx.Rollback(context.Background()) // Rollback if any update failed
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to update some settings: %s", strings.Join(failedUpdates, ", "))})
			return
		}

		if err := tx.Commit(context.Background()); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to commit settings updates"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "Settings updated successfully"})
	}
}

// TriggerIngestion allows admin to manually trigger ingestion for a course.
// POST /admin/ingest/:course_code
func TriggerIngestion(pool *pgxpool.Pool, labsRepoPath string) gin.HandlerFunc {
	return func(c *gin.Context) {
		courseCode := c.Param("course_code")
		actor := c.GetString("user_email") // Get actor from JWT

		// In a real system, you might pull the latest from git here or ensure it's already updated.
		// For now, it assumes the labsRepoPath is kept up-to-date by an external process.

		err := ingestion.ProcessCourseData(pool, courseCode, labsRepoPath)
		if err != nil {
			log.Printf("Manual ingestion failed for %s: %v", courseCode, err)
			db.LogAdminEvent(pool, actor, "manual_ingestion_failed", courseCode, fmt.Sprintf("Error: %v", err))
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Ingestion failed: %v", err)})
			return
		}

		db.LogAdminEvent(pool, actor, "manual_ingestion_success", courseCode, "Ingestion and exam regeneration completed.")
		c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("Ingestion and exam regeneration for course '%s' triggered successfully. Check logs/admin dashboard for status.", courseCode)})
	}
}


// --- recap-server/middleware/auth.go ---
package middleware

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// claims struct to hold JWT custom claims
type claims struct {
	Email string   `json:"sub"`
	Roles []string `json:"roles"`
	jwt.RegisteredClaims
}

// AuthMiddleware validates the FIRM JWT and sets user context.
func AuthMiddleware(jwtSigningKey, issuer string) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatus(http.StatusUnauthorized, gin.H{"error": "Authorization header required"})
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if !(len(parts) == 2 && strings.ToLower(parts[0]) == "bearer") {
			c.AbortWithStatus(http.StatusUnauthorized, gin.H{"error": "Authorization header format must be Bearer {token}"})
			return
		}

		tokenString := parts[1]

		token, err := jwt.ParseWithClaims(tokenString, &claims{}, func(token *jwt.Token) (interface{}, error) {
			// Validate the alg is what you expect
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
			}
			return []byte(jwtSigningKey), nil
		})

		if err != nil {
			log.Printf("JWT parsing error: %v", err)
			if err == jwt.ErrSignatureInvalid {
				c.AbortWithStatus(http.StatusUnauthorized, gin.H{"error": "Invalid token signature"})
				return
			}
			if ve, ok := err.(*jwt.ValidationError); ok {
				if ve.Errors&jwt.ValidationErrorExpired != 0 {
					c.AbortWithStatus(http.StatusUnauthorized, gin.H{"error": "Token expired"})
					return
				}
			}
			c.AbortWithStatus(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
			return
		}

		if claims, ok := token.Claims.(*claims); ok && token.Valid {
			// Validate issuer
			if claims.Issuer != issuer {
				c.AbortWithStatus(http.StatusUnauthorized, gin.H{"error": "Invalid token issuer"})
				return
			}
			// Validate expiration
			if claims.ExpiresAt == nil || claims.ExpiresAt.Before(time.Now()) {
				c.AbortWithStatus(http.StatusUnauthorized, gin.H{"error": "Token expired"})
				return
			}

			c.Set("user_email", claims.Email)
			c.Set("user_roles", claims.Roles) // Pass roles to context for RBAC
			c.Next()
		} else {
			c.AbortWithStatus(http.StatusUnauthorized, gin.H{"error": "Invalid token claims"})
			return
		}
	}
}

// RoleCheckMiddleware checks if the user has one of the required roles.
func RoleCheckMiddleware(requiredRoles []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		userRoles, exists := c.Get("user_roles")
		if !exists {
			c.AbortWithStatus(http.StatusForbidden, gin.H{"error": "User roles not found in context"})
			return
		}

		roles, ok := userRoles.([]string)
		if !ok {
			c.AbortWithStatus(http.StatusInternalServerError, gin.H{"error": "Invalid user roles format"})
			return
		}

		hasRequiredRole := false
		for _, requiredRole := range requiredRoles {
			for _, userRole := range roles {
				if userRole == requiredRole {
					hasRequiredRole = true
					break
				}
			}
			if hasRequiredRole {
				break
			}
		}

		if !hasRequiredRole {
			c.AbortWithStatus(http.StatusForbidden, gin.H{"error": "Insufficient permissions"})
			return
		}
		c.Next()
	}
}

// Logger middleware for request logging
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		t := time.Now()
		c.Next()
		latency := time.Since(t)
		log.Printf("[RECAP] %s %s %s %d %s", c.Request.Method, c.Request.URL.Path, c.Request.Proto, c.Writer.Status(), latency)
	}
}


// --- recap-server/utils/utils.go ---
package utils

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// StringPtr returns a pointer to a string, or nil if empty.
func StringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ContainsInt checks if an int slice contains a specific int.
func ContainsInt(slice []int, item int) bool {
	for _, a := range slice {
		if a == item {
			return true
		}
	}
	return false
}

// ContainsString checks if a string slice contains a specific string.
func ContainsString(slice []string, item string) bool {
	for _, a := range slice {
		if a == item {
			return true
		}
	}
	return false
}

// ParseDomainWeights parses a pipe-separated string of "Name:Weight" into a map.
// Also validates that weights sum to 1.0 (within 0.01 tolerance).
func ParseDomainWeights(domainStr string) (map[string]float64, error) {
	weights := make(map[string]float64)
	totalWeight := 0.0

	pairs := strings.Split(domainStr, "|")
	for _, pair := range pairs {
		parts := strings.Split(pair, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid domain format: %s. Expected 'Name:Weight'", pair)
		}
		domainName := strings.TrimSpace(parts[0])
		weightStr := strings.TrimSpace(parts[1])

		weight, err := strconv.ParseFloat(weightStr, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid weight for domain '%s': %s", domainName, weightStr)
		}
		if weight < 0 || weight > 1 {
			return nil, fmt.Errorf("domain weight for '%s' must be between 0.0 and 1.0", domainName)
		}
		weights[domainName] = weight
		totalWeight += weight
	}

	if math.Abs(totalWeight-1.0) > 0.01 { // Allow for slight floating point inaccuracies
		return nil, fmt.Errorf("domain weights do not sum to 1.0 (sum is %.2f)", totalWeight)
	}

	return weights, nil
}

// LevenshteinDistance calculates the Levenshtein distance between two strings.
// Used for fuzzy matching in fill-in-the-blank hints.
func LevenshteinDistance(s1, s2 string) int {
	len1 := len(s1)
	len2 := len(s2)

	if len1 == 0 {
		return len2
	}
	if len2 == 0 {
		return len1
	}

	dp := make([][]int, len1+1)
	for i := range dp {
		dp[i] = make([]int, len2+1)
	}

	for i := 0; i <= len1; i++ {
		dp[i][0] = i
	}
	for j := 0; j <= len2; j++ {
		dp[0][j] = j
	}

	for i := 1; i <= len1; i++ {
		for j := 1; j <= len2; j++ {
			cost := 0
			if s1[i-1] != s2[j-1] {
				cost = 1
			}
			dp[i][j] = min(dp[i-1][j]+1, dp[i][j-1]+1, dp[i-1][j-1]+cost)
		}
	}
	return dp[len1][len2]
}

func min(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

// BytesToInt converts a byte slice (e.g., from SHA256 sum) to an int64.
// Used for generating a deterministic seed from a hash.
func BytesToInt(b []byte) int64 {
	// Take the first 8 bytes (or less if available) to fit into int64
	var i int64
	for idx, val := range b {
		if idx >= 8 {
			break
		}
		i = (i << 8) | int64(val)
	}
	return i
}


// --- recap-server/templates/layout.html ---
{{define "admin_layout"}}
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>{{.Title}} - RECAP Admin</title>
    <script src="https://cdn.tailwindcss.com"></script>
    <link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;600;700&display=swap" rel="stylesheet">
    <style>
        body { font-family: 'Inter', sans-serif; }
        .sidebar { transition: width 0.3s ease; }
        .sidebar.collapsed { width: 4rem; }
        .sidebar.expanded { width: 16rem; }
    </style>
</head>
<body class="bg-gray-100 flex min-h-screen">
    <!-- Sidebar -->
    <aside id="sidebar" class="sidebar bg-gray-800 text-white w-64 p-4 space-y-4 shadow-lg flex-shrink-0 expanded">
        <h1 class="text-2xl font-bold text-center mb-6">RECAP Admin</h1>
        <nav>
            <ul class="space-y-2">
                <li>
                    <a href="/admin/dashboard" class="flex items-center px-4 py-2 rounded-md hover:bg-gray-700">
                        <svg class="w-5 h-5 mr-3" fill="none" stroke="currentColor" viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 12l2-2m0 0l7-7 7 7M5 10v10a1 1 0 001 1h3m10-11l2 2m0 0l7 7m-3 7v-10a1 1 0 00-1-1h-3"></path></svg>
                        Dashboard
                    </a>
                </li>
                <li>
                    <a href="/admin/courses" class="flex items-center px-4 py-2 rounded-md hover:bg-gray-700">
                        <svg class="w-5 h-5 mr-3" fill="none" stroke="currentColor" viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 6.253v13m0-13C10.832 5.477 9.246 5 7.5 5S4.168 5.477 3 6.253v13C4.168 18.523 5.754 18 7.5 18s3.332.477 4.5 1.253m0-13C13.168 5.477 14.754 5 16.5 5c1.747 0 3.332.477 4.5 1.253v13C19.832 18.523 18.246 18 16.5 18c-1.747 0-3.332.477-4.5 1.253"></path></svg>
                        Courses
                    </a>
                </li>
                <li>
                    <a href="/admin/error_logs" class="flex items-center px-4 py-2 rounded-md hover:bg-gray-700">
                        <svg class="w-5 h-5 mr-3" fill="none" stroke="currentColor" viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 8v4m0 4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z"></path></svg>
                        Error Logs
                    </a>
                </li>
                <li>
                    <a href="/admin/user_activity" class="flex items-center px-4 py-2 rounded-md hover:bg-gray-700">
                        <svg class="w-5 h-5 mr-3" fill="none" stroke="currentColor" viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M17 20h5v-2a3 3 0 00-5.356-1.857M17 20H2v-2a3 3 0 015.356-1.857M17 20v-2c0-.653-.106-1.288-.302-1.875M16 4v3.134a4.102 4.102 0 01-.137.587M6 10v1.134a4.102 4.102 0 00.302 1.875m-3.993-2.003L2.356 12H1m4.356-1.857a3.001 3.001 0 00-2.356 1.857C2.106 13.288 2 13.653 2 14v2M14 4h7m-3 3l-3-3M17 7L4 20"></path></svg>
                        User Activity
                    </a>
                </li>
                <li>
                    <a href="/admin/question_stats" class="flex items-center px-4 py-2 rounded-md hover:bg-gray-700">
                        <svg class="w-5 h-5 mr-3" fill="none" stroke="currentColor" viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 19v-6a2 2 0 00-2-2H5a2 2 0 00-2 2v6a2 2 0 002 2h2a2 2 0 002-2zm0 0h.01M9 19h7m-7 0h2m-7 0H4a2 2 0 01-2-2v-6a2 2 0 012-2h2a2 2 0 012 2v6zm5 0h2m-2 0a2 2 0 01-2-2v-6a2 2 0 012-2h2a2 2 0 012 2v6a2 2 0 01-2 2zm0 0h2m-2 0a2 2 0 01-2-2v-6a2 2 0 012-2h2a2 2 0 012 2v6a2 2 0 01-2 2z"></path></svg>
                        Question Stats
                    </a>
                </li>
                <li>
                    <a href="/admin/settings" class="flex items-center px-4 py-2 rounded-md hover:bg-gray-700">
                        <svg class="w-5 h-5 mr-3" fill="none" stroke="currentColor" viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M10.325 4.317c.426-1.756 2.924-1.756 3.35 0a1.724 1.724 0 002.573 1.066c1.543-.94 3.31.826 2.37 2.37a1.724 1.724 0 001.065 2.572c1.756.426 1.756 2.924 0 3.35a1.724 1.724 0 00-1.066 2.573c.94 1.543-.826 3.31-2.37 2.37a1.724 1.724 0 00-2.572 1.065c-.426 1.756-2.924 1.756-3.35 0a1.724 1.724 0 00-2.573-1.066c-1.543.94-3.31-.826-2.37-2.37a1.724 1.724 0 00-1.065-2.572c-1.756-.426-1.756-2.924 0-3.35a1.724 1.724 0 001.066-2.573c-.94-1.543.826-3.31 2.37-2.37.996.608 2.296.07 2.572-1.065z"></path><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 12a3 3 0 11-6 0 3 3 0 016 0z"></path></svg>
                        Settings
                    </a>
                </li>
            </ul>
        </nav>
        <div class="absolute bottom-4 left-4 text-gray-400 text-sm">
            Logged in as: <br><span class="font-medium">{{.UserEmail}}</span>
        </div>
    </aside>

    <!-- Main Content Area -->
    <main class="flex-grow p-8">
        <div class="bg-white rounded-lg shadow-md p-6">
            {{template "content" .}}
        </div>
    </main>
</body>
</html>
{{end}}

<!-- --- recap-server/templates/admin_dashboard.html --- -->
{{define "content"}}
<h2 class="text-3xl font-bold text-gray-800 mb-6">Dashboard</h2>

<div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-6 mb-8">
    <div class="bg-blue-100 p-6 rounded-lg shadow-sm">
        <div class="text-blue-700 font-semibold text-lg">Total Verified Users</div>
        <div class="text-blue-900 text-4xl font-bold">{{.TotalVerifiedUsers}}</div>
    </div>
    <div class="bg-green-100 p-6 rounded-lg shadow-sm">
        <div class="text-green-700 font-semibold text-lg">Total Exams Taken</div>
        <div class="text-green-900 text-4xl font-bold">{{.TotalExamsTaken}}</div>
    </div>
    <div class="bg-red-100 p-6 rounded-lg shadow-sm">
        <div class="text-red-700 font-semibold text-lg">CSV Validation Failures</div>
        <div class="text-red-900 text-4xl font-bold">{{.ValidationFailures}}</div>
    </div>
</div>

<h3 class="text-2xl font-bold text-gray-800 mb-4">Recent Activity</h3>

<div class="grid grid-cols-1 lg:grid-cols-2 gap-6">
    <!-- Recent Admin Events -->
    <div class="bg-gray-50 p-6 rounded-lg shadow-sm">
        <h4 class="text-xl font-semibold text-gray-700 mb-4">Latest Admin Events</h4>
        <ul class="space-y-3">
            {{if .RecentAdminEvents}}
            {{range .RecentAdminEvents}}
            <li class="p-3 bg-white rounded-md shadow-sm border border-gray-200">
                <p class="text-sm text-gray-500">{{.Timestamp.Format "2006-01-02 15:04:05"}}</p>
                <p class="font-medium text-gray-800">{{.Actor}} <span class="text-gray-600">- {{.Action}} on {{.Target}}</span></p>
                <p class="text-gray-700 text-sm">{{.Notes}}</p>
            </li>
            {{end}}
            {{else}}
            <p class="text-gray-600">No recent admin events.</p>
            {{end}}
        </ul>
    </div>

    <!-- Recently Ingested Courses -->
    <div class="bg-gray-50 p-6 rounded-lg shadow-sm">
        <h4 class="text-xl font-semibold text-gray-700 mb-4">Recently Ingested Courses</h4>
        <ul class="space-y-3">
            {{if .RecentCourses}}
            {{range .RecentCourses}}
            <li class="p-3 bg-white rounded-md shadow-sm border border-gray-200">
                <p class="font-medium text-gray-800">{{.MarketingName}} <span class="text-gray-600">({{.CourseCode}})</span></p>
                <p class="text-sm text-gray-700">ID: {{.ID}}, Responsibility: {{.Responsibility}}</p>
            </li>
            {{end}}
            {{else}}
            <p class="text-gray-600">No recently ingested courses.</p>
            {{end}}
        </ul>
    </div>
</div>
{{end}}


<!-- --- recap-server/config.yaml.example --- -->
# Example configuration file for RECAP Go Backend
# Copy this to config.yaml and adjust values as needed.

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

