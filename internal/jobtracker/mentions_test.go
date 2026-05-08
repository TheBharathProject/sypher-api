package jobtracker

import (
	"reflect"
	"testing"
)

func TestExtractMentions(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"none", "no mentions here", nil},
		{"one simple", "thanks @alice", []string{"alice"}},
		{"trailing punct", "thanks @alice!", []string{"alice"}},
		{"comma", "@alice, @bob agreed", []string{"alice", "bob"}},
		{"dedup", "@alice and @alice again", []string{"alice"}},
		{"email-style ignored", "ping me at hi@example.com", nil},
		{"mid-sentence vs email", "see hi@example.com but also @alice", []string{"alice"}},
		{"too-short skipped", "@ab is too short", nil},
		{"too-long skipped", "@" + string(make([]byte, 41)), nil}, // pad ignored anyway by regex
		{"dash inside", "shoutout @priya-m for the help", []string{"priya-m"}},
		{"leading dash skipped", "@-bob isn't valid", nil},
		{"uppercase ignored", "@Alice not matched, @alice is", []string{"alice"}},
		{"after newline", "first line\n@alice second line", []string{"alice"}},
		{"start of string", "@alice opening", []string{"alice"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractMentions(c.in)
			if c.want == nil && len(got) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("ExtractMentions(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
