package pi

import (
	"encoding/json"
	"testing"
)

// The ids below are what cursor-bridge's plugin (conversationID) makes for
// the same session and first user message, so a change on either side that
// breaks the join fails here.
func TestCursorConversationMatchesThePlugin(t *testing.T) {
	const session = "01a00000-0000-7000-8000-00000000p001"
	for _, c := range []struct {
		name string
		user string // the user message's content as pi stores it
		want string
	}{
		{"text with HTML characters and U+2028", `"fix <a> & b\u2028next"`, "aef6f1ec-9621-4403-a7df-0e7368302846"},
		{"parts with HTML characters", `[{"type":"text","text":"a <b> & c"}]`, "8c3b3c1c-f261-437f-8200-f13b59344e1f"},
		{"text and an image, empty text dropped", `[{"type":"text","text":"look"},{"type":"text","text":""},{"type":"image","data":"AAAA","mimeType":"image/png"}]`, "f8d1d0e4-f790-4340-b711-b75a07b839d7"},
	} {
		if got := cursorConversation(session, sentUser(json.RawMessage(c.user))); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	if got := cursorConversation(session, summaryUser("sum")); got != "a2958455-99c7-43cb-97b4-424204b1763e" {
		t.Errorf("compaction summary: %s", got)
	}
}
