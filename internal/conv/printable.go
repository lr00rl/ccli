package conv

import (
	"strings"
	"unicode/utf8"
)

// Printable removes terminal escape sequences and control characters other
// than newline and tab, so recorded tool output cannot move the cursor,
// retitle the window or recolor what follows when it is shown. Bytes that
// are not UTF-8 become U+FFFD.
func Printable(s string) string {
	if !needsCleaning(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == 0x1b:
			i += escapeLen(s[i:])
			continue
		case r == utf8.RuneError && size == 1:
			b.WriteRune(utf8.RuneError)
		case !isControl(r):
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func needsCleaning(s string) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == 0x1b || isControl(r) || (r == utf8.RuneError && size == 1) {
			return true
		}
		i += size
	}
	return false
}

func isControl(r rune) bool {
	return (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// escapeLen is the length of the escape sequence s starts with: a CSI
// sequence up to its final byte, an OSC, DCS, SOS, PM or APC string up to
// BEL or ST, or ESC and the one character after it.
func escapeLen(s string) int {
	if len(s) < 2 {
		return len(s)
	}
	switch s[1] {
	case '[':
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
		}
		return len(s)
	case ']', 'P', 'X', '^', '_':
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return len(s)
	}
	return 2
}
