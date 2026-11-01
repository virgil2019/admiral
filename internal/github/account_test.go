package github

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeGhAccount describes one account as the fake gh sees it.
type fakeGhAccount struct {
	loggedIn bool
	// repos maps "owner/name" → push permission. A repo missing from the
	// map is invisible to the account (gh api 404).
	repos map[string]bool
}

// fakeGhAccountsRunner emulates `gh auth token -u <user>` and
// `gh api repos/<owner>/<name> --jq .permissions.push` for the given
// accounts. The token for user X is "tok-X"; api calls authenticate by the
// GH_TOKEN in env, so a call without the right token fails like gh would.
func fakeGhAccountsRunner(accounts map[string]fakeGhAccount) Runner {
	return func(_ context.Context, env []string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case len(args) >= 2 && args[0] == "auth" && args[1] == "token":
			i := slices.Index(args, "-u")
			if i < 0 || i+1 >= len(args) {
				return "", errors.New("fake gh: auth token without -u")
			}
			user := args[i+1]
			if acct, ok := accounts[user]; ok && acct.loggedIn {
				return "tok-" + user + "\n", nil
			}
			return "no oauth token found for github.com account " + user, errors.New("exit status 1")
		case len(args) >= 4 && args[0] == "api" && args[1] == "--hostname" && args[2] == "github.com" && strings.HasPrefix(args[3], "repos/"):
			var user string
			for _, kv := range env {
				if tok, ok := strings.CutPrefix(kv, "GH_TOKEN=tok-"); ok {
					user = tok
				}
			}
			acct, ok := accounts[user]
			if !ok || !acct.loggedIn {
				return "gh: Bad credentials (HTTP 401)", errors.New("exit status 1")
			}
			push, visible := acct.repos[strings.TrimPrefix(args[3], "repos/")]
			if !visible {
				return `{"message":"Not Found"}gh: Not Found (HTTP 404)`, errors.New("exit status 1")
			}
			if push {
				return "true\n", nil
			}
			return "false\n", nil
		}
		return "", errors.New("fake gh: unexpected call: " + joined)
	}
}

func TestParseGitHubRepo(t *testing.T) {
	cases := []struct {
		in        string
		wantOwner string
		wantName  string
		wantOK    bool
	}{
		{"git@virgil.github.com:virgil2019/klink.git", "virgil2019", "klink", true},
		{"git@github.com:virgil2019/admiral.git\n", "virgil2019", "admiral", true},
		{"git@github.com:virgil2019/admiral", "virgil2019", "admiral", true},
		{"ssh://git@github.com/virgil2019/klink.git", "virgil2019", "klink", true},
		{"https://github.com/virgil2019/klink.git", "virgil2019", "klink", true},
		{"https://github.com/virgil2019/klink", "virgil2019", "klink", true},
		{"virgil:virgil2019/klink.git", "virgil2019", "klink", true},
		{"git@github.com:/virgil2019/klink.git", "virgil2019", "klink", true},
		{"ssh://git@github.com:22", "", "", false},
		{"./rel/path:x/y", "", "", false},
		{"", "", "", false},
		{"/local/path/repo", "", "", false},
		{"git@github.com:onlyowner", "", "", false},
	}
	for _, c := range cases {
		owner, name, ok := parseGitHubRepo(c.in)
		if owner != c.wantOwner || name != c.wantName || ok != c.wantOK {
			t.Errorf("parseGitHubRepo(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, owner, name, ok, c.wantOwner, c.wantName, c.wantOK)
		}
	}
}

func TestAccounts_ValidateRepoAccess(t *testing.T) {
	accounts := map[string]fakeGhAccount{
		"virgil2019": {loggedIn: true, repos: map[string]bool{"virgil2019/klink": true, "other/readonly": false}},
		"chababa9":   {loggedIn: true, repos: map[string]bool{}},
	}
	a := NewAccountsWithRunner("gh", fakeGhAccountsRunner(accounts))
	const origin = "git@virgil.github.com:virgil2019/klink.git"

	cases := []struct {
		name    string
		user    string
		origin  string
		wantErr string // substring; "" = expect success
	}{
		{"logged in with push", "virgil2019", origin, ""},
		{"empty user", "", origin, "gh_user is required"},
		{"not logged in", "nobody", origin, `cannot get a token for gh account "nobody"`},
		{"repo not visible", "chababa9", origin, `cannot see virgil2019/klink`},
		{"no push permission", "virgil2019", "git@github.com:other/readonly.git", `no push permission on other/readonly`},
		{"unparseable origin", "virgil2019", "/local/path", "cannot parse GitHub owner/repo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := a.ValidateRepoAccess(context.Background(), c.user, c.origin)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("expected error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestAccounts_Token(t *testing.T) {
	a := NewAccountsWithRunner("gh", fakeGhAccountsRunner(map[string]fakeGhAccount{
		"virgil2019": {loggedIn: true},
	}))
	tok, err := a.Token(context.Background(), "virgil2019")
	if err != nil || tok != "tok-virgil2019" {
		t.Fatalf("Token = (%q, %v), want (tok-virgil2019, nil)", tok, err)
	}
	if _, err := a.Token(context.Background(), "nobody"); err == nil {
		t.Fatal("expected error for account not logged in")
	}
}

// TestAccounts_EnvForPinsIdentity: the env admiral hands to gh must pin
// GH_TOKEN to the requested account and drop every ambient token variable,
// so the host's active gh account can never take over.
func TestAccounts_EnvForPinsIdentity(t *testing.T) {
	t.Setenv("GH_TOKEN", "ambient-token")
	t.Setenv("GITHUB_TOKEN", "ambient-token-2")
	a := NewAccountsWithRunner("gh", fakeGhAccountsRunner(map[string]fakeGhAccount{
		"virgil2019": {loggedIn: true},
	}))
	env, err := a.EnvFor(context.Background(), "virgil2019")
	if err != nil {
		t.Fatalf("EnvFor: %v", err)
	}
	var tokens []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "GH_TOKEN=") || strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			tokens = append(tokens, kv)
		}
	}
	want := []string{"GH_TOKEN=tok-virgil2019"}
	if !slices.Equal(tokens, want) {
		t.Fatalf("token vars = %v, want %v", tokens, want)
	}
	if _, err := a.EnvFor(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty user")
	}
}

// TestAccounts_RealRunnerIgnoresStderr runs a fake gh script that warns on
// stderr while succeeding: only stdout may reach the token / --jq value.
func TestAccounts_RealRunnerIgnoresStderr(t *testing.T) {
	script := filepath.Join(t.TempDir(), "gh")
	body := `#!/bin/sh
echo "warning: something deprecated" >&2
case "$1" in
  auth) echo "tok-real" ;;
  api) [ "$GH_TOKEN" = "tok-real" ] && echo true || { echo "bad token" >&2; exit 1; } ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	a := NewAccounts(script)
	if tok, err := a.Token(context.Background(), "u"); err != nil || tok != "tok-real" {
		t.Fatalf("Token = (%q, %v), want tok-real", tok, err)
	}
	if err := a.ValidateRepoAccess(context.Background(), "u", "git@github.com:o/n.git"); err != nil {
		t.Fatalf("ValidateRepoAccess: %v", err)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("✓✓✓", 2); got != "✓✓..." {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("ab", 5); got != "ab" {
		t.Fatalf("truncate = %q", got)
	}
}

func TestStripGhTokenEnv(t *testing.T) {
	in := []string{"PATH=/bin", "GH_TOKEN=x", "GITHUB_TOKEN=y", "GH_ENTERPRISE_TOKEN=z", "GITHUB_ENTERPRISE_TOKEN=w", "HOME=/h"}
	got := stripGhTokenEnv(in)
	want := []string{"PATH=/bin", "HOME=/h"}
	if !slices.Equal(got, want) {
		t.Fatalf("stripGhTokenEnv = %v, want %v", got, want)
	}
}
