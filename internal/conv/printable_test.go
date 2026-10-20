package conv

import "testing"

func TestPrintable(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"plain\ttext\nnext", "plain\ttext\nnext"},
		{"\x1b[31mred\x1b[0m done", "red done"},
		{"\x1b]0;new title\x07after", "after"},
		{"\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\", "link"},
		{"50%\r100%\n", "50%100%\n"},
		{"bell\a and \x1b7saved", "bell and saved"},
		{"中文 ok", "中文 ok"},
		{"bad \xff byte", "bad � byte"},
		{"cut \x1b[", "cut "},
	} {
		if got := Printable(c.in); got != c.want {
			t.Errorf("Printable(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
