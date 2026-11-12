package pi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// A pi session that talks to Cursor through cursor-bridge sends each request
// under a Cursor conversation id that the bridge's plugin
// (@magpie-community/opencode-cursor-auth, conversationID) makes from the
// session id pi names and the first user message of what pi sent:
//
//	uuid4(sha256("cursor conversation\x00" + session + "\x00" + JSON(first)))
//
// with JSON as JavaScript's JSON.stringify writes the message pi built
// (pi-ai's openai-completions convertMessages). Cursor's usage page bills by
// that id. ccli rebuilds it from the transcript to join the two.
//
// The first user message pi sends is the session's first prompt until a
// compaction replaces the history before it with a summary, and the summary
// message is first from then on, so one session can run under several ids.

const (
	compactionPrefix = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n"
	compactionSuffix = "\n</summary>"
)

// cursorConversation is the plugin's conversation id for session and the
// first user message as pi sends it.
func cursorConversation(session string, first any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// JSON.stringify leaves <, > and & alone, and so must this.
	enc.SetEscapeHTML(false)
	if enc.Encode(first) != nil {
		return ""
	}
	js := bytes.TrimRight(buf.Bytes(), "\n")
	// JSON.stringify leaves U+2028 and U+2029 unescaped; Go escapes them.
	js = bytes.ReplaceAll(js, []byte(`\u2028`), []byte("\u2028"))
	js = bytes.ReplaceAll(js, []byte(`\u2029`), []byte("\u2029"))
	h := sha256.New()
	h.Write([]byte("cursor conversation\x00" + session + "\x00"))
	h.Write(js)
	b := h.Sum(nil)[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

// userPart is one part of a user message as pi sends it.
type userPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

// sentUser is a user message as pi sends it: a string stays a string, parts
// keep their order with empty text parts dropped and images as data URLs.
func sentUser(raw json.RawMessage) any {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
		return struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{"user", stripLoneSurrogates(s)}
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	out := []userPart{}
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, userPart{Type: "text", Text: stripLoneSurrogates(p.Text)})
			}
		case "image":
			out = append(out, userPart{Type: "image_url", ImageURL: &imageURL{URL: "data:" + p.MimeType + ";base64," + p.Data}})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return userParts(out)
}

// userParts marshals as pi's {"role":"user","content":[...]}.
type userParts []userPart

// MarshalJSON leaves <, > and & unescaped, as JSON.stringify does: the
// encoder's SetEscapeHTML(false) does not undo escapes a Marshaler already
// wrote, and every compaction summary holds <summary>.
func (u userParts) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(struct {
		Role    string     `json:"role"`
		Content []userPart `json:"content"`
	}{"user", []userPart(u)}); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// cursorConversations is every Cursor conversation id a session's requests
// to Cursor went out under: one for its first prompt, and one more for each
// compaction's summary, which is the first user message pi sends after it.
// Only a session with a reply from the cursor provider has any; the ids of
// a session that never asked Cursor would join nothing. Abandoned branches
// count: they were sent and billed too.
func cursorConversations(session string, tree []rec) []string {
	asked := false
	for _, rc := range tree {
		if rc.role == "assistant" && strings.HasPrefix(rc.model, "cursor/") {
			asked = true
			break
		}
	}
	if !asked || session == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(first any) {
		if first == nil {
			return
		}
		if id := cursorConversation(session, first); id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	first := true
	for _, rc := range tree {
		switch {
		case first && rc.kind == "message" && rc.role == "user":
			add(sentUser(rc.userRaw))
			first = false
		case rc.kind == "compaction" && rc.summary != "":
			add(summaryUser(rc.summary))
		}
	}
	return out
}

// summaryUser is the user message pi sends for a compaction's summary.
func summaryUser(summary string) any {
	return userParts{{Type: "text", Text: stripLoneSurrogates(compactionPrefix + summary + compactionSuffix)}}
}

// stripLoneSurrogates mirrors pi's sanitizeSurrogates. Go strings hold UTF-8,
// where a lone surrogate is already invalid and json.Marshal writes U+FFFD;
// dropping the replacement character here matches what pi removed.
func stripLoneSurrogates(s string) string {
	if !strings.ContainsRune(s, '\uFFFD') {
		return s
	}
	return strings.ToValidUTF8(s, "")
}
