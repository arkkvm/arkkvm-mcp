// Package logger writes structured log lines to stderr and an optional file.
// stdout is reserved for the MCP JSON-RPC transport, so logging must never
// break or block on it: every write error is swallowed.
package logger

import (
	"fmt"
	"os"
	"sync"
	"time"
)

type Level int

const (
	LevelError Level = iota
	LevelWarn
	LevelInfo
	LevelDebug
)

func (l Level) String() string {
	switch l {
	case LevelError:
		return "ERROR"
	case LevelWarn:
		return "WARN"
	case LevelInfo:
		return "INFO"
	case LevelDebug:
		return "DEBUG"
	}
	return "INFO"
}

// ParseLevel mirrors normalizeLevel: only the four exact names are valid.
func ParseLevel(value string) (Level, bool) {
	switch value {
	case "error":
		return LevelError, true
	case "warn":
		return LevelWarn, true
	case "info":
		return LevelInfo, true
	case "debug":
		return LevelDebug, true
	}
	return LevelInfo, false
}

// Logger is the process-wide singleton: level from ARKKVM_LOG_LEVEL
// (default info), optional append-only file from ARKKVM_LOG_FILE.
type Logger struct {
	level Level
	file  *os.File
}

// New returns a logger writing to stderr (and appending to filePath when
// non-empty). A file that cannot be opened is ignored: a log-file error must
// not break the MCP server.
func New(levelName, filePath string) *Logger {
	level, ok := ParseLevel(levelName)
	if !ok {
		level = LevelInfo
	}
	l := &Logger{level: level}
	if filePath != "" {
		if f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			l.file = f
		}
	}
	return l
}

// NewFromEnv builds the process logger from the two ARKKVM_LOG_* variables.
func NewFromEnv() *Logger {
	return New(os.Getenv("ARKKVM_LOG_LEVEL"), os.Getenv("ARKKVM_LOG_FILE"))
}

var writeMu sync.Mutex

// FormatLine renders one log line: [ISO8601Z] [LEVEL] message {"json":...}.
func FormatLine(at time.Time, level Level, message string, contextJSON string) string {
	line := fmt.Sprintf("[%s] [%s] %s", at.UTC().Format("2006-01-02T15:04:05.000Z07:00"), level, message)
	if contextJSON != "" {
		line += " " + contextJSON
	}
	return line
}

func (l *Logger) log(level Level, message string, context any) {
	if level > l.level {
		return
	}
	line := FormatLine(time.Now(), level, message, marshalContext(context))
	writeMu.Lock()
	defer writeMu.Unlock()
	// Logging must never break the MCP server when stderr is unavailable.
	fmt.Fprintln(os.Stderr, line)
	if l.file != nil {
		fmt.Fprintln(l.file, line)
	}
}

func marshalContext(context any) string {
	if context == nil {
		return ""
	}
	data, err := jsonMarshal(context)
	if err != nil {
		// Fallback for unserializable context values.
		return `{"context":"[unserializable]"}`
	}
	return string(data)
}

func (l *Logger) Error(message string, err error, context map[string]any) {
	if err == nil {
		l.log(LevelError, message, context)
		return
	}
	merged := make(map[string]any, len(context)+1)
	for k, v := range context {
		merged[k] = v
	}
	merged["error"] = map[string]any{
		"name":    errorName(err),
		"message": err.Error(),
	}
	l.log(LevelError, message, merged)
}

func (l *Logger) Warn(message string, context any) { l.log(LevelWarn, message, context) }

func (l *Logger) Info(message string, context any) { l.log(LevelInfo, message, context) }

func (l *Logger) Debug(message string, context any) { l.log(LevelDebug, message, context) }

// L is the process-wide logger, seeded from the environment at init.
var L = NewFromEnv()

// Package-level convenience wrappers over L.
func Error(message string, err error, context map[string]any) { L.Error(message, err, context) }

func Warn(message string, context any) { L.Warn(message, context) }

func Info(message string, context any) { L.Info(message, context) }

func Debug(message string, context any) { L.Debug(message, context) }
