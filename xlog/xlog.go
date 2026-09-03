// Package xlog provides the process-wide logger for pr-review-agent.
//
// Logs are structured (slog) and go to stderr by default in human-readable
// form. When a log file is configured, a machine-readable JSON copy is
// appended to it. Stdout is never used for logging so that streamed model
// output stays clean.
package xlog

import (
	"log/slog"
	"os"
)

var l *slog.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

// Setup configures the process-wide logger at the given level. When
// filePath is non-empty, every record is also appended to that file as JSON
// (the file is created if missing). The returned func closes the file and
// resets the logger to its default; call it on shutdown.
func Setup(level slog.Level, filePath string) (func(), error) {
	opts := &slog.HandlerOptions{Level: level}

	handlers := []slog.Handler{slog.NewTextHandler(os.Stderr, opts)}

	var file *os.File
	if filePath != "" {
		var err error
		file, err = os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		handlers = append(handlers, slog.NewJSONHandler(file, opts))
	}

	l = slog.New(slog.NewMultiHandler(handlers...))

	return func() {
		if file != nil {
			file.Close()
		}
		l = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}, nil
}

// Logger returns the process-wide logger.
func Logger() *slog.Logger { return l }

// Info logs at Info level.
func Info(msg string, args ...any) { l.Info(msg, args...) }

// Debug logs at Debug level.
func Debug(msg string, args ...any) { l.Debug(msg, args...) }

// Warn logs at Warn level.
func Warn(msg string, args ...any) { l.Warn(msg, args...) }

// Error logs at Error level.
func Error(msg string, args ...any) { l.Error(msg, args...) }
