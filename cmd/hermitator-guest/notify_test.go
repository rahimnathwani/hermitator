package main

import (
	"strings"
	"testing"
)

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 10}
	b.Write([]byte("0123456789"))
	b.Write([]byte("abc"))
	if got := b.String(); got != "3456789abc" {
		t.Fatalf("tail = %q", got)
	}
	// A cut through a multi-byte character leaves no broken half.
	b = &tailBuffer{max: 4}
	b.Write([]byte("xé€"))
	if got := b.String(); !strings.HasSuffix(got, "€") || strings.ContainsRune(got, '�') {
		t.Fatalf("tail = %q", got)
	}
}
