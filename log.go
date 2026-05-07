package jisr

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

func formatLogAttrs(msg string, attrs ...slog.Attr) string {
	var b strings.Builder
	b.WriteString(msg)
	for _, attr := range attrs {
		appendLogAttr(&b, attr)
	}
	return b.String()
}

func appendLogAttr(b *strings.Builder, attr slog.Attr) {
	if attr.Key == "" {
		return
	}
	attr.Value = attr.Value.Resolve()
	if attr.Value.Kind() == slog.KindGroup {
		appendLogGroup(b, attr.Key, attr.Value.Group())
		return
	}
	b.WriteByte(' ')
	b.WriteString(attr.Key)
	b.WriteByte('=')
	b.WriteString(formatLogValue(attr.Value))
}

func appendLogGroup(b *strings.Builder, prefix string, attrs []slog.Attr) {
	for _, attr := range attrs {
		if attr.Key == "" {
			continue
		}
		attr.Key = prefix + "." + attr.Key
		appendLogAttr(b, attr)
	}
}

func formatLogValue(value slog.Value) string {
	switch value.Kind() {
	case slog.KindString:
		return quoteLogString(value.String())
	case slog.KindBool:
		return strconv.FormatBool(value.Bool())
	case slog.KindInt64:
		return strconv.FormatInt(value.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(value.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(value.Float64(), 'g', -1, 64)
	case slog.KindDuration:
		return value.Duration().String()
	case slog.KindTime:
		return quoteLogString(value.Time().Format("2006-01-02T15:04:05.999999999Z07:00"))
	case slog.KindAny:
		return formatAnyLogValue(value.Any())
	case slog.KindLogValuer:
		return formatLogValue(value.Resolve())
	default:
		return quoteLogString(value.String())
	}
}

func formatAnyLogValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "<nil>"
	case string:
		return quoteLogString(v)
	case fmt.Stringer:
		return quoteLogString(v.String())
	case error:
		return quoteLogString(v.Error())
	default:
		return quoteLogString(fmt.Sprint(v))
	}
}

func quoteLogString(s string) string {
	if s == "" {
		return `""`
	}
	if strings.IndexFunc(s, needsLogQuote) == -1 {
		return s
	}
	return strconv.Quote(s)
}

func needsLogQuote(r rune) bool {
	return r <= ' ' || r == '"' || r == '='
}
