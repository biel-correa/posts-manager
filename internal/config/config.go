package config

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port                  string
	MongoURI              string
	MongoDBName           string
	GeminiAPIKey          string
	GeminiTextModel       string
	GeminiImageModel      string
	GeminiSearchGrounding bool
	GeminiImageGeneration bool
	Timezone              string
	Location              *time.Location
	PostsPerWeek          int
	SundayCron            string
	WebhookURL            string
}

func Load() *Config {
	tz := envOrDefault("TZ", "America/Sao_Paulo")
	loc, err := time.LoadLocation(tz)
	if err != nil {
		slog.Warn("invalid TZ environment variable, falling back to UTC", "tz", tz, "error", err)
		tz = "UTC"
		loc = time.UTC
	}

	postsPerWeek, err := strconv.Atoi(envOrDefault("POSTS_PER_WEEK", "3"))
	if err != nil || postsPerWeek < 1 {
		postsPerWeek = 3
	}

	return &Config{
		Port:                  envOrDefault("PORT", "8080"),
		MongoURI:              envOrDefault("MONGO_URI", "mongodb://localhost:27017"),
		MongoDBName:           envOrDefault("MONGO_DB", "linkedin_post_manager"),
		GeminiAPIKey:          strings.TrimSpace(os.Getenv("GEMINI_API_KEY")),
		GeminiTextModel:       envOrDefault("GEMINI_TEXT_MODEL", "gemini-3.5-flash-lite"),
		GeminiImageModel:      envOrDefault("GEMINI_IMAGE_MODEL", "gemini-nano-banana-2.1"),
		GeminiSearchGrounding: envOrDefaultBool("GEMINI_SEARCH_GROUNDING", false),
		GeminiImageGeneration: envOrDefaultBool("GEMINI_IMAGE_GENERATION", false),
		Timezone:              tz,
		Location:              loc,
		PostsPerWeek:          postsPerWeek,
		SundayCron:            envOrDefault("SUNDAY_CRON", "0 8 * * 0"),
		WebhookURL:            strings.TrimSpace(os.Getenv("WEBHOOK_URL")),
	}
}

func envOrDefault(key, fallback string) string {
	if val := strings.TrimSpace(os.Getenv(key)); val != "" {
		return val
	}
	return fallback
}

func envOrDefaultBool(key string, fallback bool) bool {
	if val := strings.TrimSpace(os.Getenv(key)); val != "" {
		if b, err := strconv.ParseBool(val); err == nil {
			return b
		}
	}
	return fallback
}
