package telemetry

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"

	apilog "go.opentelemetry.io/otel/log"
)

type stdLogWriter struct {
	scope string
	emit  func(context.Context, string, apilog.Severity, string)

	mu  sync.Mutex
	buf bytes.Buffer
}

func NewStdLogWriter(scope string) io.Writer {
	return &stdLogWriter{
		scope: scope,
		emit:  emitLogRecord,
	}
}

func (w *stdLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if _, err := w.buf.Write(p); err != nil {
		return 0, err
	}
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			if len(line) > 0 {
				_, _ = w.buf.WriteString(line)
			}
			break
		}
		w.emitLine(strings.TrimSpace(line))
	}
	return len(p), nil
}

func (w *stdLogWriter) emitLine(line string) {
	if line == "" {
		return
	}
	w.emit(context.Background(), w.scope, severityForLine(line), line)
}

func severityForLine(line string) apilog.Severity {
	lower := strings.ToLower(line)
	switch {
	case strings.Contains(lower, "panic"), strings.Contains(lower, "fatal"):
		return apilog.SeverityFatal
	case strings.Contains(lower, "error"), strings.Contains(lower, "failed"):
		return apilog.SeverityError
	case strings.Contains(lower, "warn"):
		return apilog.SeverityWarn
	default:
		return apilog.SeverityInfo
	}
}
