package render

import "testing"

func TestShortHome(t *testing.T) {
	t.Setenv("HOME", "/Users/al")
	for in, want := range map[string]string{
		"/Users/al":           "~",
		"/Users/al/code/app":  "~/code/app",
		"/Users/alex/code":    "/Users/alex/code", // shares the prefix, not the folder
		"/private/tmp/x":      "/private/tmp/x",
		"relative/Users/al/x": "relative/Users/al/x",
	} {
		if got := ShortHome(in); got != want {
			t.Errorf("ShortHome(%q) = %q, want %q", in, got, want)
		}
	}
}
