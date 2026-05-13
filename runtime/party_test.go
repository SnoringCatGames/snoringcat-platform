package main

import (
	"strings"
	"testing"
)

func TestPartyInviteCodeKeys(t *testing.T) {
	if got := partyInviteCodeForwardKey("ABC23X"); got != "code:ABC23X" {
		t.Errorf("forward key = %q, want code:ABC23X", got)
	}
	if got := partyInviteCodeReverseKey(
		"party-uuid"); got != "party:party-uuid" {
		t.Errorf("reverse key = %q, want party:party-uuid", got)
	}
}

// TestGeneratePartyInviteCodeShape — codes are always exactly
// partyInviteCodeLength characters, drawn from the unambiguous
// alphabet (no I/O/0/1). Run 200 times to give the random distribution
// a chance to surface any out-of-alphabet character.
func TestGeneratePartyInviteCodeShape(t *testing.T) {
	const iterations = 200
	allowed := make(map[byte]bool, len(partyInviteCodeAlphabet))
	for i := 0; i < len(partyInviteCodeAlphabet); i++ {
		allowed[partyInviteCodeAlphabet[i]] = true
	}
	for i := 0; i < iterations; i++ {
		code, err := generatePartyInviteCode()
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if len(code) != partyInviteCodeLength {
			t.Fatalf("iteration %d: len=%d, want %d (code=%q)",
				i, len(code), partyInviteCodeLength, code)
		}
		for j := 0; j < len(code); j++ {
			if !allowed[code[j]] {
				t.Fatalf("iteration %d: code %q contains "+
					"out-of-alphabet byte %q",
					i, code, code[j])
			}
		}
	}
}

// TestPartyInviteCodeAlphabetExclusions — explicitly assert the
// readability-driven omissions (I/O/0/1) are absent. A maintainer
// who "fixes" the alphabet by re-including a confusing character
// will trip this test.
func TestPartyInviteCodeAlphabetExclusions(t *testing.T) {
	for _, banned := range "IO01" {
		if strings.ContainsRune(partyInviteCodeAlphabet, banned) {
			t.Errorf("alphabet must exclude %q (visual ambiguity)",
				banned)
		}
	}
	// Length should be a power of two so the modulo bias is minimal
	// (32 bytes → no bias when picking from a 256-value uniform
	// random source: 256 % 32 == 0).
	if len(partyInviteCodeAlphabet) != 32 {
		t.Errorf("alphabet length = %d, want 32 (modulo-bias free)",
			len(partyInviteCodeAlphabet))
	}
}
