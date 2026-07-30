package auth

import (
	"testing"
	"time"
)

func TestExpiryFromExpiresIn(t *testing.T) {
	if got := expiryFromExpiresIn(0); got != 0 {
		t.Errorf("expiresIn=0: got %d, want 0 (unknown/trusted)", got)
	}
	if got := expiryFromExpiresIn(-5); got != 0 {
		t.Errorf("expiresIn=-5: got %d, want 0 (unknown/trusted)", got)
	}
	before := time.Now().Add(3600 * time.Second).UnixMilli()
	got := expiryFromExpiresIn(3600)
	after := time.Now().Add(3600 * time.Second).UnixMilli()
	if got < before || got > after {
		t.Errorf("expiresIn=3600: got %d, want ~now+3600s in [%d,%d]", got, before, after)
	}
}
