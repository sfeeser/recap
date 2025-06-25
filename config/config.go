
package config
import (
	"fmt"
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
