package github

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ghHost is the GitHub host admiral talks to. Repo remotes may use an SSH
// alias host (e.g. git@virgil.github.com:owner/name.git) to pin the push
// identity; for API calls only owner/name matter and the host is always
// github.com. GitHub Enterprise is not supported.
const ghHost = "github.com"

// ghValidateTimeout bounds one ValidateRepoDir call (token lookup + API).
const ghValidateTimeout = 30 * time.Second

// Runner runs the gh binary with args. env is the complete child
// environment (nil = inherit). It returns stdout on success, and stdout plus
// stderr on failure (for error messages). Tests substitute a fake.
type Runner func(ctx context.Context, env []string, args ...string) (string, error)

// Accounts resolves a repo's configured gh account (repos.gh_user) to a
// token and validates that the account can push to the repo. Tokens are
// read from the gh keyring on every call and never cached or persisted.
type Accounts struct {
	bin string
	run Runner
}

// NewAccounts builds an Accounts that shells out to the gh binary at bin
// ("gh" resolves through PATH). An empty bin means "gh".
func NewAccounts(bin string) *Accounts {
	if bin == "" {
		bin = "gh"
	}
	return &Accounts{bin: bin}
}

// NewAccountsWithRunner builds an Accounts whose gh invocations go through
// run instead of the real binary. For tests in other packages that need to
// fake the gh CLI.
func NewAccountsWithRunner(bin string, run Runner) *Accounts {
	return &Accounts{bin: bin, run: run}
}

func (a *Accounts) gh(ctx context.Context, env []string, args ...string) (string, error) {
	if a.run != nil {
		return a.run(ctx, env, args...)
	}
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Env = env
	// stdout only: gh may print warnings on stderr even on success, which
	// would corrupt the token / --jq value.
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stdout.String() + "\n" + stderr.String()), err
	}
	return stdout.String(), nil
}

// Token returns the OAuth token gh holds for user in its keyring. With -u,
// gh ignores any GH_TOKEN in the environment.
func (a *Accounts) Token(ctx context.Context, user string) (string, error) {
	user = strings.TrimSpace(user)
	if user == "" {
		return "", ErrGhUserRequired
	}
	out, err := a.gh(ctx, nil, "auth", "token", "-h", ghHost, "-u", user)
	tok := strings.TrimSpace(out)
	if err != nil || tok == "" {
		return "", fmt.Errorf("cannot get a token for gh account %q (not logged in? run `gh auth login -h %s` and sign in as %s): %s",
			user, ghHost, user, truncate(tok, 200))
	}
	return tok, nil
}

// EnvFor returns the environment a gh subprocess must run under to
// authenticate as user: the host environment with every ambient token
// variable stripped, plus GH_TOKEN pinned to that account's token.
//
// The token is read from the keyring on every call and never cached, so a
// re-login (or a rotated credential) takes effect on the next call without
// restarting admiral.
func (a *Accounts) EnvFor(ctx context.Context, user string) ([]string, error) {
	tok, err := a.Token(ctx, user)
	if err != nil {
		return nil, err
	}
	return append(stripGhTokenEnv(os.Environ()), "GH_TOKEN="+tok), nil
}

// ValidateRepoAccess checks that user is logged in to gh and has push
// permission on the GitHub repo originURL points at. The returned error
// says which of the two is missing.
func (a *Accounts) ValidateRepoAccess(ctx context.Context, user, originURL string) error {
	user = strings.TrimSpace(user)
	if user == "" {
		return ErrGhUserRequired
	}
	owner, name, ok := parseGitHubRepo(originURL)
	if !ok {
		return fmt.Errorf("cannot parse GitHub owner/repo from origin URL %q", strings.TrimSpace(originURL))
	}
	tok, err := a.Token(ctx, user)
	if err != nil {
		return err
	}
	env := append(stripGhTokenEnv(os.Environ()), "GH_TOKEN="+tok)
	out, err := a.gh(ctx, env, "api", "--hostname", ghHost, "repos/"+owner+"/"+name, "--jq", ".permissions.push")
	if err != nil {
		return fmt.Errorf("gh account %q cannot see %s/%s (not a collaborator, or the repo does not exist): %s",
			user, owner, name, truncate(strings.TrimSpace(out), 200))
	}
	if strings.TrimSpace(out) != "true" {
		return fmt.Errorf("gh account %q has no push permission on %s/%s", user, owner, name)
	}
	return nil
}

// ErrGhUserRequired is returned when a repo has no gh account registered.
// admiral refuses to act on a repo whose GitHub identity is unknown rather
// than falling back to whatever account happens to be active on the host.
var ErrGhUserRequired = fmt.Errorf("gh_user is required: the gh account admiral uses for this repo (see `gh auth status`)")

// stripGhTokenEnv drops the env vars gh reads a token from, so the token
// admiral passes (or the keyring lookup) is the only identity in play.
func stripGhTokenEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// parseGitHubRepo extracts owner and name from a git remote URL in scp
// ([user@]host:owner/name.git), ssh:// or https:// form. The host is ignored.
func parseGitHubRepo(remoteURL string) (owner, name string, ok bool) {
	u := strings.TrimSpace(remoteURL)
	var path string
	switch {
	case strings.HasPrefix(u, "ssh://"), strings.HasPrefix(u, "https://"), strings.HasPrefix(u, "http://"):
		rest := u[strings.Index(u, "://")+3:]
		i := strings.Index(rest, "/")
		if i < 0 {
			return "", "", false
		}
		path = rest[i+1:]
	case strings.Contains(u, "://"):
		return "", "", false
	case strings.Index(u, ":") > 0 && !strings.Contains(u[:strings.Index(u, ":")], "/"):
		path = u[strings.Index(u, ":")+1:]
	default:
		return "", "", false
	}
	path = strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// repoOriginURL returns origin's push URL for repoDir (differs from the
// fetch URL when remote.origin.pushurl is set).
func repoOriginURL(ctx context.Context, repoDir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "remote", "get-url", "--push", "origin")
	cmd.Dir = repoDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read origin of %s: %w (%s)", repoDir, err, truncate(strings.TrimSpace(stderr.String()), 200))
	}
	return strings.TrimSpace(string(out)), nil
}

// ValidateRepoDir is ValidateRepoAccess against repoDir's origin remote,
// bounded by ghValidateTimeout.
func (a *Accounts) ValidateRepoDir(ctx context.Context, user, repoDir string) error {
	if strings.TrimSpace(user) == "" {
		return ErrGhUserRequired
	}
	ctx, cancel := context.WithTimeout(ctx, ghValidateTimeout)
	defer cancel()
	origin, err := repoOriginURL(ctx, repoDir)
	if err != nil {
		return err
	}
	return a.ValidateRepoAccess(ctx, user, origin)
}

// NewRepoGhUserValidator returns the registration check (see
// Accounts.ValidateRepoDir) for callers outside the package, i.e. the
// boot-time config seed.
func NewRepoGhUserValidator(ghBin string) func(ctx context.Context, ghUser, repoDir string) error {
	return NewAccounts(ghBin).ValidateRepoDir
}
