package cursorcli

import (
	"testing"
	"time"
)

func TestCursorWhen(t *testing.T) {
	text := "Wednesday, Sep 23, 2026, 3:19 AM (UTC-7)\n<user_query>hi</user_query>"
	text = "<timestamp>" + text[:len("Wednesday, Sep 23, 2026, 3:19 AM (UTC-7)")] + "</timestamp>"
	got := cursorWhen(text)
	if got == 0 {
		t.Fatal("timestamp tag did not parse")
	}
	if cursorWhen("no clock here") != 0 {
		t.Fatal("plain text produced a time")
	}
}

func TestCursorWhenNeedsAZone(t *testing.T) {
	at := cursorWhen("<timestamp>Wednesday, Sep 23, 2026, 3:19 AM (UTC-7)</timestamp>")
	if want := time.Date(2026, 9, 23, 10, 19, 0, 0, time.UTC).UnixMilli(); at != want {
		t.Fatalf("UTC-7: %d, want %d", at, want)
	}
	for _, s := range []string{
		"<timestamp>Wednesday, Sep 23, 2026, 3:19 AM (PDT)</timestamp>",
		"<timestamp>Wednesday, Sep 23, 2026, 3:19 AM</timestamp>",
	} {
		if at := cursorWhen(s); at != 0 {
			t.Fatalf("%s: %d, want 0 so the chat's own clock is used", s, at)
		}
	}
}
