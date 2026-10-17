package media

import (
	"strings"
	"testing"
	"time"
)

func TestFileName(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	cases := []struct {
		orig, mime string
		seq        int
		want       string
	}{
		{"", "image/jpeg", 0, "20261017-093000.jpg"},
		{"", "audio/ogg; codecs=opus", 2, "20261017-093000-2.ogg"},
		{"Q3 report (final).pdf", "application/pdf", 0, "20261017-093000-Q3_report_final_.pdf"},
		{"../../etc/passwd", "text/plain", 0, "20261017-093000-passwd.txt"},
		{"notes", "text/plain", 0, "20261017-093000-notes.txt"},
	}
	for _, c := range cases {
		if got := FileName(c.orig, c.mime, now, c.seq); got != c.want {
			t.Errorf("FileName(%q, %q) = %q, want %q", c.orig, c.mime, got, c.want)
		}
		if strings.Contains(FileName(c.orig, c.mime, now, c.seq), "/") {
			t.Errorf("FileName(%q) contains a slash", c.orig)
		}
	}
}

func TestInlineImage(t *testing.T) {
	if !InlineImage("image/jpeg") || InlineImage("image/svg+xml") || InlineImage("application/pdf") {
		t.Fatal("InlineImage misclassified")
	}
}
