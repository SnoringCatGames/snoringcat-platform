package main

import (
	"strings"
	"testing"
)

func TestValidateClientStorageWrite(t *testing.T) {
	if err := validateClientStorageWrite(
		"settings", `{"volume":3}`); err != nil {
		t.Fatalf("settings write: err = %v, want nil", err)
	}
	if err := validateClientStorageWrite(
		"presence", `{}`); errorCode(err) != 7 {
		t.Fatalf("presence write: err = %v, want code 7", err)
	}
	big := strings.Repeat("x", storageWriteMaxBytes+1)
	if err := validateClientStorageWrite(
		"settings", big); errorCode(err) != 3 {
		t.Fatalf("oversize write: err = %v, want code 3", err)
	}
}
