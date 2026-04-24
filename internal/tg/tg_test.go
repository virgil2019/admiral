package tg

import (
	"errors"
	"testing"
)

func TestIsPermanentTGError(t *testing.T) {
	cases := []struct {
		err  string
		want bool
	}{
		{"Forbidden: bot was blocked by the user", true},
		{"Bad Request: chat not found", true},
		{"Forbidden: user is deactivated", true},
		{"Too Many Requests: retry after 5", false},
		{"connection reset by peer", false},
		{"context deadline exceeded", false},
	}
	for _, c := range cases {
		got := isPermanentTGError(errors.New(c.err))
		if got != c.want {
			t.Errorf("isPermanentTGError(%q) = %v, want %v", c.err, got, c.want)
		}
	}
}
