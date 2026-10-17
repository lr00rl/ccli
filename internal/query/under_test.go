package query_test

import (
	"testing"

	"github.com/lr00rl/ccli/internal/query"
)

func TestUnder(t *testing.T) {
	for _, c := range []struct {
		path, dir string
		want      bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b/c", "/a/b", true},
		{"/a/bc", "/a/b", false}, // shares the prefix, not the folder
		{"/a/b", "/a/b/", true},
		{"/a", "/a/b", false},
		{"/x/y", "/", true},
		{"", "/", false},
		{"/a", "", false},
	} {
		if got := query.Under(c.path, c.dir); got != c.want {
			t.Errorf("Under(%q, %q) = %v, want %v", c.path, c.dir, got, c.want)
		}
	}
	if got := query.UnderAny("/p/app/x", []string{"/l/app", "/p/app"}); got != "/p/app" {
		t.Errorf("UnderAny picked %q, want the form that matched", got)
	}
}
