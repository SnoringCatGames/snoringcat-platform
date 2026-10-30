package main

import (
	"strings"
	"testing"
)

func TestValidateDisplayName(t *testing.T) {
	good := []string{
		"BraveBunny42",
		"Ada Lovelace",
		"x",
		strings.Repeat("n", maxDisplayNameRunes),
	}
	for _, name := range good {
		if err := validateDisplayName(name); err != nil {
			t.Errorf("name %q: err = %v, want nil", name, err)
		}
	}
	bad := []string{
		"",
		"   ",
		"[deleted]",
		"line\nbreak",
		strings.Repeat("n", maxDisplayNameRunes+1),
	}
	for _, name := range bad {
		if err := validateDisplayName(name); errorCode(err) != 3 {
			t.Errorf("name %q: err = %v, want code 3", name, err)
		}
	}
}

func TestValidateAvatarURL(t *testing.T) {
	good := []string{
		"",
		"https://lh3.googleusercontent.com/a/avatar.png",
		"https://scontent.fbcdn.net/a.png",
		"https://avatars.steamstatic.com/a.jpg",
	}
	for _, raw := range good {
		if err := validateAvatarURL(raw); err != nil {
			t.Errorf("url %q: err = %v, want nil", raw, err)
		}
	}
	bad := []string{
		"http://lh3.googleusercontent.com/a.png",
		"https://tracker.invalid/pixel.png",
		"https://googleusercontent.com.evil.invalid/a.png",
		"https://lh3.googleusercontent.com/" +
			strings.Repeat("a", maxAvatarURLBytes),
	}
	for _, raw := range bad {
		if err := validateAvatarURL(raw); errorCode(err) != 3 {
			t.Errorf("url: err = %v, want code 3", err)
		}
	}
}
