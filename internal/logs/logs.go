package logs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Level string

const (
	LevelInfo  Level = "INFO"
	LevelError Level = "ERROR"
	LevelDebug Level = "DEBUG"
)

type Entry struct {
	Time    time.Time `json:"time"`
	Level   Level     `json:"level"`
	Source  string    `json:"source"`
	Message string    `json:"message"`
}

type Logger struct {
	mu        sync.Mutex
	entries   []Entry
	max       int
	listeners map[chan Entry]struct{}
	fileMutex sync.Mutex
}

func NewLogger(max int) *Logger {
	if max <= 0 {
		max = 500
	}
	return &Logger{
		entries:   make([]Entry, 0, max),
		max:       max,
		listeners: make(map[chan Entry]struct{}),
	}
}

func (l *Logger) log(level Level, source, msg string) {
	now := time.Now().UTC()
	entry := Entry{
		Time:    now,
		Level:   level,
		Source:  source,
		Message: msg,
	}

	// 1. Output to stdout (Terminal)
	localTime := time.Now().Format("2006/01/02 15:04:05")
	fmt.Printf("[%s] [%s] [%s] %s\n", localTime, level, source, msg)

	// 2. Append to file if it's an order log, but skip action=test or [TEST] tags
	if source == "order" || source == "signal" {
		if !strings.Contains(msg, "action=test") && !strings.Contains(msg, "[TEST]") {
			l.appendToFile("order.log", fmt.Sprintf("[%s] [%s] [%s] %s\n", localTime, level, source, msg))
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.entries) >= l.max {
		copy(l.entries[0:], l.entries[1:])
		l.entries[len(l.entries)-1] = entry
	} else {
		l.entries = append(l.entries, entry)
	}

	for ch := range l.listeners {
		select {
		case ch <- entry:
		default:
		}
	}
}

func (l *Logger) appendToFile(filename, line string) {
	l.fileMutex.Lock()
	defer l.fileMutex.Unlock()

	// Ensure the directory exists if we want to put it in a specific folder.
	// For now, we put it in the current working directory.
	dir := "data"
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}

	filePath := filepath.Join(dir, filename)
	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	if _, err := f.WriteString(line); err != nil {
		return
	}
}

func (l *Logger) Info(source, msg string) {
	l.log(LevelInfo, source, msg)
}

func (l *Logger) Error(source, msg string) {
	l.log(LevelError, source, msg)
}

func (l *Logger) Debug(source, msg string) {
	l.log(LevelDebug, source, msg)
}

func (l *Logger) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]Entry, len(l.entries))
	copy(out, l.entries)
	return out
}

func (l *Logger) AddListener() (chan Entry, func()) {
	ch := make(chan Entry, 100)

	l.mu.Lock()
	l.listeners[ch] = struct{}{}
	l.mu.Unlock()

	cancel := func() {
		l.mu.Lock()
		if _, ok := l.listeners[ch]; ok {
			delete(l.listeners, ch)
			close(ch)
		}
		l.mu.Unlock()
	}

	return ch, cancel
}

func (e Entry) JSON() []byte {
	b, _ := json.Marshal(e)
	return b
}
