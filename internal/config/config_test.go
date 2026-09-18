package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaultsAndEnvFile(t *testing.T) {
	// изолируем среду
	clear := []string{"YD_TOKEN", "YD_ROOT", "QUEUE_DIR", "STATE_FILE", "POLL_INTERVAL",
		"RETENTION_DAYS", "CLAIM_TIMEOUT", "LLM_BASE_URL", "LLM_API_KEY", "LLM_MODEL",
		"LLM_SYSTEM", "LLM_TIMEOUT"}
	for _, k := range clear {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	os.WriteFile(envFile, []byte(`# тест
YD_TOKEN=tok-123
YD_ROOT="/echo"
POLL_INTERVAL=30
RETENTION_DAYS=14
LLM_API_KEY=sk-x
`), 0o600)

	cfg, err := Load(envFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.YDToken != "tok-123" || cfg.YDRoot != "/echo" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.PollInterval != 30*time.Second || cfg.RetentionDays != 14 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.QueueDir != "/opt/echo-bot/queue" || cfg.ClaimTimeout != 1800*time.Second {
		t.Fatalf("умолчания не применились: %+v", cfg)
	}
	if cfg.LLMBaseURL != "https://api.neuraldeep.ru/v1" || cfg.LLMAPIKey != "sk-x" {
		t.Fatalf("LLM cfg = %+v", cfg)
	}
}

func TestEnvBeatsEnvFile(t *testing.T) {
	for _, k := range []string{"YD_ROOT", "POLL_INTERVAL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	os.WriteFile(envFile, []byte("YD_ROOT=/from-file\nPOLL_INTERVAL=99\n"), 0o600)

	t.Setenv("YD_ROOT", "/from-env")
	cfg, err := Load(envFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.YDRoot != "/from-env" {
		t.Fatalf("окружение должно быть приоритетнее .env: %q", cfg.YDRoot)
	}
	if cfg.PollInterval != 99*time.Second {
		t.Fatalf("POLL_INTERVAL=%v, want 99s", cfg.PollInterval)
	}
}

func TestBadNumber(t *testing.T) {
	t.Setenv("POLL_INTERVAL", "не-число")
	if _, err := Load(filepath.Join(t.TempDir(), "missing.env")); err == nil {
		t.Fatal("ожидалась ошибка на нечисловой POLL_INTERVAL")
	}
}
