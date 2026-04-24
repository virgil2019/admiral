package config

import "testing"

func TestSanitizeTeamName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"vibe bridge session", "vibe-bridge-session"},
		{"  vibe bridge session  ", "vibe-bridge-session"},
		{"UPPER Case Task", "upper-case-task"},
		{"multi---dashes   task", "multi-dashes-task"},
		{"task!@#$%^&*with symbols", "task-with-symbols"},
		{"中文-test-123", "test-123"},
		{"-leading-hyphen", "leading-hyphen"},
		{"trailing-hyphen-", "trailing-hyphen"},
		{"a-very-long-task-description-that-exceeds-thirty-chars", "a-very-long-task-description-t"},
		{"", ""},
	}
	for _, c := range cases {
		if got := SanitizeTeamName(c.in); got != c.want {
			t.Errorf("SanitizeTeamName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
