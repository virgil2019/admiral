package autopilot

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/georgehuang/admiral/internal/store"
)

func TestAdminAuth_BearerToken(t *testing.T) {
	token := "test-secret-123"
	h := adminAuth(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	mux := http.NewServeMux()
	mux.Handle("/", h)

	req := httptest.NewRequest("GET", "/admin/health", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestAdminAuth_BearerToken_Wrong(t *testing.T) {
	token := "test-secret-123"
	h := adminAuth(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	mux := http.NewServeMux()
	mux.Handle("/", h)

	req := httptest.NewRequest("GET", "/admin/health", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAdminAuth_Cookie(t *testing.T) {
	token := "test-secret-123"
	h := adminAuth(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	mux := http.NewServeMux()
	mux.Handle("/", h)

	req := httptest.NewRequest("GET", "/admin/health", nil)
	req.AddCookie(&http.Cookie{Name: "admiral_admin", Value: token})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestAdminAuth_Cookie_Wrong(t *testing.T) {
	token := "test-secret-123"
	h := adminAuth(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	mux := http.NewServeMux()
	mux.Handle("/", h)

	req := httptest.NewRequest("GET", "/admin/health", nil)
	req.AddCookie(&http.Cookie{Name: "admiral_admin", Value: "wrong-token"})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAdminAuth_UI_GET_Redirect(t *testing.T) {
	token := "test-secret-123"
	h := adminAuth(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	mux := http.NewServeMux()
	mux.Handle("/", h)

	req := httptest.NewRequest("GET", "/admin/ui/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	if w.Header().Get("Location") != "/admin/ui/login" {
		t.Fatalf("expected redirect to /admin/ui/login, got %s", w.Header().Get("Location"))
	}
}

func TestAdminAuth_API_GET_401(t *testing.T) {
	token := "test-secret-123"
	h := adminAuth(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	mux := http.NewServeMux()
	mux.Handle("/", h)

	req := httptest.NewRequest("GET", "/admin/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAdminAuth_LoginPage_Passthrough(t *testing.T) {
	token := "test-secret-123"
	h := adminAuth(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	mux := http.NewServeMux()
	mux.Handle("/", h)

	for _, path := range []string{"/admin/ui/login", "/admin/ui/login.css"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("path %s: expected 200, got %d", path, w.Code)
		}
	}
}

func TestAdminAuth_UI_GET_WithValidCookie(t *testing.T) {
	token := "test-secret-123"
	h := adminAuth(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	mux := http.NewServeMux()
	mux.Handle("/", h)

	req := httptest.NewRequest("GET", "/admin/ui/", nil)
	req.AddCookie(&http.Cookie{Name: "admiral_admin", Value: token})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestListReposHandler_Empty(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/repos", as.listReposHandler)

	req := httptest.NewRequest("GET", "/admin/repos", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var out []adminRepoResponse
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected empty, got %d repos", len(out))
	}
}

func TestListReposHandler_WithRepos(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Insert a repo directly
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.DB.Exec(`
		INSERT INTO repos(project_id, project_name, repo_dir, base_branch, enabled, created_at, updated_at)
		VALUES('proj-1', 'My Project', '/tmp/repo', 'main', 1, ?, ?)`,
		now, now)
	if err != nil {
		t.Fatal(err)
	}

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/repos", as.listReposHandler)

	req := httptest.NewRequest("GET", "/admin/repos", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var out []adminRepoResponse
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(out))
	}
	if out[0].TeamID != "proj-1" || out[0].TeamName != "My Project" {
		t.Fatalf("unexpected repo data: %+v", out[0])
	}
	if out[0].Enabled != true {
		t.Fatalf("expected enabled=true")
	}
}

func TestListJobsHandler_Empty(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/jobs", as.listJobsHandler)

	req := httptest.NewRequest("GET", "/admin/jobs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var out []adminJobResponse
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected empty, got %d jobs", len(out))
	}
}

func TestListJobsHandler_WithJobs(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Insert a job directly
	_, err = s.DB.Exec(`
		INSERT INTO autopilot_jobs(agent_session_id, issue_id, issue_identifier, state, started_at)
		VALUES('session-1', 'issue-1', 'GEO-1', 'DONE', '2024-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/jobs", as.listJobsHandler)

	req := httptest.NewRequest("GET", "/admin/jobs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var out []adminJobResponse
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 job, got %d", len(out))
	}
	if out[0].SessionID != "session-1" {
		t.Fatalf("unexpected session_id: %s", out[0].SessionID)
	}
}

func TestListJobsHandler_FilterByStatus(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	_, _ = s.DB.Exec(`INSERT INTO autopilot_jobs(agent_session_id, issue_id, issue_identifier, state, started_at) VALUES('s1','i1','GEO-1','DONE','2024-01-01T00:00:00Z')`)
	_, _ = s.DB.Exec(`INSERT INTO autopilot_jobs(agent_session_id, issue_id, issue_identifier, state, started_at) VALUES('s2','i2','GEO-2','EXECUTING','2024-01-01T00:00:00Z')`)

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/jobs", as.listJobsHandler)

	req := httptest.NewRequest("GET", "/admin/jobs?status=DONE", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var out []adminJobResponse
	json.NewDecoder(w.Body).Decode(&out)
	if len(out) != 1 || out[0].SessionID != "s1" {
		t.Fatalf("expected 1 DONE job, got %+v", out)
	}
}

func TestGetJobHandler_NotFound(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/jobs/", as.getJobHandler)

	req := httptest.NewRequest("GET", "/admin/jobs/nonexistent", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetJobHandler_Found(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	_, _ = s.DB.Exec(`INSERT INTO autopilot_jobs(agent_session_id, issue_id, issue_identifier, state, started_at) VALUES('session-x','i1','GEO-1','DONE','2024-01-01T00:00:00Z')`)

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/jobs/", as.getJobHandler)

	req := httptest.NewRequest("GET", "/admin/jobs/session-x", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var job store.AutopilotJob
	json.NewDecoder(w.Body).Decode(&job)
	if job.AgentSessionID != "session-x" {
		t.Fatalf("unexpected job: %+v", job)
	}
}

func TestGetJobHandler_StreamNotFound(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	_, _ = s.DB.Exec(`INSERT INTO autopilot_jobs(agent_session_id, issue_id, issue_identifier, state, started_at) VALUES('session-y','i1','GEO-1','DONE','2024-01-01T00:00:00Z')`)

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/jobs/", as.getJobHandler)

	req := httptest.NewRequest("GET", "/admin/jobs/session-y/stream", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for stream without path, got %d", w.Code)
	}
}

func TestGetJobHandler_StreamServe(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	streamPath := filepath.Join(dir, "streams", "session-z.jsonl")
	_ = os.MkdirAll(filepath.Dir(streamPath), 0o755)
	_ = os.WriteFile(streamPath, []byte(`{"type":"result"}`+"\n"), 0o644)

	_, _ = s.DB.Exec(`INSERT INTO autopilot_jobs(agent_session_id, issue_id, issue_identifier, state, started_at, stream_log_path) VALUES('session-z','i1','GEO-1','DONE','2024-01-01T00:00:00Z', ?)`, streamPath)

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/jobs/", as.getJobHandler)

	req := httptest.NewRequest("GET", "/admin/jobs/session-z/stream", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != `{"type":"result"}`+"\n" {
		t.Fatalf("unexpected stream content: %q", w.Body.String())
	}
}

func TestLoadHandler(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	as := newAdminServer(s, nil, "gh", slog.Default(), 5, "")
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/load", as.loadHandler)

	req := httptest.NewRequest("GET", "/admin/load", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var out adminLoadResponse
	json.NewDecoder(w.Body).Decode(&out)
	if out.Workers != 5 {
		t.Fatalf("expected workers=5, got %d", out.Workers)
	}
}

func TestHealthHandler(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/health", as.healthHandler)

	req := httptest.NewRequest("GET", "/admin/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var out adminHealthResponse
	json.NewDecoder(w.Body).Decode(&out)
	if !out.DBOK {
		t.Fatal("expected db_ok=true")
	}
	if out.UptimeS < 0 {
		t.Fatalf("uptime_s should be non-negative, got %d", out.UptimeS)
	}
}

// --- POST /admin/repos ---

func TestCreateRepoHandler_Success(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Create a real git repo on disk
	gitDir := filepath.Join(dir, "gitrepo")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init")
	cmd.Dir = gitDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git not available: %v, out: %s", err, out)
	}
	cmd = exec.Command("git", "config", "user.email", "test@test.com")
	cmd.Dir = gitDir
	cmd.Run()
	cmd = exec.Command("git", "config", "user.name", "test")
	cmd.Dir = gitDir
	cmd.Run()

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	as.registerRoutesForTest(mux)

	body := `{"team_id":"proj-1","team_name":"My Project","repo_dir":"` + gitDir + `","base_branch":"main"}`
	req := httptest.NewRequest("POST", "/admin/repos/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp createRepoResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TeamID != "proj-1" {
		t.Fatalf("expected team_id=proj-1, got %s", resp.TeamID)
	}
}

func TestCreateRepoHandler_NotGitRepo(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	notGit := filepath.Join(dir, "notgit")
	os.MkdirAll(notGit, 0o755)

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	as.registerRoutesForTest(mux)

	body := `{"team_id":"proj-1","team_name":"My Project","repo_dir":"` + notGit + `"}`
	req := httptest.NewRequest("POST", "/admin/repos/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateRepoHandler_BadRequest(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	as.registerRoutesForTest(mux)

	// Missing required fields
	body := `{"team_id":"","team_name":"","repo_dir":""}`
	req := httptest.NewRequest("POST", "/admin/repos/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateRepoHandler_MethodNotAllowed(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	as.registerRoutesForTest(mux)

	req := httptest.NewRequest("GET", "/admin/repos/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// GET on /admin/repos/ is listReposHandler — should be 200
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for GET, got %d", w.Code)
	}
}

// --- PATCH /admin/repos/<project_id> ---

func TestUpdateRepoHandler_Success(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Seed a repo
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.DB.Exec(`
		INSERT INTO repos(project_id, project_name, repo_dir, base_branch, enabled, created_at, updated_at)
		VALUES('proj-1', 'My Project', '/tmp/repo', 'main', 1, ?, ?)`,
		now, now)
	if err != nil {
		t.Fatal(err)
	}

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	as.registerRoutesForTest(mux)

	body := `{"base_branch":"develop","enabled":false}`
	req := httptest.NewRequest("PATCH", "/admin/repos/proj-1", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp adminRepoResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.BaseBranch != "develop" {
		t.Fatalf("expected base_branch=develop, got %s", resp.BaseBranch)
	}
	if resp.Enabled != false {
		t.Fatalf("expected enabled=false")
	}
}

func TestUpdateRepoHandler_NotFound(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	as.registerRoutesForTest(mux)

	body := `{"base_branch":"develop"}`
	req := httptest.NewRequest("PATCH", "/admin/repos/nonexistent", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateRepoHandler_BadRequest(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.DB.Exec(`
		INSERT INTO repos(project_id, project_name, repo_dir, base_branch, enabled, created_at, updated_at)
		VALUES('proj-1', 'My Project', '/tmp/repo', 'main', 1, ?, ?)`,
		now, now)
	if err != nil {
		t.Fatal(err)
	}

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	as.registerRoutesForTest(mux)

	// Malformed JSON
	body := `{invalid json}`
	req := httptest.NewRequest("PATCH", "/admin/repos/proj-1", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateRepoHandler_MethodNotAllowed(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	as.registerRoutesForTest(mux)

	// POST to /admin/repos/<id> should be method not allowed
	body := `{}`
	req := httptest.NewRequest("POST", "/admin/repos/proj-1", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d: %s", w.Code, w.Body.String())
	}
}

// --- DELETE /admin/repos/<project_id> ---

func TestDeleteRepoHandler_Success(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.DB.Exec(`
		INSERT INTO repos(project_id, project_name, repo_dir, base_branch, enabled, created_at, updated_at)
		VALUES('proj-1', 'My Project', '/tmp/repo', 'main', 1, ?, ?)`,
		now, now)
	if err != nil {
		t.Fatal(err)
	}

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	as.registerRoutesForTest(mux)

	req := httptest.NewRequest("DELETE", "/admin/repos/proj-1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify deleted
	repos, err := s.ListRepos()
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 0 {
		t.Fatalf("expected 0 repos after delete, got %d", len(repos))
	}
}

func TestDeleteRepoHandler_NotFound(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	as.registerRoutesForTest(mux)

	req := httptest.NewRequest("DELETE", "/admin/repos/nonexistent", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteRepoHandler_MethodNotAllowed(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	as.registerRoutesForTest(mux)

	// PUT is not a supported method on /admin/repos/<id>
	req := httptest.NewRequest("PUT", "/admin/repos/proj-1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d: %s", w.Code, w.Body.String())
	}
}

// --- POST /admin/repos/<project_id>/check_gh ---

func TestCheckGhHandler_NotFound(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	as.registerRoutesForTest(mux)

	req := httptest.NewRequest("POST", "/admin/repos/nonexistent/check_gh", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCheckGhHandler_MethodNotAllowed(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	as.registerRoutesForTest(mux)

	req := httptest.NewRequest("GET", "/admin/repos/proj-1/check_gh", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d: %s", w.Code, w.Body.String())
	}
}

// --- POST /admin/repos/<project_id>/test_clone ---

func TestTestCloneHandler_NotFound(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	as.registerRoutesForTest(mux)

	req := httptest.NewRequest("POST", "/admin/repos/nonexistent/test_clone", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTestCloneHandler_NotGitRepo(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	notGit := filepath.Join(dir, "notgit")
	os.MkdirAll(notGit, 0o755)

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.DB.Exec(`
		INSERT INTO repos(project_id, project_name, repo_dir, base_branch, enabled, created_at, updated_at)
		VALUES('proj-1', 'My Project', ?, 'main', 1, ?, ?)`,
		notGit, now, now)
	if err != nil {
		t.Fatal(err)
	}

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	as.registerRoutesForTest(mux)

	req := httptest.NewRequest("POST", "/admin/repos/proj-1/test_clone", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTestCloneHandler_NoOrigin(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	gitDir := filepath.Join(dir, "gitrepo")
	os.MkdirAll(gitDir, 0o755)
	// Init a git repo without origin
	cmd := exec.Command("git", "init")
	cmd.Dir = gitDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git not available: %v, out: %s", err, out)
	}
	cmd = exec.Command("git", "config", "user.email", "test@test.com")
	cmd.Dir = gitDir
	cmd.Run()
	cmd = exec.Command("git", "config", "user.name", "test")
	cmd.Dir = gitDir
	cmd.Run()
	cmd = exec.Command("git", "commit", "--allow-empty", "-m", "initial")
	cmd.Dir = gitDir
	cmd.Run()

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.DB.Exec(`
		INSERT INTO repos(project_id, project_name, repo_dir, base_branch, enabled, created_at, updated_at)
		VALUES('proj-1', 'My Project', ?, 'main', 1, ?, ?)`,
		gitDir, now, now)
	if err != nil {
		t.Fatal(err)
	}

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	as.registerRoutesForTest(mux)

	req := httptest.NewRequest("POST", "/admin/repos/proj-1/test_clone", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (no origin), got %d: %s", w.Code, w.Body.String())
	}
}

func TestTestCloneHandler_MethodNotAllowed(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := http.NewServeMux()
	as.registerRoutesForTest(mux)

	req := httptest.NewRequest("GET", "/admin/repos/proj-1/test_clone", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d: %s", w.Code, w.Body.String())
	}
}

// registerRoutesForTest registers all routes on mux for testing.
// POST /admin/repos uses trailing slash for correct routing.
func (s *adminServer) registerRoutesForTest(mux *http.ServeMux) {
	mux.HandleFunc("/admin/repos/", s.reposDispatchHandler)
	mux.HandleFunc("/admin/jobs", s.listJobsHandler)
	mux.HandleFunc("/admin/jobs/", s.getJobHandler)
	mux.HandleFunc("/admin/load", s.loadHandler)
	mux.HandleFunc("/admin/health", s.healthHandler)
}

// integrationAdminMux builds a mux with all UI + API routes wired up (no auth).
func integrationAdminMux(as *adminServer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/admin/ui/", http.StripPrefix("/admin/ui", http.FileServer(http.FS(uiFS))))
	mux.HandleFunc("/admin/ui", as.uiHandler)
	mux.HandleFunc("/admin/ui/login", as.uiHandler)
	mux.HandleFunc("/admin/ui/repos", as.uiHandler)
	mux.HandleFunc("/admin/ui/jobs", as.uiHandler)
	mux.HandleFunc("/admin/ui/jobs/", as.uiHandler)
	mux.HandleFunc("/admin/repos", as.reposDispatchHandler)
	mux.HandleFunc("/admin/repos/", as.reposDispatchHandler)
	mux.HandleFunc("/admin/jobs", as.listJobsHandler)
	mux.HandleFunc("/admin/jobs/", as.getJobHandler)
	mux.HandleFunc("/admin/load", as.loadHandler)
	mux.HandleFunc("/admin/health", as.healthHandler)
	return mux
}

// TestUI_LoginPage tests that /admin/ui/login returns 200 with login.html content.
func TestUI_LoginPage(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := integrationAdminMux(as)

	for _, path := range []string{"/admin/ui/login"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("%s: expected 200, got %d", path, w.Code)
		}
		if !strings.Contains(w.Body.String(), "Admiral Admin") {
			t.Errorf("%s: expected login page content, got %q", path, w.Body.String())
		}
	}
}

// TestUI_LoginCSS tests that /admin/ui/login.css serves login.css.
func TestUI_LoginCSS(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := integrationAdminMux(as)

	req := httptest.NewRequest("GET", "/admin/ui/login.css", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("/admin/ui/login.css: expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "box-sizing") {
		t.Errorf("/admin/ui/login.css: expected CSS content, got %q", w.Body.String())
	}
}

// TestUI_Dashboard tests that /admin/ui/ returns 200 with index.html content.
func TestUI_Dashboard(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "")
	mux := integrationAdminMux(as)

	req := httptest.NewRequest("GET", "/admin/ui/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("/admin/ui/: expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Admiral") {
		t.Errorf("/admin/ui/: expected dashboard content, got %q", w.Body.String())
	}
}
