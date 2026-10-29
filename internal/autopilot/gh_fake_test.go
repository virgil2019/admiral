package autopilot

import (
	"context"
	"errors"
	"slices"
	"strings"

	ghpkg "github.com/georgehuang/admiral/internal/github"
)

// defaultTestGhUser is the gh account a mock repo is registered with unless
// a test opts out (mockStore.RepoNoGhUser).
const defaultTestGhUser = "admiral"

// fakeGhAccount describes one account as the fake gh sees it.
type fakeGhAccount struct {
	loggedIn bool
	// repos maps "owner/name" → push permission. A repo missing from the
	// map is invisible to the account (gh api 404).
	repos map[string]bool
}

// fakeAccounts returns an Accounts whose gh resolves a token for any
// account. Tests that don't exercise account resolution use this so the
// identity gate passes.
func fakeAccounts() *ghpkg.Accounts {
	return ghpkg.NewAccountsWithRunner("gh", func(_ context.Context, _ []string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			i := slices.Index(args, "-u")
			if i < 0 || i+1 >= len(args) {
				return "", errors.New("fake gh: auth token without -u")
			}
			return "tok-" + args[i+1] + "\n", nil
		}
		return "", errors.New("fake gh: unexpected call: " + strings.Join(args, " "))
	})
}

// fakeGhAccountsRunner emulates `gh auth token -u <user>` and
// `gh api repos/<owner>/<name> --jq .permissions.push` for the given
// accounts. The token for user X is "tok-X"; api calls authenticate by the
// GH_TOKEN in env, so a call without the right token fails like gh would.
func fakeGhAccountsRunner(accounts map[string]fakeGhAccount) ghpkg.Runner {
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
