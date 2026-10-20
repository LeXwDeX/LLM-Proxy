package logging

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Config drives the logger initialization.
type Config struct {
	Level         string
	AccessLogPath string
	ErrorLogPath  string

	// Optional rotation settings. Zero values fall back to sensible defaults.
	MaxSizeMB   int
	MaxBackups  int
	MaxAgeDays  int
	CompressOld bool
}

// Manager keeps references to the configured loggers for reuse.
type Manager struct {
	appLogger    *slog.Logger
	accessLogger *slog.Logger
	closers      []io.Closer
}

// Setup initializes logging outputs based on configuration.
func Setup(cfg Config) (*Manager, error) {
	if strings.TrimSpace(cfg.AccessLogPath) == "" {
		return nil, errors.New("logging: access log path must not be empty")
	}
	if strings.TrimSpace(cfg.ErrorLogPath) == "" {
		return nil, errors.New("logging: error log path must not be empty")
	}

	errorWriter, closerErr, err := newRollingWriter(cfg.ErrorLogPath, cfg)
	if err != nil {
		return nil, fmt.Errorf("logging: init error log writer: %w", err)
	}

	accessWriter, closerAccess, err := newRollingWriter(cfg.AccessLogPath, cfg)
	if err != nil {
		_ = closerErr.Close()
		return nil, fmt.Errorf("logging: init access log writer: %w", err)
	}

	level := parseLevel(cfg.Level)
	var levelVar slog.LevelVar
	levelVar.Set(level)

	appHandler := slog.NewTextHandler(
		io.MultiWriter(os.Stdout, errorWriter),
		&slog.HandlerOptions{Level: &levelVar, AddSource: false},
	)

	accessHandler := slog.NewTextHandler(
		io.MultiWriter(os.Stdout, accessWriter),
		&slog.HandlerOptions{Level: slog.LevelInfo, AddSource: false},
	)

	appLogger := slog.New(appHandler)
	accessLogger := slog.New(accessHandler)
	slog.SetDefault(appLogger)

	return &Manager{
		appLogger:    appLogger,
		accessLogger: accessLogger,
		closers:      []io.Closer{closerErr, closerAccess},
	}, nil
}

// App returns the main application logger.
func (m *Manager) App() *slog.Logger {
	return m.appLogger
}

// Access returns the dedicated access logger.
func (m *Manager) Access() *slog.Logger {
	return m.accessLogger
}

// Close closes the underlying log writers.
func (m *Manager) Close() error {
	var firstErr error
	for _, closer := range m.closers {
		if closer == nil {
			continue
		}
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func newRollingWriter(path string, cfg Config) (io.WriteCloser, io.Closer, error) {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, err
		}
		if info, err := os.Stat(dir); err != nil {
			return nil, nil, err
		} else if !info.IsDir() {
			return nil, nil, fmt.Errorf("log directory is not a directory: %s", dir)
		}
	}

	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return nil, nil, fmt.Errorf("log path must be a file, got directory: %s", path)
	}

	if file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err != nil {
		return nil, nil, err
	} else if err := file.Close(); err != nil {
		return nil, nil, err
	}

	maxSize := cfg.MaxSizeMB
	if maxSize <= 0 {
		maxSize = 20
	}
	maxBackups := cfg.MaxBackups
	if maxBackups < 0 {
		maxBackups = 0
	}
	if maxBackups == 0 {
		maxBackups = 5
	}
	maxAge := cfg.MaxAgeDays
	if maxAge < 0 {
		maxAge = 0
	}
	if maxAge == 0 {
		maxAge = 7
	}

	rolling := &lumberjack.Logger{
		Filename:   path,
		MaxSize:    maxSize,
		MaxBackups: maxBackups,
		MaxAge:     maxAge,
		Compress:   cfg.CompressOld,
	}

	return rolling, rolling, nil
}
