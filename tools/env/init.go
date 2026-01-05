package env

import (
	"fmt"
	"os"

	"github.com/caarlos0/env/v11"
)

type config struct {
	TolgeeKey string `env:"TOLGEE_KEY" envDefault:""`
	// --- mensa-localizations: Redis ---
	RedisAddr string `env:"REDIS_ADDR" envDefault:"localhost:6379"`

	// --- mensa-localizations: MinIO/S3 fallback & versioning ---
	S3Enabled        bool   `env:"S3_ENABLED" envDefault:"true"`
	S3Bucket         string `env:"S3_BUCKET" envDefault:""`
	S3Region         string `env:"S3_REGION" envDefault:"us-east-1"`
	S3Endpoint       string `env:"S3_ENDPOINT" envDefault:""`
	S3AccessKey      string `env:"S3_ACCESS_KEY" envDefault:""`
	S3SecretKey      string `env:"S3_SECRET_KEY" envDefault:""`
	S3ForcePathStyle bool   `env:"S3_FORCE_PATH_STYLE" envDefault:"true"`
}

var cfg = config{}

func init() {
	if os.Getenv("DEBUG") == "true" {
		fmt.Println("DEBUG MODE ON | Getting env from .env file")
	}
	if err := env.Parse(&cfg); err != nil {
		fmt.Printf("%+v\n", err)
	}
}

// --- Nuovi getter usati da mensa-localizations/main.go ---
func GetRedisAddr() string { return cfg.RedisAddr }

func GetS3Enabled() bool { return cfg.S3Enabled }
func GetS3Bucket() string {
	return cfg.S3Bucket
}
func GetS3Region() string {
	return cfg.S3Region
}
func GetS3Endpoint() string {
	return cfg.S3Endpoint
}
func GetS3AccessKey() string {
	return cfg.S3AccessKey
}
func GetS3SecretKey() string {
	return cfg.S3SecretKey
}
func GetS3ForcePathStyle() bool {
	return cfg.S3ForcePathStyle
}
