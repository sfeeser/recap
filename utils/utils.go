
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
