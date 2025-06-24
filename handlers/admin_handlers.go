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