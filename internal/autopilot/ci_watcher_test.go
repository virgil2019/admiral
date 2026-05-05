package autopilot

import (
	"testing"
	"time"
)

func TestAllFinal(t *testing.T) {
	tests := []struct {
		name string
		runs []checkRun
		want bool
	}{
		{
			name: "empty runs returns false (no checks yet)",
			runs: []checkRun{},
			want: false,
		},
		{
			name: "all completed returns true",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "success"},
				{Name: "lint", Status: "completed", Conclusion: "success"},
			},
			want: true,
		},
		{
			name: "one in_progress returns false",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "success"},
				{Name: "lint", Status: "in_progress", Conclusion: ""},
			},
			want: false,
		},
		{
			name: "one queued returns false",
			runs: []checkRun{
				{Name: "test", Status: "queued", Conclusion: ""},
			},
			want: false,
		},
		{
			name: "mixed completed and queued returns false",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "success"},
				{Name: "lint", Status: "queued", Conclusion: ""},
				{Name: "build", Status: "queued", Conclusion: ""},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := allFinal(tt.runs)
			if got != tt.want {
				t.Errorf("allFinal() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFailedChecks(t *testing.T) {
	tests := []struct {
		name string
		runs []checkRun
		want int
	}{
		{
			name: "no runs returns empty",
			runs: []checkRun{},
			want: 0,
		},
		{
			name: "all success returns empty",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "success"},
			},
			want: 0,
		},
		{
			name: "one failure returns one",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "failure"},
			},
			want: 1,
		},
		{
			name: "mixed success and failure returns failures",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "success"},
				{Name: "lint", Status: "completed", Conclusion: "failure"},
				{Name: "build", Status: "completed", Conclusion: "success"},
			},
			want: 1,
		},
		{
			name: "neutral is not a failure",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "neutral"},
			},
			want: 0,
		},
		{
			name: "skipped is not a failure",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "skipped"},
			},
			want: 0,
		},
		{
			name: "cancelled is a failure",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "cancelled"},
			},
			want: 1,
		},
		{
			name: "in_progress is not a failure (not final)",
			runs: []checkRun{
				{Name: "test", Status: "in_progress", Conclusion: ""},
			},
			want: 0,
		},
		{
			name: "multiple failures",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "failure"},
				{Name: "lint", Status: "completed", Conclusion: "failure"},
			},
			want: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := failedChecks(tt.runs)
			if len(got) != tt.want {
				t.Errorf("failedChecks() returned %d, want %d", len(got), tt.want)
			}
		})
	}
}

func TestCheckNamesList(t *testing.T) {
	tests := []struct {
		name string
		runs []checkRun
		want string
	}{
		{
			name: "empty returns empty string",
			runs: []checkRun{},
			want: "",
		},
		{
			name: "single check",
			runs: []checkRun{
				{Name: "test"},
			},
			want: "test",
		},
		{
			name: "multiple checks unique names",
			runs: []checkRun{
				{Name: "test"},
				{Name: "lint"},
				{Name: "build"},
			},
			want: "test, lint, build",
		},
		{
			name: "duplicate names deduplicated",
			runs: []checkRun{
				{Name: "test"},
				{Name: "test"},
				{Name: "lint"},
			},
			want: "test, lint",
		},
		{
			name: "order preserved first-seen",
			runs: []checkRun{
				{Name: "lint"},
				{Name: "test"},
				{Name: "lint"}, // duplicate, should not change order
			},
			want: "lint, test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkNamesList(tt.runs)
			if got != tt.want {
				t.Errorf("checkNamesList() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractPRNumberFromURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		want    int
		wantErr bool
	}{
		{
			name:    "standard github PR URL",
			url:     "https://github.com/owner/repo/pull/123",
			want:    123,
			wantErr: false,
		},
		{
			name:    "PR URL with files suffix",
			url:     "https://github.com/owner/repo/pull/456/files",
			want:    456,
			wantErr: false,
		},
		{
			name:    "PR URL with conversation suffix",
			url:     "https://github.com/owner/repo/pull/789/conversations",
			want:    789,
			wantErr: false,
		},
		{
			name:    "PR URL with extra path segments after number",
			url:     "https://github.com/owner/repo/pull/100/checks",
			want:    100,
			wantErr: false,
		},
		{
			name:    "invalid URL - not a PR",
			url:     "https://github.com/owner/repo/issues/1",
			want:    0,
			wantErr: true,
		},
		{
			name:    "invalid URL - empty",
			url:     "",
			want:    0,
			wantErr: true,
		},
		{
			name:    "invalid URL - no host",
			url:     "/pull/123",
			want:    0,
			wantErr: true,
		},
		{
			name:    "invalid URL - no PR segment",
			url:     "https://github.com/owner/repo",
			want:    0,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractPRNumberFromURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("extractPRNumberFromURL() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("extractPRNumberFromURL() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseInt(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want int
		ok   bool
	}{
		{"simple number", "123", 123, true},
		{"zero not valid for PR numbers", "0", 0, false},
		{"single digit", "5", 5, true},
		{"empty string", "", 0, false},
		{"non-numeric", "abc", 0, false},
		{"mixed", "12abc", 0, false},
		{"leading zeros", "007", 7, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseInt(tt.s)
			if ok != tt.ok {
				t.Errorf("parseInt() ok = %v, want %v", ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("parseInt() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewCIWatcher(t *testing.T) {
	w := newCIWatcher(nil, nil, nil, "gh", 30*time.Second, 15*time.Minute)
	if w == nil {
		t.Fatal("newCIWatcher returned nil")
	}
	if w.pollInter != 30*time.Second {
		t.Errorf("pollInter = %v, want 30s", w.pollInter)
	}
	if w.timeout != 15*time.Minute {
		t.Errorf("timeout = %v, want 15m", w.timeout)
	}
	if w.ghBin != "gh" {
		t.Errorf("ghBin = %q, want gh", w.ghBin)
	}
}
