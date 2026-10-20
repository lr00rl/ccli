package cli

import (
	"strings"
	"testing"
)

func TestPeelGlobals(t *testing.T) {
	for _, c := range []struct {
		args    string
		debug   debugLevel
		verbose bool
		rest    string
	}{
		{"--debug sessions", 1, false, "sessions"},
		{"--debug 2 sessions", 2, false, "sessions"},
		{"-debug=3 --verbose sessions -a x", 3, true, "sessions -a x"},
		{"--debug=3 --debug=1 stats", 1, false, "stats"},
		{"--debug=3 --debug=false stats", 0, false, "stats"},
		{"--verbose=1 --verbose=false quota", 0, false, "quota"},
		{"sessions --debug", 0, false, "sessions --debug"},
	} {
		a := &app{}
		rest, err := a.peelGlobals(strings.Fields(c.args))
		if err != nil {
			t.Fatalf("%s: %v", c.args, err)
		}
		if a.debug != c.debug || a.verbose != c.verbose || strings.Join(rest, " ") != c.rest {
			t.Errorf("%s: debug %d verbose %t rest %q", c.args, a.debug, a.verbose, rest)
		}
	}
	for _, bad := range []string{"--debug=7 sessions", "--debug 9 sessions", "--verbose=maybe sessions"} {
		if _, err := (&app{}).peelGlobals(strings.Fields(bad)); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}
