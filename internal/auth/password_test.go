package auth

import "testing"

func TestPassword(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "correct horse battery") {
		t.Fatal("correct password rejected")
	}
	if CheckPassword(h, "correct horse battery!") {
		t.Fatal("wrong password accepted")
	}
	if CheckPassword("garbage", "x") {
		t.Fatal("garbage hash accepted")
	}
}

func TestPasswordTooShort(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("expected error")
	}
}
