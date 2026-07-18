package main

import (
	"context"
	"strings"
	"testing"
)

func TestFriendCodeKeys(t *testing.T) {
	if got := friendCodeForwardKey("K7QP2M9X"); got != "code:K7QP2M9X" {
		t.Errorf("forward key = %q, want code:K7QP2M9X", got)
	}
	if got := friendCodeReverseKey("user-uuid"); got != "user:user-uuid" {
		t.Errorf("reverse key = %q, want user:user-uuid", got)
	}
}

// TestGenerateFriendCodeShape — codes are always exactly
// friendCodeLength characters drawn from the unambiguous alphabet
// (no I/O/0/1). Run 200 times so the random distribution has a chance
// to surface any out-of-alphabet byte or wrong length.
func TestGenerateFriendCodeShape(t *testing.T) {
	const iterations = 200
	allowed := make(map[byte]bool, len(friendCodeAlphabet))
	for i := 0; i < len(friendCodeAlphabet); i++ {
		allowed[friendCodeAlphabet[i]] = true
	}
	for i := 0; i < iterations; i++ {
		code, err := generateFriendCode()
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if len(code) != friendCodeLength {
			t.Fatalf("iteration %d: len=%d, want %d (code=%q)",
				i, len(code), friendCodeLength, code)
		}
		for j := 0; j < len(code); j++ {
			if !allowed[code[j]] {
				t.Fatalf("iteration %d: code %q contains "+
					"out-of-alphabet byte %q", i, code, code[j])
			}
		}
	}
}

// TestFriendCodeAlphabetExclusions — the readability omissions
// (I/O/0/1) must stay absent, and the alphabet must be 32 chars so the
// modulo pick is bias-free (256 % 32 == 0). Mirrors the party-code
// guard so the two stay in the same family.
func TestFriendCodeAlphabetExclusions(t *testing.T) {
	for _, banned := range "IO01" {
		if strings.ContainsRune(friendCodeAlphabet, banned) {
			t.Errorf("alphabet must exclude %q (visual ambiguity)",
				banned)
		}
	}
	if len(friendCodeAlphabet) != 32 {
		t.Errorf("alphabet length = %d, want 32 (modulo-bias free)",
			len(friendCodeAlphabet))
	}
}

// TestFriendCodeLength pins the length to 8. The client's
// _FRIEND_CODE_LENGTH (add_friend_panel.gd) mirrors this value and the
// textbox auto-advances at exactly this many characters, so a change
// here that isn't matched client-side breaks the entry UX.
func TestFriendCodeLength(t *testing.T) {
	if friendCodeLength != 8 {
		t.Errorf("friendCodeLength = %d, want 8 (mirror the client's"+
			" _FRIEND_CODE_LENGTH when changing this)", friendCodeLength)
	}
}

// TestResolveFriendCodeEmpty — an empty or whitespace-only code
// short-circuits to "" before any storage access, so the resolver
// never touches nk for a blank input. Passing a nil module proves the
// early return: a regression that dropped the guard would nil-panic.
func TestResolveFriendCodeEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n"} {
		got, err := resolveFriendCode(context.Background(), nil, in)
		if err != nil {
			t.Errorf("resolveFriendCode(%q) err = %v, want nil", in, err)
		}
		if got != "" {
			t.Errorf("resolveFriendCode(%q) = %q, want \"\"", in, got)
		}
	}
}
