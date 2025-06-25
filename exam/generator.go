
package exam
import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"strconv"
	"strings"
	_ "time" // USED: For time.Now() in UpdateQuestionValidityScores
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
			db.LogError(pool, "exam_generation", courseMarketingName, "", 0, "", "Failed to insert exam", fmt.Sprintf("Exam: %s, Error: %v", examID, err))
			return fmt.Errorf("failed to insert exam %s: %w", examID, err)
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
			bestQuestionsPerExam := questionsUsedForThisQ
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
    log.Printf("Calculating validity for %d attempts...", len(allAttempts))
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
