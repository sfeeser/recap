
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
	_ "github.com/jackc/pgx/v5/pgxpool" // USED: Required for db.InitDB to initialize the pgxpool.Pool type
	_ "github.com/spf13/viper"         // USED: Required for config.LoadConfig() to unmarshal configuration
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
