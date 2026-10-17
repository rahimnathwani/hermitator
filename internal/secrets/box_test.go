package secrets

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	key, err := ParseKey(GenerateKey())
	if err != nil {
		t.Fatal(err)
	}
	box, _ := New(key)
	ct := box.Seal("alice", "token", []byte("hunter2"))
	got, err := box.Open("alice", "token", ct)
	if err != nil || !bytes.Equal(got, []byte("hunter2")) {
		t.Fatalf("Open = %q, %v", got, err)
	}
}

func TestBoundToOwnerAndName(t *testing.T) {
	key, _ := ParseKey(GenerateKey())
	box, _ := New(key)
	ct := box.Seal("alice", "token", []byte("hunter2"))
	if _, err := box.Open("bob", "token", ct); err != ErrCorrupt {
		t.Fatalf("other owner: err = %v", err)
	}
	if _, err := box.Open("alice", "other", ct); err != ErrCorrupt {
		t.Fatalf("other name: err = %v", err)
	}
	ct[len(ct)-1] ^= 1
	if _, err := box.Open("alice", "token", ct); err != ErrCorrupt {
		t.Fatalf("tampered: err = %v", err)
	}
}

func TestParseKeyRejectsWrongLength(t *testing.T) {
	if _, err := ParseKey("c2hvcnQ="); err == nil {
		t.Fatal("expected error for short key")
	}
}
