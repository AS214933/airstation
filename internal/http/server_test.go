package http

import "testing"

func TestTelegramHLSURL(t *testing.T) {
	got := telegramHLSURL("7331")
	const want = "http://127.0.0.1:7331/stream.m3u8"
	if got != want {
		t.Fatalf("telegramHLSURL() = %q, want %q", got, want)
	}
}
