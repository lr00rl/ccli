package cursorcli

import "testing"

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
