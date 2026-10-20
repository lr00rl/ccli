package conv

import (
	"strings"
	"testing"
)

func texts(es []Entry) string {
	var out []string
	for _, e := range es {
		out = append(out, string(e.Kind)+":"+e.Text)
	}
	return strings.Join(out, " | ")
}

func TestReplyEntries(t *testing.T) {
	tool := Entry{Kind: KindTool, Call: &Call{Name: "Bash"}}
	for _, c := range []struct {
		name string
		in   []Entry
		want string
	}{
		{"last reply after the tools", []Entry{
			{Kind: KindPrompt, Text: "fix it"}, {Kind: KindText, Text: "Looking."}, tool, {Kind: KindText, Text: "Fixed."},
		}, "prompt:fix it | text:Fixed."},
		{"an earlier notice is history", []Entry{
			{Kind: KindPrompt, Text: "fix it"}, {Kind: KindNotice, Text: "API error, retrying"}, {Kind: KindText, Text: "Fixed."},
		}, "prompt:fix it | text:Fixed."},
		{"a turn that was stopped says so", []Entry{
			{Kind: KindPrompt, Text: "fix it"}, {Kind: KindText, Text: "Running the tests."}, tool, {Kind: KindNotice, Text: "Interrupted"},
		}, "prompt:fix it | text:Running the tests. | notice:Interrupted"},
	} {
		if got := texts(ReplyEntries(c.in)); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}
