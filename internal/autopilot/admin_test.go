package autopilot

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/georgehuang/admiral/internal/store"
)

func TestListReposHandler_Empty(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	as := newAdminServer(s, nil, slog.Default(), 3)
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

	as := newAdminServer(s, nil, slog.Default(), 3)
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

	as := newAdminServer(s, nil, slog.Default(), 3)
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

	as := newAdminServer(s, nil, slog.Default(), 3)
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

	as := newAdminServer(s, nil, slog.Default(), 3)
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

	as := newAdminServer(s, nil, slog.Default(), 3)
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

	as := newAdminServer(s, nil, slog.Default(), 3)
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

	as := newAdminServer(s, nil, slog.Default(), 3)
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

	as := newAdminServer(s, nil, slog.Default(), 3)
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

	as := newAdminServer(s, nil, slog.Default(), 5)
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

	as := newAdminServer(s, nil, slog.Default(), 3)
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
