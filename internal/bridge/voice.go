// Голосовые задачи (kind:agent + voice): расшифровка аудио через STT и
// постановка воркеру задачи с текстом транскрипта. Протокол — docs/PROTOCOL.md §4.
package bridge

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// handleVoice обрабатывает голосовую задачу (kind:agent + voice): скачивает
// ровно одно аудио-вложение, расшифровывает его через STT и ставит воркеру
// задачу с текстом транскрипта; аудио остаётся вложением задачи. Транскрипт
// публикуется отдельным конвертом (voice:true) — клиент показывает его в ленте.
func (p *Puller) handleVoice(name, id string, env Envelope) (handling, error) {
	// отказ публикует ok=false и архивирует исходник; сбой перекладки — не ошибка
	refuse := func(reason string) (handling, error) {
		envName, err := p.publishOutEnvelope(id, false, "voice", reason, nil)
		if err != nil {
			return handlingDeferred, err
		}
		if err := p.Client.Move(p.in(name), p.archive("in", name)); err != nil && !isNameTaken(err) {
			log.Printf("WARN: voice %s отвечен (%s), но конверт не переложен: %v", id, envName, err)
		}
		p.cleanupAtt(id)
		p.removeLocalFiles(id)
		log.Printf("voice %s: отказ (%s), конверт %s", id, reason, envName)
		return handlingReplied, nil
	}

	audio, ok := singleAudio(env.Attachments)
	if !ok {
		// конверт нарушает протокол: голосовой без единственного аудио-вложения
		log.Printf("WARN: voice-конверт %s без единственного аудио-вложения — в archive/broken", id)
		return handlingBroken, p.Client.Move(p.in(name), p.archive("broken", name))
	}
	if p.STT == nil {
		return refuse("распознавание речи на сервере не настроено")
	}

	localAtts, deferred, err := p.downloadAtts(id, env.Attachments)
	if err != nil {
		return handlingDeferred, err
	}
	if deferred {
		return handlingDeferred, nil
	}
	audioLocal := ""
	for _, a := range localAtts {
		if a.Name == audio.Name {
			audioLocal = filepath.Join(p.QueueDir, filepath.FromSlash(a.Path))
		}
	}
	text, err := p.STT.Transcribe(audioLocal)
	if err != nil {
		log.Printf("WARN: voice %s: STT не удалось: %v", id, err)
		return refuse("не удалось расшифровать аудио: " + err.Error())
	}

	// транскрипт — промежуточный конверт для ленты; сбой публикации не должен
	// терять саму задачу, поэтому только WARN
	if envName, err := p.publishOutEnvelopeEx(id, true, "stt", text, nil, true, false); err != nil {
		log.Printf("WARN: voice %s: транскрипт не опубликован: %v", id, err)
	} else {
		log.Printf("voice %s: транскрипт опубликован (%s)", id, envName)
	}

	if err := p.queueTask(id, env.Worker, text, env.Context, localAtts); err != nil {
		return handlingDeferred, err
	}
	if err := p.Client.Move(p.in(name), p.archive("in", name)); err != nil && !isNameTaken(err) {
		log.Printf("WARN: голосовая задача %s в inbox, но конверт не переложен: %v", id, err)
	}
	p.cleanupAtt(id)
	log.Printf("voice %s: расшифровано %d символов → inbox (worker=%s)", id, len([]rune(text)), env.Worker)
	return handlingQueued, nil
}

// singleAudio — ровно одно аудио-вложение в конверте.
func singleAudio(atts []Att) (Att, bool) {
	if len(atts) != 1 {
		return Att{}, false
	}
	if isAudio(atts[0]) {
		return atts[0], true
	}
	return Att{}, false
}

// isAudio — вложение похоже на аудио: mime audio/* либо известное расширение
// (клиент может не заполнить mime).
func isAudio(att Att) bool {
	if strings.HasPrefix(strings.ToLower(att.Mime), "audio/") {
		return true
	}
	switch strings.ToLower(filepath.Ext(att.Name)) {
	case ".m4a", ".mp3", ".wav", ".ogg", ".opus", ".aac", ".flac", ".webm":
		return true
	}
	return false
}

// removeLocalFiles убирает скачанные вложения задачи из очереди (отказ голосовой
// задачи): воркер их не увидит, а очередь не засоряется.
func (p *Puller) removeLocalFiles(id string) {
	if err := os.RemoveAll(filepath.Join(p.QueueDir, "files", id)); err != nil {
		log.Printf("WARN: не удалось убрать files/%s: %v", id, err)
	}
}
