package main

import "testing"

func TestSelectTransportType(t *testing.T) {
	cases := []struct {
		name      string
		platforms []string
		want      string
	}{
		{"empty", nil, "enet"},
		{"all-native", []string{"native", "native"}, "enet"},
		{"single-web", []string{"web"}, "webrtc"},
		{"mixed", []string{"native", "web", "native"}, "webrtc"},
		{"unknown-counts-as-native",
			[]string{"", "macos", "android"}, "enet"},
		{"web-first", []string{"web", "native"}, "webrtc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectTransportType(tc.platforms)
			if got != tc.want {
				t.Fatalf("selectTransportType(%v) = %q, want %q",
					tc.platforms, got, tc.want)
			}
		})
	}
}
