// Package config — конфигурация адаптера: значения из .env-файла и окружения
// (окружение приоритетнее: systemd EnvironmentFile уже экспортирует переменные).
package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	YDToken  string
	YDRoot   string
	QueueDir string

	PollInterval  time.Duration
	RetentionDays int
	ClaimTimeout  time.Duration
	StateFile     string

	// ReclaimParkAfter — сколько раз задачу можно вернуть в inbox, прежде чем
	// запарковать (0 — не парковать; защита от прогонов по кругу).
	ReclaimParkAfter int

	LLMBaseURL string
	LLMAPIKey  string
	LLMModel   string
	LLMSystem  string
	LLMTimeout time.Duration

	NtfyURL   string
	NtfyTopic string // пусто — push выключен
}

// Load читает envFile (если существует), затем значения из окружения,
// затем подставляет умолчания. Ошибка — только на нечитаемый файл или
// нечисловое значение длительности/дней.
func Load(envFile string) (Config, error) {
	if err := loadEnvFile(envFile); err != nil && !os.IsNotExist(err) {
		return Config{}, err
	}
	cfg := Config{
		YDToken:       os.Getenv("YD_TOKEN"),
		YDRoot:        orDefault(os.Getenv("YD_ROOT"), "/echo"),
		QueueDir:      orDefault(os.Getenv("QUEUE_DIR"), "/opt/echo-bot/queue"),
		StateFile:     orDefault(os.Getenv("STATE_FILE"), "/opt/echo-bot/yd-adapter/state.json"),
		LLMBaseURL:    orDefault(os.Getenv("LLM_BASE_URL"), "https://api.neuraldeep.ru/v1"),
		LLMAPIKey:     os.Getenv("LLM_API_KEY"),
		LLMModel:      orDefault(os.Getenv("LLM_MODEL"), "gpt-oss-120b"),
		LLMSystem:     os.Getenv("LLM_SYSTEM"),
		PollInterval:  10 * time.Second,
		RetentionDays: 30,
		ClaimTimeout:  1800 * time.Second,

		ReclaimParkAfter: 3,
		LLMTimeout:       120 * time.Second,
		NtfyURL:          orDefault(os.Getenv("NTFY_URL"), "https://ntfy.sh"),
		NtfyTopic:        os.Getenv("NTFY_TOPIC"),
	}
	var err error
	if v := os.Getenv("POLL_INTERVAL"); v != "" {
		if cfg.PollInterval, err = parseSeconds(v); err != nil {
			return cfg, keyErr("POLL_INTERVAL", err)
		}
	}
	if v := os.Getenv("RETENTION_DAYS"); v != "" {
		if cfg.RetentionDays, err = strconv.Atoi(v); err != nil {
			return cfg, keyErr("RETENTION_DAYS", err)
		}
	}
	if v := os.Getenv("CLAIM_TIMEOUT"); v != "" {
		if cfg.ClaimTimeout, err = parseSeconds(v); err != nil {
			return cfg, keyErr("CLAIM_TIMEOUT", err)
		}
	}
	if v := os.Getenv("RECLAIM_PARK_AFTER"); v != "" {
		if cfg.ReclaimParkAfter, err = strconv.Atoi(v); err != nil {
			return cfg, keyErr("RECLAIM_PARK_AFTER", err)
		}
	}
	if v := os.Getenv("LLM_TIMEOUT"); v != "" {
		if cfg.LLMTimeout, err = parseSeconds(v); err != nil {
			return cfg, keyErr("LLM_TIMEOUT", err)
		}
	}
	return cfg, nil
}

type keyError struct {
	key string
	err error
}

func (e *keyError) Error() string { return "config: " + e.key + ": " + e.err.Error() }
func (e *keyError) Unwrap() error { return e.err }

func keyErr(key string, err error) error { return &keyError{key: key, err: err} }

func parseSeconds(v string) (time.Duration, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, err
	}
	return time.Duration(n) * time.Second, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// loadEnvFile — минимальный парсер .env: KEY=VALUE, комментарии #, кавычки
// снимаются, значения НЕ перезаписывают уже установленные переменные среды.
func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' ||
			val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
	return sc.Err()
}
