package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	DatabaseURL         string
	HMACSecret          string
	Concurrency         int
	MaxAttempts         int
	DeliveryTimeout     time.Duration
	ShutdownGracePeriod time.Duration
	Addr                string
}

func Load() Config {
	return Config{
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		HMACSecret:          os.Getenv("HMAC_SECRET"),
		Concurrency:         envInt("WORKER_CONCURRENCY", 10),
		MaxAttempts:         envInt("MAX_ATTEMPTS", 5),
		DeliveryTimeout:     envDuration("DELIVERY_TIMEOUT", 10*time.Second),
		ShutdownGracePeriod: envDuration("SHUTDOWN_GRACE_PERIOD", 15*time.Second),
		Addr:                envString("ADDR", ":8080"),
	}
}

func envString(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
