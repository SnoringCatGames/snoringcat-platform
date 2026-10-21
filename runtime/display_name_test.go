package main

import (
	"strings"
	"testing"
)

// H59: generated names are the only display names; they must
// look generated (word + word + number), never provider text.
func TestGeneratedDisplayNameShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		name, err := generatedDisplayName()
		if err != nil {
			t.Fatalf("generatedDisplayName: %v", err)
		}
		parts := strings.Split(name, "_")
		if len(parts) != 2 || len(parts[1]) != 4 {
			t.Errorf("name %q lacks the _NNNN suffix", name)
		}
		if strings.Contains(name, " ") {
			t.Errorf("name %q contains a space (provider-style name?)", name)
		}
		seen[name] = true
	}
	if len(seen) < 60 {
		t.Errorf("only %d distinct names in 64 mints; generator looks stuck", len(seen))
	}
}
