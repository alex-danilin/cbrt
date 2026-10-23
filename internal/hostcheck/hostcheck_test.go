package hostcheck

import "testing"

func TestLoopbackHeader(t *testing.T) {
	for h, want := range map[string]bool{
		"localhost": true, "LOCALHOST:8080": true, "127.0.0.1": true, "127.9.9.9:80": true,
		"[::1]:8080": true, "[::1]": true, "::1": true,
		"evil.example": false, "evil.example:80": false, "10.0.0.1": false, "[::2]:80": false,
		"localhost.evil.example": false, "": false,
	} {
		if got := LoopbackHeader(h); got != want {
			t.Errorf("%q: loopback=%v, want %v", h, got, want)
		}
	}
}
