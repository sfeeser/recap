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
