package autopilot

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/georgehuang/admiral/internal/store"
)

// ghUserTestAccounts: virgil2019 can push to virgil2019/klink, chababa9 is
// logged in but can't see it, readonly can see it without push.
var ghUserTestAccounts = map[string]fakeGhAccount{
	"virgil2019": {loggedIn: true, repos: map[string]bool{"virgil2019/klink": true}},
	"chababa9":   {loggedIn: true, repos: map[string]bool{}},
	"readonly":   {loggedIn: true, repos: map[string]bool{"virgil2019/klink": false}},
}

// newGhUserTestServer returns an admin server over a fresh store with a fake
// gh, plus a real git repo whose origin is virgil2019/klink.
func newGhUserTestServer(t *testing.T) (*adminServer, *store.Store, *http.ServeMux, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	gitDir := filepath.Join(dir, "klink")
	for _, args := range [][]string{
		{"init", gitDir},
		{"-C", gitDir, "remote", "add", "origin", "git@virgil.github.com:virgil2019/klink.git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Skipf("git not available: %v, out: %s", err, out)
		}
	}

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "", "")
	as.gh = &ghAccounts{bin: "gh", run: fakeGhAccountsRunner(ghUserTestAccounts)}
	mux := http.NewServeMux()
	as.registerRoutesForTest(mux)
	return as, s, mux, gitDir
}

func doJSON(mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestCreateRepoHandler_GhUser(t *testing.T) {
	cases := []struct {
		name     string
		ghUser   string
		wantCode int
		wantErr  string
	}{
		{"logged in with push", "virgil2019", http.StatusCreated, ""},
		{"missing gh_user", "", http.StatusBadRequest, "gh_user is required"},
		{"not logged in", "nobody", http.StatusBadRequest, `cannot get a token for gh account \"nobody\"`},
		{"repo not visible", "chababa9", http.StatusBadRequest, "cannot see virgil2019/klink"},
		{"no push permission", "readonly", http.StatusBadRequest, "no push permission on virgil2019/klink"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, s, mux, gitDir := newGhUserTestServer(t)
			body := `{"team_id":"proj-1","team_name":"klink","repo_dir":"` + gitDir + `","gh_user":"` + c.ghUser + `"}`
			w := doJSON(mux, "POST", "/admin/repos/", body)
			if w.Code != c.wantCode {
				t.Fatalf("expected %d, got %d: %s", c.wantCode, w.Code, w.Body.String())
			}
			repo, err := s.GetRepoByProjectID("proj-1")
			if err != nil {
				t.Fatal(err)
			}
			if c.wantErr != "" {
				if !strings.Contains(w.Body.String(), c.wantErr) {
					t.Fatalf("expected body containing %q, got %s", c.wantErr, w.Body.String())
				}
				if repo != nil {
					t.Fatalf("rejected registration must not persist a row, got %+v", repo)
				}
				return
			}
			if repo == nil || repo.GhUser != c.ghUser {
				t.Fatalf("expected stored gh_user=%q, got %+v", c.ghUser, repo)
			}
		})
	}
}

func TestUpdateRepoHandler_GhUser(t *testing.T) {
	_, s, mux, gitDir := newGhUserTestServer(t)
	if err := s.UpsertRepo(store.Repo{ProjectID: "proj-1", ProjectName: "klink", RepoDir: gitDir, BaseBranch: "main", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	w := doJSON(mux, "PATCH", "/admin/repos/proj-1", `{"gh_user":"chababa9"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "cannot see virgil2019/klink") {
		t.Fatalf("expected 400 cannot-see, got %d: %s", w.Code, w.Body.String())
	}
	if repo, _ := s.GetRepoByProjectID("proj-1"); repo.GhUser != "" {
		t.Fatalf("rejected PATCH must not persist gh_user, got %q", repo.GhUser)
	}

	w = doJSON(mux, "PATCH", "/admin/repos/proj-1", `{"gh_user":"virgil2019"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp adminRepoResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.GhUser != "virgil2019" {
		t.Fatalf("expected response gh_user=virgil2019, got %q", resp.GhUser)
	}
	if repo, _ := s.GetRepoByProjectID("proj-1"); repo.GhUser != "virgil2019" {
		t.Fatalf("expected stored gh_user=virgil2019, got %q", repo.GhUser)
	}

	// Moving repo_dir re-validates the stored account against the new origin.
	otherDir := filepath.Join(t.TempDir(), "other")
	for _, args := range [][]string{
		{"init", otherDir},
		{"-C", otherDir, "remote", "add", "origin", "git@github.com:someone/else.git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	w = doJSON(mux, "PATCH", "/admin/repos/proj-1", `{"repo_dir":"`+otherDir+`"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "cannot see someone/else") {
		t.Fatalf("expected 400 on repo_dir move to inaccessible repo, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCheckGhHandler_ValidatesGhUser(t *testing.T) {
	_, s, mux, gitDir := newGhUserTestServer(t)
	for id, user := range map[string]string{"ok": "virgil2019", "bad": "chababa9", "unset": ""} {
		if err := s.UpsertRepo(store.Repo{ProjectID: id, ProjectName: id, RepoDir: gitDir, BaseBranch: "main", Enabled: true, GhUser: user}); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		id      string
		wantOK  bool
		wantOut string
	}{
		{"ok", true, "virgil2019"},
		{"bad", false, "cannot see virgil2019/klink"},
		{"unset", false, "gh_user is required"},
	}
	for _, c := range cases {
		w := doJSON(mux, "POST", "/admin/repos/"+c.id+"/check_gh", "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", c.id, w.Code, w.Body.String())
		}
		var resp checkGhResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.GHAuthOK != c.wantOK || !strings.Contains(resp.GHStatusOutput, c.wantOut) {
			t.Fatalf("%s: got %+v, want ok=%v output~%q", c.id, resp, c.wantOK, c.wantOut)
		}
	}
}

// TestCreateRepoHandler_ValidatesPushURL: push permission is checked against
// origin's push URL, not its fetch URL.
func TestCreateRepoHandler_ValidatesPushURL(t *testing.T) {
	_, s, mux, gitDir := newGhUserTestServer(t)
	if out, err := exec.Command("git", "-C", gitDir, "remote", "set-url", "--push", "origin", "git@github.com:someone/else.git").CombinedOutput(); err != nil {
		t.Fatalf("set pushurl: %v %s", err, out)
	}
	body := `{"team_id":"proj-1","team_name":"klink","repo_dir":"` + gitDir + `","gh_user":"virgil2019"}`
	w := doJSON(mux, "POST", "/admin/repos/", body)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "cannot see someone/else") {
		t.Fatalf("expected 400 against push URL, got %d: %s", w.Code, w.Body.String())
	}
	if repo, _ := s.GetRepoByProjectID("proj-1"); repo != nil {
		t.Fatalf("rejected registration persisted: %+v", repo)
	}
}
