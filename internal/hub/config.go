package hub

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Config struct {
	Port          int
	DataDir       string
	Token         string
	GitBin        string
	GitGCInterval time.Duration
	GitGCPruneAge time.Duration
	LeaseTTL      time.Duration
}

func (c Config) ReposDir() string { return filepath.Join(c.DataDir, "repos") }
func (c Config) BlobsDir() string { return filepath.Join(c.DataDir, "blobs") }
func (c Config) DBPath() string   { return filepath.Join(c.DataDir, "zync.db") }

func NewConfig() (Config, error) {
	cfg := Config{
		Port:          8080,
		DataDir:       "/data",
		GitBin:        "git",
		GitGCInterval: 24 * time.Hour,
		GitGCPruneAge: 14 * 24 * time.Hour,
		LeaseTTL:      2 * time.Minute,
	}
	if v := os.Getenv("ZYNC_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return cfg, errors.New("ZYNC_PORT must be an integer")
		}
		cfg.Port = p
	}
	if v := os.Getenv("ZYNC_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("ZYNC_GIT_BIN"); v != "" {
		cfg.GitBin = v
	}
	if v := os.Getenv("ZYNC_GIT_GC_INTERVAL"); v != "" {
		interval, err := time.ParseDuration(v)
		if err != nil || interval < 0 {
			return cfg, fmt.Errorf("ZYNC_GIT_GC_INTERVAL must be a non-negative duration")
		}
		cfg.GitGCInterval = interval
	}
	if v := os.Getenv("ZYNC_GIT_GC_PRUNE_AGE"); v != "" {
		age, err := time.ParseDuration(v)
		if err != nil || age < 24*time.Hour {
			return cfg, fmt.Errorf("ZYNC_GIT_GC_PRUNE_AGE must be a duration of at least 24h")
		}
		cfg.GitGCPruneAge = age
	}
	if v := os.Getenv("ZYNC_LEASE_TTL"); v != "" {
		ttl, err := time.ParseDuration(v)
		if err != nil || ttl < 0 || (ttl > 0 && ttl < time.Second) {
			return cfg, fmt.Errorf("ZYNC_LEASE_TTL must be 0 or a duration of at least 1s")
		}
		cfg.LeaseTTL = ttl
	}
	cfg.Token = os.Getenv("ZYNC_TOKEN")
	if cfg.Token == "" {
		return cfg, errors.New("ZYNC_TOKEN is required")
	}
	if err := os.MkdirAll(cfg.ReposDir(), 0o755); err != nil {
		return cfg, err
	}
	return cfg, nil
}
