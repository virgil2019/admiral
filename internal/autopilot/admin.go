package autopilot

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

//go:embed static
var staticFS embed.FS

// uiFS is the fs.FS rooted at "static" (via fs.Sub), used for ReadFile.
var uiFS fs.FS = must(fs.Sub(staticFS, "static"))

func must(fsys fs.FS, err error) fs.FS {
	if err != nil {
		panic("static FS setup failed: " + err.Error())
	}
	return fsys
}

// adminServer serves the admin HTTP API (read + write).
type adminServer struct {
	db         *store.Store
	lc         *linear.Client
	logger     *slog.Logger
	ghBin      string
	start      time.Time
	workers    int
	adminToken string
}

// adminRepoResponse is the JSON shape for /admin/repos.
type adminRepoResponse struct {
	TeamID     string `json:"team_id"`
	TeamName   string `json:"project_name"`
	RepoDir    string `json:"repo_dir"`
	BaseBranch string `json:"base_branch"`
	Enabled    bool   `json:"enabled"`
}

// adminJobResponse is the JSON shape for /admin/jobs list.
type adminJobResponse struct {
	SessionID       string `json:"session_id"`
	IssueIdentifier string `json:"issue_identifier"`
	State           string `json:"state"`
	StartedAt       string `json:"started_at"`
	FinishedAt      string `json:"finished_at,omitempty"`
	PRURL           string `json:"pr_url,omitempty"`
	RepoDir         string `json:"repo_dir,omitempty"`
	StreamLogPath   string `json:"stream_log_path,omitempty"`
	ClaudeSessionID string `json:"claude_session_id,omitempty"`
}

// adminLoadResponse is the JSON shape for /admin/load.
type adminLoadResponse struct {
	Workers          int `json:"workers"`
	PendingEvents    int `json:"pending_events"`
	ProcessingEvents int `json:"processing_events"`
	InFlightJobs     int `json:"in_flight_jobs"`
}

// adminHealthResponse is the JSON shape for /admin/health.
type adminHealthResponse struct {
	OK               bool `json:"ok"`
	UptimeS          int  `json:"uptime_s"`
	DBOK             bool `json:"db_ok"`
	LinearTokenValid bool `json:"linear_token_valid"`
}

// --- Write API request/response types ---

// createRepoRequest is the body for POST /admin/repos.
type createRepoRequest struct {
	TeamID     string `json:"team_id"`
	TeamName   string `json:"team_name"`
	RepoDir    string `json:"repo_dir"`
	BaseBranch string `json:"base_branch"`
}

// createRepoResponse is the body for POST /admin/repos.
type createRepoResponse struct {
	TeamID string `json:"team_id"`
}

// updateRepoRequest is the body for PATCH /admin/repos/<team_id>.
type updateRepoRequest struct {
	RepoDir    *string `json:"repo_dir,omitempty"`
	BaseBranch *string `json:"base_branch,omitempty"`
	Enabled    *bool   `json:"enabled,omitempty"`
}

// checkGhResponse is the body for POST /admin/repos/<team_id>/check_gh.
type checkGhResponse struct {
	GHAuthOK       bool   `json:"gh_auth_ok"`
	CurrentUser    string `json:"current_user,omitempty"`
	GHStatusOutput string `json:"gh_status_output,omitempty"`
}

// testCloneResponse is the body for POST /admin/repos/<team_id>/test_clone.
type testCloneResponse struct {
	OK        bool   `json:"ok"`
	OriginURL string `json:"origin_url,omitempty"`
	Error     string `json:"error,omitempty"`
}

func newAdminServer(db *store.Store, lc *linear.Client, ghBin string, logger *slog.Logger, workers int, adminToken string) *adminServer {
	return &adminServer{db: db, lc: lc, ghBin: ghBin, logger: logger, start: time.Now(), workers: workers, adminToken: adminToken}
}

// projectFilterCookie is the name of the cookie that scopes the admin UI
// (repos table + jobs lists) to a single Linear project. Empty value or
// missing cookie means "all projects".
const projectFilterCookie = "admiral_project"

// projectFilter returns the project_id selected by the admin UI's project
// switcher. Empty string means no filter.
func projectFilter(r *http.Request) string {
	c, err := r.Cookie(projectFilterCookie)
	if err != nil || c == nil {
		return ""
	}
	return strings.TrimSpace(c.Value)
}

// --- Read handlers ---

func (s *adminServer) listReposHandler(w http.ResponseWriter, r *http.Request) {
	repos, err := s.db.ListRepos()
	if err != nil {
		s.logger.Warn("admin_list_repos_failed", "err", err)
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	out := make([]adminRepoResponse, 0, len(repos))
	for _, r := range repos {
		out = append(out, adminRepoResponse{
			TeamID:     r.ProjectID,
			TeamName:   r.ProjectName,
			RepoDir:    r.RepoDir,
			BaseBranch: r.BaseBranch,
			Enabled:    r.Enabled,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *adminServer) listJobsHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	status := r.Form.Get("status")
	teamID := r.Form.Get("team_id")
	limit := 50
	if l := r.Form.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	var since *time.Time
	if sinceStr := r.Form.Get("since"); sinceStr != "" {
		if t, err := time.Parse(time.RFC3339, sinceStr); err == nil {
			since = &t
		}
	}

	jobs, err := s.db.ListAutopilotJobs(status, teamID, since, limit)
	if err != nil {
		s.logger.Warn("admin_list_jobs_failed", "err", err)
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	out := make([]adminJobResponse, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, adminJobResponse{
			SessionID:       j.AgentSessionID,
			IssueIdentifier: j.IssueIdentifier,
			State:           j.State,
			StartedAt:       j.StartedAt,
			FinishedAt:      j.FinishedAt,
			PRURL:           j.PRURL,
			StreamLogPath:   j.StreamLogPath,
			ClaudeSessionID: j.ClaudeSessionID,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *adminServer) getJobHandler(w http.ResponseWriter, r *http.Request) {
	// Strip /admin/jobs/ prefix to get session_id
	sessionID := strings.TrimPrefix(r.URL.Path, "/admin/jobs/")
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		http.Error(w, `{"error":"session_id required"}`, http.StatusBadRequest)
		return
	}

	// Handle /admin/jobs/<id>/stream
	if strings.HasSuffix(sessionID, "/stream") {
		sessionID = strings.TrimSuffix(sessionID, "/stream")
		streamHandler := s.streamJobHandler(sessionID)
		streamHandler(w, r)
		return
	}

	job, err := s.db.GetAutopilotJob(sessionID)
	if err != nil {
		s.logger.Warn("admin_get_job_failed", "err", err)
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	if job == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(job)
}

func (s *adminServer) streamJobHandler(sessionID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		job, err := s.db.GetAutopilotJob(sessionID)
		if err != nil {
			s.logger.Warn("admin_get_job_stream_failed", "err", err)
			http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
			return
		}
		if job == nil || job.StreamLogPath == "" {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		http.ServeFile(w, r, job.StreamLogPath)
	}
}

func (s *adminServer) loadHandler(w http.ResponseWriter, r *http.Request) {
	pending, _ := s.db.CountPendingEvents()

	var processing int
	s.db.DB.QueryRow(`
		SELECT COUNT(*) FROM events_inbox WHERE status='processing'
	`).Scan(&processing)

	inFlight, _, _ := s.db.AnyAutopilotJobActive()
	inFlightJobs := 0
	if inFlight {
		inFlightJobs = 1
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(adminLoadResponse{
		Workers:          s.workers,
		PendingEvents:    pending,
		ProcessingEvents: processing,
		InFlightJobs:     inFlightJobs,
	})
}

func (s *adminServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	dbOK := true
	tok, err := s.db.GetLinearOAuthToken()
	linearOK := err == nil && tok != nil && tok.AccessToken != ""

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(adminHealthResponse{
		OK:               dbOK && linearOK,
		UptimeS:          int(time.Since(s.start).Seconds()),
		DBOK:             dbOK,
		LinearTokenValid: linearOK,
	})
}

// --- Write handlers ---

func (s *adminServer) createRepoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	// htmx posts the add-repo form as application/x-www-form-urlencoded; the
	// JSON path is kept for direct API callers.
	isForm := strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded")
	var req createRepoRequest
	if isForm {
		if err := r.ParseForm(); err != nil {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
		req.TeamID = r.Form.Get("team_id")
		req.TeamName = r.Form.Get("team_name")
		req.RepoDir = r.Form.Get("repo_dir")
		req.BaseBranch = r.Form.Get("base_branch")
	} else {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
	}
	if strings.TrimSpace(req.TeamID) == "" || strings.TrimSpace(req.RepoDir) == "" {
		http.Error(w, `{"error":"team_id and repo_dir are required"}`, http.StatusBadRequest)
		return
	}
	baseBranch := req.BaseBranch
	if strings.TrimSpace(baseBranch) == "" {
		baseBranch = "main"
	}

	// Validate repo_dir exists and is a git repo
	if !isGitRepo(req.RepoDir) {
		http.Error(w, `{"error":"repo_dir is not a git repository or does not exist"}`, http.StatusBadRequest)
		return
	}

	// Validate team_id exists in Linear (if lc is available); reuse the
	// returned project name when the caller didn't supply one (form path).
	if s.lc != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		proj, err := s.lc.GetProject(ctx, req.TeamID)
		if err != nil {
			s.logger.Warn("create_repo_linear_validation_failed", "team_id", req.TeamID, "err", err)
			http.Error(w, `{"error":"invalid team_id (project not found in Linear)"}`, http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.TeamName) == "" && proj != nil {
			req.TeamName = proj.Name
		}
	}
	if strings.TrimSpace(req.TeamName) == "" {
		http.Error(w, `{"error":"team_name is required (Linear lookup unavailable)"}`, http.StatusBadRequest)
		return
	}

	repo := store.Repo{
		ProjectID:   req.TeamID,
		ProjectName: req.TeamName,
		RepoDir:     req.RepoDir,
		BaseBranch:  baseBranch,
		Enabled:     true,
	}
	if err := s.db.UpsertRepo(repo); err != nil {
		s.logger.Warn("admin_create_repo_failed", "err", err)
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	if isForm {
		// htmx swap target is the repos table; respond with the same fragment
		// so the new row appears immediately.
		s.reposTableHandler(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(createRepoResponse{TeamID: req.TeamID})
}

// listLinearProjectsHandler returns all Linear projects as JSON for the
// add-repo form's project picker.
func (s *adminServer) listLinearProjectsHandler(w http.ResponseWriter, r *http.Request) {
	if s.lc == nil {
		http.Error(w, `{"error":"linear client not configured"}`, http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	projects, err := s.lc.ListProjects(ctx)
	if err != nil {
		s.logger.Warn("admin_list_linear_projects_failed", "err", err)
		http.Error(w, `{"error":"linear list_projects failed"}`, http.StatusBadGateway)
		return
	}
	type projOut struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	out := make([]projOut, 0, len(projects))
	for _, p := range projects {
		out = append(out, projOut{ID: p.ID, Name: p.Name})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// selectProjectHandler writes the project filter cookie from a form POST and
// redirects back to the page that submitted (or the dashboard).
func (s *adminServer) selectProjectHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	projectID := strings.TrimSpace(r.Form.Get("project_id"))
	cookie := &http.Cookie{
		Name:     projectFilterCookie,
		Value:    projectID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	if projectID == "" {
		// "All projects": clear the cookie.
		cookie.MaxAge = -1
	} else {
		cookie.MaxAge = 30 * 24 * 60 * 60
	}
	http.SetCookie(w, cookie)
	dest := r.Header.Get("Referer")
	if dest == "" {
		dest = "/admin/ui/"
	}
	// htmx follows the redirect via HX-Redirect for full-page reload, which
	// re-runs every partial against the new cookie.
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", dest)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// projectSwitcherHandler returns the global project-switcher select element,
// scoped to currently-configured projects (one per repo row).
func (s *adminServer) projectSwitcherHandler(w http.ResponseWriter, r *http.Request) {
	repos, _ := s.db.ListRepos()
	current := projectFilter(r)
	w.Header().Set("Content-Type", "text/html")
	var b strings.Builder
	b.WriteString(`<form class="project-switcher" hx-post="/admin/ui/select_project" hx-trigger="change">`)
	b.WriteString(`<label>Project: <select name="project_id">`)
	selAll := ""
	if current == "" {
		selAll = " selected"
	}
	fmt.Fprintf(&b, `<option value=""%s>All projects</option>`, selAll)
	for _, repo := range repos {
		sel := ""
		if repo.ProjectID == current {
			sel = " selected"
		}
		fmt.Fprintf(&b, `<option value="%s"%s>%s</option>`, html.EscapeString(repo.ProjectID), sel, html.EscapeString(repo.ProjectName))
	}
	b.WriteString(`</select></label></form>`)
	w.Write([]byte(b.String()))
}

// linearProjectsOptionsHandler returns <option> tags for the add-repo form,
// pulling from Linear directly. Errors render as a single disabled option so
// the form stays usable for the rare case the user has no Linear access.
func (s *adminServer) linearProjectsOptionsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	if s.lc == nil {
		w.Write([]byte(`<option value="" disabled>Linear not configured</option>`))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	projects, err := s.lc.ListProjects(ctx)
	if err != nil {
		s.logger.Warn("admin_options_linear_failed", "err", err)
		w.Write([]byte(`<option value="" disabled>Failed to load Linear projects</option>`))
		return
	}
	var b strings.Builder
	b.WriteString(`<option value="">— select a project —</option>`)
	for _, p := range projects {
		fmt.Fprintf(&b, `<option value="%s">%s</option>`, html.EscapeString(p.ID), html.EscapeString(p.Name))
	}
	w.Write([]byte(b.String()))
}


func (s *adminServer) updateRepoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	projectID := strings.TrimPrefix(r.URL.Path, "/admin/repos/")
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		http.Error(w, `{"error":"project_id required"}`, http.StatusBadRequest)
		return
	}

	// Check repo exists
	existing, err := s.db.GetRepoByProjectID(projectID)
	if err != nil {
		s.logger.Warn("admin_update_repo_db_err", "err", err)
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	if existing == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}

	var req updateRepoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}

	updated := *existing
	if req.RepoDir != nil {
		if !isGitRepo(*req.RepoDir) {
			http.Error(w, `{"error":"repo_dir is not a git repository or does not exist"}`, http.StatusBadRequest)
			return
		}
		updated.RepoDir = *req.RepoDir
	}
	if req.BaseBranch != nil {
		updated.BaseBranch = *req.BaseBranch
	}
	if req.Enabled != nil {
		updated.Enabled = *req.Enabled
	}

	if err := s.db.UpsertRepo(updated); err != nil {
		s.logger.Warn("admin_update_repo_failed", "err", err)
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(adminRepoResponse{
		TeamID:     updated.ProjectID,
		TeamName:   updated.ProjectName,
		RepoDir:    updated.RepoDir,
		BaseBranch: updated.BaseBranch,
		Enabled:    updated.Enabled,
	})
}

func (s *adminServer) deleteRepoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	projectID := strings.TrimPrefix(r.URL.Path, "/admin/repos/")
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		http.Error(w, `{"error":"project_id required"}`, http.StatusBadRequest)
		return
	}

	// Check repo exists
	existing, err := s.db.GetRepoByProjectID(projectID)
	if err != nil {
		s.logger.Warn("admin_delete_repo_db_err", "err", err)
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	if existing == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}

	if err := s.db.DeleteRepo(projectID); err != nil {
		s.logger.Warn("admin_delete_repo_failed", "err", err)
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *adminServer) checkGhAuthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	projectID := strings.TrimPrefix(r.URL.Path, "/admin/repos/")
	projectID = strings.TrimSpace(strings.TrimSuffix(projectID, "/check_gh"))
	if projectID == "" {
		http.Error(w, `{"error":"project_id required"}`, http.StatusBadRequest)
		return
	}

	repo, err := s.db.GetRepoByProjectID(projectID)
	if err != nil {
		s.logger.Warn("admin_check_gh_db_err", "err", err)
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	if repo == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Detect GitHub host from repo remote origin
	host := "github.com" // default
	originOut, err := captureCmd(ctx, repo.RepoDir, "git", "remote", "get-url", "origin")
	if err == nil && strings.Contains(originOut, "github.com") {
		host = "github.com"
	} else if err == nil {
		// Try to extract hostname from URL
		lines := strings.Split(strings.TrimSpace(originOut), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "http") || strings.HasPrefix(line, "git@") {
				if idx := strings.Index(line, "@"); idx != -1 {
					hostPart := strings.TrimSuffix(strings.Split(line, "@")[1], ":")
					hostPart = strings.TrimSuffix(hostPart, ".git")
					if hostPart != "" {
						host = hostPart
					}
				}
			}
		}
	}

	status, err := checkGhAuth(ctx, repo.RepoDir, s.ghBin, host)
	if err != nil {
		s.logger.Warn("admin_check_gh_failed", "err", err)
		http.Error(w, `{"error":"gh check failed"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(checkGhResponse{
		GHAuthOK:       status.OK,
		CurrentUser:    status.CurrentUser,
		GHStatusOutput: status.StatusOutput,
	})
}

func (s *adminServer) testCloneHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	projectID := strings.TrimPrefix(r.URL.Path, "/admin/repos/")
	projectID = strings.TrimSpace(strings.TrimSuffix(projectID, "/test_clone"))
	if projectID == "" {
		http.Error(w, `{"error":"project_id required"}`, http.StatusBadRequest)
		return
	}

	repo, err := s.db.GetRepoByProjectID(projectID)
	if err != nil {
		s.logger.Warn("admin_test_clone_db_err", "err", err)
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	if repo == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// 1. Verify repo_dir is a git repo
	if !isGitRepo(repo.RepoDir) {
		http.Error(w, `{"error":"repo_dir is not a git repository or does not exist"}`, http.StatusBadRequest)
		return
	}

	// 2. Get origin URL
	originOut, err := captureCmd(ctx, repo.RepoDir, "git", "remote", "get-url", "origin")
	if err != nil {
		http.Error(w, `{"error":"no remote origin configured"}`, http.StatusBadRequest)
		return
	}
	originURL := strings.TrimSpace(originOut)
	if originURL == "" {
		http.Error(w, `{"error":"remote origin has no URL"}`, http.StatusBadRequest)
		return
	}

	// 3. Try git fetch to verify connectivity/auth
	_, err = captureCmd(ctx, repo.RepoDir, "git", "fetch", "origin")
	if err != nil {
		// fetch failed — likely auth issue but we report it
		http.Error(w, fmt.Sprintf(`{"error":"git fetch failed: %v"}`, err), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(testCloneResponse{
		OK:        true,
		OriginURL: originURL,
	})
}

// isGitRepo returns true if dir is a valid git repository.
func isGitRepo(dir string) bool {
	out, err := captureCmd(context.Background(), dir, "git", "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "true"
}

// --- UI handlers ---

// loginPostHandler accepts a form-submitted admin token, verifies it
// against the configured token, and sets the HttpOnly admiral_admin
// cookie via Set-Cookie before redirecting to the dashboard. This
// replaces the previous client-side cookie write in login.html, which
// the browser silently rejected when HttpOnly was set from JS (Safari)
// and which could not actually be HttpOnly-guarded against XSS anyway.
//
// On rejection the handler redirects back to /admin/ui/login?error=invalid
// so the page can render an inline error message. Constant-time
// comparison is intentionally not used: the admin token is local-only
// and the timing channel here is a localhost form POST.
func (s *adminServer) loginPostHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	token := r.Form.Get("token")
	if token == "" || token != s.adminToken {
		http.Redirect(w, r, "/admin/ui/login?error=invalid", http.StatusFound)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "admiral_admin",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   24 * 60 * 60,
	})
	http.Redirect(w, r, "/admin/ui/", http.StatusFound)
}

func (s *adminServer) uiHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	var name string
	switch path {
	case "/admin/ui", "/admin/ui/":
		name = "index.html"
	case "/admin/ui/login":
		name = "login.html"
	case "/admin/ui/repos":
		name = "repos.html"
	case "/admin/ui/jobs", "/admin/ui/jobs/":
		name = "jobs.html"
	default:
		if strings.HasPrefix(path, "/admin/ui/jobs/") {
			name = "job-detail.html"
		} else {
			http.NotFound(w, r)
			return
		}
	}
	data, err := fs.ReadFile(uiFS, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.Write(data)
}

// loadCardHandler returns the dashboard load card HTML fragment.
func (s *adminServer) loadCardHandler(w http.ResponseWriter, r *http.Request) {
	pending, _ := s.db.CountPendingEvents()
	var processing int
	s.db.DB.QueryRow(`SELECT COUNT(*) FROM events_inbox WHERE status='processing'`).Scan(&processing)
	inFlight, _, _ := s.db.AnyAutopilotJobActive()
	inFlightJobs := 0
	if inFlight {
		inFlightJobs = 1
	}
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<div class="card">
<div class="stat"><span class="label">Workers</span><span class="value">%d</span></div>
<div class="stat"><span class="label">Pending</span><span class="value">%d</span></div>
<div class="stat"><span class="label">Processing</span><span class="value">%d</span></div>
<div class="stat"><span class="label">In-Flight</span><span class="value">%d</span></div>
</div>`, s.workers, pending, processing, inFlightJobs)
}

// recentJobsHandler returns the 5 most recent jobs HTML fragment.
func (s *adminServer) recentJobsHandler(w http.ResponseWriter, r *http.Request) {
	jobs, _ := s.db.ListAutopilotJobsByProject(projectFilter(r), "", "", nil, 5)
	w.Header().Set("Content-Type", "text/html")
	if len(jobs) == 0 {
		w.Write([]byte(`<p>No jobs yet.</p>`))
		return
	}
	var b strings.Builder
	b.WriteString(`<table><thead><tr><th>ID</th><th>Issue</th><th>State</th><th>Started</th></tr></thead><tbody>`)
	for _, j := range jobs {
		fmt.Fprintf(&b, `<tr><td><a href="/admin/ui/jobs/%s">%s</a></td><td>%s</td><td><span class="badge badge-%s">%s</span></td><td>%s</td></tr>`,
			j.AgentSessionID, j.AgentSessionID, j.IssueIdentifier, j.State, j.State, j.StartedAt)
	}
	b.WriteString(`</tbody></table>`)
	w.Write([]byte(b.String()))
}

// reposTableHandler returns the repos table HTML fragment.
func (s *adminServer) reposTableHandler(w http.ResponseWriter, r *http.Request) {
	all, _ := s.db.ListRepos()
	filter := projectFilter(r)
	var repos []store.Repo
	if filter == "" {
		repos = all
	} else {
		for _, repo := range all {
			if repo.ProjectID == filter {
				repos = append(repos, repo)
			}
		}
	}
	w.Header().Set("Content-Type", "text/html")
	if len(repos) == 0 {
		w.Write([]byte(`<p>No repos configured.</p>`))
		return
	}
	var b strings.Builder
	b.WriteString(`<table><thead><tr><th>Project</th><th>Repo Dir</th><th>Branch</th><th>Enabled</th><th>Actions</th></tr></thead><tbody>`)
	for _, r := range repos {
		enabled := "Yes"
		if !r.Enabled {
			enabled = "No"
		}
		fmt.Fprintf(&b, `<tr>
<td>%s</td><td>%s</td><td>%s</td><td>%s</td>
<td>
<button class="secondary" hx-post="/admin/repos/%s/check_gh" hx-swap="none">Test GH</button>
<button class="danger" hx-delete="/admin/repos/%s" hx-confirm="Delete %s?" hx-swap="none">Delete</button>
</td></tr>`, r.ProjectName, r.RepoDir, r.BaseBranch, enabled, r.ProjectID, r.ProjectID, r.ProjectName)
	}
	b.WriteString(`</tbody></table>`)
	w.Write([]byte(b.String()))
}

// jobsTableHandler returns filtered jobs table HTML fragment.
func (s *adminServer) jobsTableHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	status := r.Form.Get("status")
	teamID := r.Form.Get("team_id")
	jobs, _ := s.db.ListAutopilotJobsByProject(projectFilter(r), status, teamID, nil, 100)
	w.Header().Set("Content-Type", "text/html")
	if len(jobs) == 0 {
		w.Write([]byte(`<p>No jobs found.</p>`))
		return
	}
	var b strings.Builder
	b.WriteString(`<table><thead><tr><th>Session ID</th><th>Issue</th><th>State</th><th>Started</th><th>Finished</th></tr></thead><tbody>`)
	for _, j := range jobs {
		fmt.Fprintf(&b, `<tr><td><a href="/admin/ui/jobs/%s">%s</a></td><td>%s</td><td><span class="badge badge-%s">%s</span></td><td>%s</td><td>%s</td></tr>`,
			j.AgentSessionID, j.AgentSessionID, j.IssueIdentifier, j.State, j.State, j.StartedAt, j.FinishedAt)
	}
	b.WriteString(`</tbody></table>`)
	w.Write([]byte(b.String()))
}

// jobDetailHandler returns a single job detail HTML fragment.
func (s *adminServer) jobDetailHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimPrefix(r.URL.Path, "/admin/ui/_partial/job_detail/")
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}
	job, err := s.db.GetAutopilotJob(sessionID)
	if err != nil || job == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	streamLink := ""
	if job.StreamLogPath != "" {
		streamLink = fmt.Sprintf(`<a href="/admin/jobs/%s/stream">Stream Log</a>`, sessionID)
	}
	fmt.Fprintf(w, `<div class="card">
<div class="detail-row"><span class="label">Session ID</span><span class="value">%s</span></div>
<div class="detail-row"><span class="label">Issue</span><span class="value">%s</span></div>
<div class="detail-row"><span class="label">State</span><span class="value"><span class="badge badge-%s">%s</span></span></div>
<div class="detail-row"><span class="label">PR URL</span><span class="value">%s</span></div>
<div class="detail-row"><span class="label">Claude Session</span><span class="value">%s</span></div>
<div class="detail-row"><span class="label">Worktree</span><span class="value">%s</span></div>
<div class="detail-row"><span class="label">Started</span><span class="value">%s</span></div>
<div class="detail-row"><span class="label">Finished</span><span class="value">%s</span></div>
<div class="detail-row"><span class="label">Stream Log</span><span class="value">%s</span></div>
<div class="detail-row"><span class="label">Error</span><span class="value">%s</span></div>
</div>`, job.AgentSessionID, job.IssueIdentifier, job.State, job.State, job.PRURL, job.ClaudeSessionID, job.WorktreePath, job.StartedAt, job.FinishedAt, streamLink, job.Error)
}

// adminAuth returns an http.Handler that enforces Bearer token auth.
// Paths /admin/ui/login and /admin/ui/login.css are always passed through.
// Cookie auth via "admiral_admin" cookie is also accepted.
// GET requests to /admin/ui/* are redirected to /admin/ui/login on auth failure.
// All other requests receive 401 on auth failure.
func adminAuth(token string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/ui/login" || r.URL.Path == "/admin/ui/login.css" {
			h.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth == "Bearer "+token {
			h.ServeHTTP(w, r)
			return
		}
		if c, _ := r.Cookie("admiral_admin"); c != nil && c.Value == token {
			h.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/admin/ui") && r.Method == "GET" {
			http.Redirect(w, r, "/admin/ui/login", http.StatusFound)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// ServeAdminHTTP starts the admin HTTP server on addr.
// If adminToken is empty the server is disabled (logged as warn) and returns nil immediately.
func ServeAdminHTTP(addr string, db *store.Store, lc *linear.Client, ghBin string, logger *slog.Logger, workers int, adminToken string) error {
	if adminToken == "" {
		logger.Warn("admin server disabled: autopilot.admin_token not set; set ADMIRAL_ADMIN_TOKEN env or autopilot.admin_token in config to enable")
		return nil
	}
	as := newAdminServer(db, lc, ghBin, logger, workers, adminToken)
	mux := newAdminMux(as, adminToken)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

func newAdminMux(as *adminServer, adminToken string) *http.ServeMux {
	mux := http.NewServeMux()
	// Wrap all handlers with auth middleware
	wrapped := adminAuth(adminToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		as.serveMux().ServeHTTP(w, r)
	}))
	mux.Handle("/", wrapped)
	return mux
}

// serveMux returns the inner mux with all routes registered (no auth).
func (s *adminServer) serveMux() *http.ServeMux {
	mux := http.NewServeMux()
	// UI static files
	mux.Handle("/admin/ui/", http.StripPrefix("/admin/ui", http.FileServer(http.FS(uiFS))))
	// UI page routes
	mux.HandleFunc("/admin/ui", s.uiHandler)
	mux.HandleFunc("/admin/ui/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			s.loginPostHandler(w, r)
			return
		}
		s.uiHandler(w, r)
	})
	mux.HandleFunc("/admin/ui/repos", s.uiHandler)
	mux.HandleFunc("/admin/ui/jobs", s.uiHandler)
	mux.HandleFunc("/admin/ui/jobs/", s.uiHandler)
	// htmx partials
	mux.HandleFunc("/admin/ui/_partial/load_card", s.loadCardHandler)
	mux.HandleFunc("/admin/ui/_partial/recent_jobs", s.recentJobsHandler)
	mux.HandleFunc("/admin/ui/_partial/repos_table", s.reposTableHandler)
	mux.HandleFunc("/admin/ui/_partial/jobs_table", s.jobsTableHandler)
	mux.HandleFunc("/admin/ui/_partial/job_detail/", s.jobDetailHandler)
	mux.HandleFunc("/admin/ui/_partial/project_switcher", s.projectSwitcherHandler)
	mux.HandleFunc("/admin/ui/_partial/linear_projects_options", s.linearProjectsOptionsHandler)
	mux.HandleFunc("/admin/ui/select_project", s.selectProjectHandler)
	// API routes
	mux.HandleFunc("/admin/linear/projects", s.listLinearProjectsHandler)
	mux.HandleFunc("/admin/repos", s.reposDispatchHandler)
	mux.HandleFunc("/admin/repos/", s.reposDispatchHandler)
	mux.HandleFunc("/admin/jobs", s.listJobsHandler)
	mux.HandleFunc("/admin/jobs/", s.getJobHandler)
	mux.HandleFunc("/admin/load", s.loadHandler)
	mux.HandleFunc("/admin/health", s.healthHandler)
	return mux
}

// reposDispatchHandler dispatches all /admin/repos/* requests.
func (s *adminServer) reposDispatchHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/repos/")
	// path is now "" (for GET/POST /admin/repos), or "<projectID>", or "<projectID>/<sub>"
	if path == "" {
		// /admin/repos/ — list (GET) or create (POST)
		switch r.Method {
		case http.MethodGet:
			s.listReposHandler(w, r)
		case http.MethodPost:
			s.createRepoHandler(w, r)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
		return
	}
	parts := strings.SplitN(path, "/", 2)
	projectID := parts[0]
	if projectID == "" {
		// e.g. /admin/repos//something — malformed
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	if len(parts) == 1 {
		// /admin/repos/<projectID> — PATCH or DELETE
		switch r.Method {
		case http.MethodPatch:
			// Reconstruct path for updateRepoHandler
			r.URL.Path = "/admin/repos/" + projectID
			s.updateRepoHandler(w, r)
		case http.MethodDelete:
			r.URL.Path = "/admin/repos/" + projectID
			s.deleteRepoHandler(w, r)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
		return
	}
	subRoute := "/" + parts[1]
	switch subRoute {
	case "/check_gh":
		r.URL.Path = "/admin/repos/" + projectID + "/check_gh"
		s.checkGhAuthHandler(w, r)
	case "/test_clone":
		r.URL.Path = "/admin/repos/" + projectID + "/test_clone"
		s.testCloneHandler(w, r)
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}
