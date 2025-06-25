
package middleware
import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"errors"
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
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authorization header required"})
			return
		}
		parts := strings.SplitN(authHeader, " ", 2)
		if !(len(parts) == 2 && strings.ToLower(parts[0]) == "bearer") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authorization header format must be Bearer {token}"})
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
			// FIXED: Use errors.Is for robust JWT error checking (correct and consistent with jwt/v5)
			if errors.Is(err, jwt.ErrSignatureInvalid) {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid token signature"})
				return
			}
			if errors.Is(err, jwt.ErrTokenExpired) {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Token expired"})
				return
			}
			if errors.Is(err, jwt.ErrTokenNotValidYet) {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Token not active yet"})
				return
			}
			// Fallback for any other parsing errors
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
			return
		}
		if claims, ok := token.Claims.(*claims); ok && token.Valid {
			// Validate issuer (optional, but good practice if FIRM provides it)
			if claims.Issuer != issuer {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid token issuer"})
				return
			}
			// Validate expiration (redundant with jwt.ParseWithClaims but good for explicit check)
			if claims.ExpiresAt == nil || claims.ExpiresAt.Before(time.Now()) {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Token expired"})
				return
			}
			c.Set("user_email", claims.Email)
			c.Set("user_roles", claims.Roles) // Pass roles to context for RBAC
			c.Next()
		} else {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid token claims"})
			return
		}
	}
}
// RoleCheckMiddleware checks if the user has one of the required roles.
func RoleCheckMiddleware(requiredRoles []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		userRoles, exists := c.Get("user_roles")
		if !exists {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "User roles not found in context"})
			return
		}
		roles, ok := userRoles.([]string)
		if !ok {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Invalid user roles format"})
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
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Insufficient permissions"})
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
