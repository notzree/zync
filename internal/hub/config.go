package hub

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	Port    int
	DataDir string
	Token   string
	GitBin  string
}

func (c Config) ReposDir() string { return filepath.Join(c.DataDir, "repos") }
func (c Config) DBPath() string   { return filepath.Join(c.DataDir, "zync.db") }

func NewConfig() (Config, error) {
	cfg := Config{
		Port:    8080,
		DataDir: "/data",
		GitBin:  "git",
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
	cfg.Token = os.Getenv("ZYNC_TOKEN")
	if cfg.Token == "" {
		return cfg, errors.New("ZYNC_TOKEN is required")
	}
	if err := os.MkdirAll(cfg.ReposDir(), 0o755); err != nil {
		return cfg, err
	}
	return cfg, nil
}
