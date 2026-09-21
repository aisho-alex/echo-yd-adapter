// yd-adapter — серверный адаптер файлового транспорта echo-бота.
// Транспорт — Яндекс.Диск (/echo/{in,out,archive}), приёмник — локальная
// очередь /opt/echo-bot/queue, которую читают воркеры Hermes без изменений.
// Протокол: docs/PROTOCOL.md.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"yd-adapter/internal/bridge"
	"yd-adapter/internal/chatllm"
	"yd-adapter/internal/config"
	"yd-adapter/internal/notify"
	"yd-adapter/internal/reclaim"
	"yd-adapter/internal/retention"
	"yd-adapter/ydisk"
)

const apiBase = "https://cloud-api.yandex.net/v1/disk"

func main() {
	once := flag.Bool("once", false, "один проход цикла и выход (для теста)")
	dryRun := flag.Bool("dry-run", false, "ничего не двигать: показать состояние каналов")
	check := flag.Bool("check", false, "проверить токен, доступ к папкам и очередь")
	envFile := flag.String("env", ".env", "файл конфигурации")
	flag.Parse()

	setupLog()

	cfg, err := config.Load(*envFile)
	if err != nil {
		log.Fatalf("ERROR: конфиг: %v", err)
	}
	client := ydisk.NewYdClient(cfg.YDToken, apiBase, nil)
	st := bridge.NewState(cfg.StateFile)
	if err := st.Load(); err != nil {
		log.Fatalf("ERROR: state: %v", err)
	}
	p := &bridge.Puller{Client: client, Root: cfg.YDRoot, QueueDir: cfg.QueueDir}
	if cfg.LLMAPIKey != "" {
		p.Chat = chatllm.New(chatllm.Config{
			BaseURL: cfg.LLMBaseURL,
			APIKey:  cfg.LLMAPIKey,
			Model:   cfg.LLMModel,
			System:  cfg.LLMSystem,
			Timeout: cfg.LLMTimeout,
			Retries: 2,
		})
	} else {
		log.Printf("WARN: LLM_API_KEY не задан — chat-конверты будут получать отказ")
	}
	if cfg.NtfyTopic != "" {
		p.Ntfy = notify.New(cfg.NtfyURL, cfg.NtfyTopic)
		log.Printf("push: ntfy %s (топик задан)", cfg.NtfyURL)
	} else {
		log.Printf("push: выключен (NTFY_TOPIC не задан)")
	}
	rc := reclaim.New(cfg.ReclaimParkAfter)

	if *dryRun {
		printState(p, st)
		return
	}
	if *check {
		if code := runCheck(p, st, cfg); code != 0 {
			os.Exit(code)
		}
		return
	}

	cycle := func() {
		pulled, err := p.PullIn(st)
		if err != nil {
			log.Printf("ERROR: pull_in: %v", err)
		} else if pulled > 0 {
			log.Printf("pull_in: %d конвертов", pulled)
		}
		if pushed, err := p.PushOut(); err != nil {
			log.Printf("ERROR: push_out: %v", err)
		} else if pushed > 0 {
			log.Printf("push_out: %d результатов", pushed)
		}
		if rec, err := rc.Run(cfg.QueueDir, cfg.ClaimTimeout); err != nil {
			log.Printf("ERROR: reclaim: %v", err)
		} else if rec > 0 {
			log.Printf("reclaim: %d задач", rec)
		}
	}
	lastRetention := time.Time{}
	for {
		cycle()
		if time.Since(lastRetention) >= time.Hour {
			if n, err := retention.Run(client, cfg.YDRoot, cfg.RetentionDays, time.Now()); err != nil {
				log.Printf("ERROR: retention: %v", err)
			} else if n > 0 {
				log.Printf("retention: удалено %d", n)
			}
			lastRetention = time.Now()
		}
		if *once {
			return
		}
		time.Sleep(cfg.PollInterval)
	}
}

// runCheck — диагностика перед деплоем/по тревоге. Возвращает код выхода.
func runCheck(p *bridge.Puller, st *bridge.State, cfg config.Config) int {
	failed := false
	fail := func(format string, args ...any) {
		failed = true
		log.Printf("ERROR: "+format, args...)
	}
	ok := func(format string, args ...any) { log.Printf("check ok: "+format, args...) }

	if err := p.Client.Check(); err != nil {
		fail("токен/доступ к Диску: %v", err)
	} else {
		ok("токен Диска живой (%s)", apiBase)
	}
	for _, d := range []string{"in", "out", "archive/in", "archive/out", "archive/broken"} {
		if err := p.Client.EnsureDir(cfg.YDRoot + "/" + d); err != nil {
			fail("папка %s: %v", d, err)
		} else {
			ok("папка %s доступна", d)
		}
	}
	for _, d := range []string{"inbox", "claimed", "outbox", "done", "files", "parked"} {
		dir := filepath.Join(cfg.QueueDir, d)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fail("очередь %s: %v", d, err)
			continue
		}
		probe := filepath.Join(dir, fmt.Sprintf(".probe-%d", os.Getpid()))
		if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
			fail("очередь %s не пишется: %v", d, err)
		} else {
			os.Remove(probe)
			ok("очередь %s пишется", d)
		}
	}

	if out, err := exec.Command("systemctl", "is-active", "echo-bot").Output(); err == nil {
		if strings.TrimSpace(string(out)) == "active" {
			log.Printf("WARN: юнит echo-bot АКТИВЕН — одновременно с yd-adapter работать нельзя (R5)")
		} else {
			ok("echo-bot не активен (взаимоисключение соблюдено)")
		}
	}

	printState(p, st)
	if failed {
		return 1
	}
	log.Printf("check: всё в порядке")
	return 0
}

// printState — сводка каналов без изменений (используется и в --dry-run).
func printState(p *bridge.Puller, st *bridge.State) {
	count := func(dir string) string {
		items, err := p.Client.List(p.Root + "/" + dir)
		if err != nil {
			return "?"
		}
		return fmt.Sprintf("%d", len(items))
	}
	log.Printf("Диск %s: in=%s out=%s archive/in=%s archive/out=%s archive/broken=%s",
		p.Root, count("in"), count("out"), count("archive/in"), count("archive/out"), count("archive/broken"))
	qcount := func(dir string) string {
		entries, err := os.ReadDir(filepath.Join(p.QueueDir, dir))
		if err != nil {
			return "?"
		}
		return fmt.Sprintf("%d", len(entries))
	}
	log.Printf("Очередь %s: inbox=%s claimed=%s outbox=%s done=%s parked=%s",
		p.QueueDir, qcount("inbox"), qcount("claimed"), qcount("outbox"), qcount("done"), qcount("parked"))
	if p.Ntfy != nil {
		log.Printf("Push: ntfy %s (топик задан)", p.Ntfy.BaseURL)
	} else {
		log.Printf("Push: выключен")
	}
	log.Printf("Состояние: обработано id=%d, chat=%v", st.Count(), p.Chat != nil)
}

// setupLog приводит логи к формату «2006-01-02 15:04:05 LEVEL сообщение»
// (journald подхватывает stdout), а «WARN:»/«ERROR:» из пакетов — к уровням.
func setupLog() {
	log.SetFlags(0)
	log.SetOutput(levelWriter{os.Stdout})
}

type levelWriter struct{ w io.Writer }

func (lw levelWriter) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	level := "INFO"
	if after, found := strings.CutPrefix(line, "WARN: "); found {
		level, line = "WARN", after
	} else if after, found := strings.CutPrefix(line, "ERROR: "); found {
		level, line = "ERROR", after
	}
	out := fmt.Sprintf("%s %s %s\n", time.Now().Format("2006-01-02 15:04:05"), level, line)
	return lw.w.Write([]byte(out))
}
