package jisr

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFormatLogAttrs(t *testing.T) {
	err := errors.New("boom boom")
	ts := time.Date(2026, 5, 8, 1, 2, 3, 4, time.UTC)

	got := formatLogAttrs("auth decision",
		slog.String("path", "/v1/chat"),
		slog.String("result", "allowed"),
		slog.String("spaced", "hello world"),
		slog.String("quoted", `a"b`),
		slog.String("empty", ""),
		slog.Int("status", 200),
		slog.Int64("delta", -12),
		slog.Uint64("bytes", 42),
		slog.Bool("cached", true),
		slog.Duration("latency", 1500*time.Millisecond),
		slog.Time("time", ts),
		slog.Any("error", err),
		slog.Any("nil", nil),
		slog.Group("route",
			slog.String("cluster", "openai"),
			slog.Int("attempt", 2),
		),
		slog.Any("", "ignored"),
	)

	assert.Equal(t,
		`auth decision path=/v1/chat result=allowed spaced="hello world" quoted="a\"b" empty="" status=200 delta=-12 bytes=42 cached=true latency=1.5s time=2026-05-08T01:02:03.000000004Z error="boom boom" nil=<nil> route.cluster=openai route.attempt=2`,
		got,
	)
}

func TestQuoteLogString(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "simple", in: "hello", want: "hello"},
		{name: "empty", in: "", want: `""`},
		{name: "space", in: "hello world", want: `"hello world"`},
		{name: "quote", in: `a"b`, want: `"a\"b"`},
		{name: "equals", in: "a=b", want: `"a=b"`},
		{name: "newline", in: "a\nb", want: `"a\nb"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, quoteLogString(tt.in))
		})
	}
}
