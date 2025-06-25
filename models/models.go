
package models
import (
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
	ExamQuestionID  int     `json:"exam_question_id,omitempty"` // ADDED: Field for API response for specific exam questions
	// For API responses, might also contain choices/acceptable answers
	Choices          []Choice `json:"choices,omitempty"`
	AcceptableAnswers []string `json:"acceptable_answers,omitempty"`
    QuestionDomainName string `json:"question_domain_name"` // Used internally for exam generation
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
// ExamPlan struct is used by the exam generation logic to define the structure of exams.
type ExamPlan struct {
	NumExams         int
	QuestionsPerExam int
	PerDomainPerExam map[string]int
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
	ExamTime        int                  `json:"time_limit_minutes"` // Renamed from exam_time to match API
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
