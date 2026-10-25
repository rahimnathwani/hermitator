package hostops

import (
	"strings"
	"testing"
)

func TestFormatLogs(t *testing.T) {
	raw := `{"time":"2026-10-24T21:39:18.103-07:00","level":"INFO","msg":"hermitator listening"}
{"time":"2026-10-24T21:39:19.000-07:00","level":"WARN","msg":"send reply; will try again","chat":"x@s.whatsapp.net","err":"not connected"}
Stopping hermitator.service...
{"time":"2026-10-24T21:40:00.000-07:00","level":"ERROR","msg":"route lookup","err":"database is locked\nretry"}
`
	got := FormatLogs(raw, 10)
	want := "10-25 04:39:19 WARN send reply; will try again chat=x@s.whatsapp.net err=not connected\n" +
		"Stopping hermitator.service...\n" +
		"10-25 04:40:00 ERROR route lookup err=database is locked retry"
	if got != want {
		t.Fatalf("FormatLogs =\n%s\nwant\n%s", got, want)
	}
	if got := FormatLogs(raw, 1); strings.Count(got, "\n") != 0 || !strings.Contains(got, "route lookup") {
		t.Fatalf("last 1 = %q", got)
	}
	if got := FormatLogs(`{"level":"INFO","msg":"fine"}`, 5); !strings.HasPrefix(got, "No warnings") {
		t.Fatalf("quiet log = %q", got)
	}
}
