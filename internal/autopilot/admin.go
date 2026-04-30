package autopilot

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

// adminServer serves the read-only admin API.
type adminServer struct {
	db        *store.Store
	lc       *linear.Client
	logger   *slog.Logger
	start    time.Time
	workers  int
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
	SessionID        string `json:"session_id"`
	IssueIdentifier  string `json:"issue_identifier"`
	State            string `json:"state"`
	StartedAt        string `json:"started_at"`
	FinishedAt       string `json:"finished_at,omitempty"`
	PRURL            string `json:"pr_url,omitempty"`
	RepoDir          string `json:"repo_dir,omitempty"`
	StreamLogPath    string `json:"stream_log_path,omitempty"`
	ClaudeSessionID  string `json:"claude_session_id,omitempty"`
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
	UptimeS           int  `json:"uptime_s"`
	DBOK              bool `json:"db_ok"`
	LinearTokenValid  bool `json:"linear_token_valid"`
}

func newAdminServer(db *store.Store, lc *linear.Client, logger *slog.Logger, workers int) *adminServer {
	return &adminServer{db: db, lc: lc, logger: logger, start: time.Now(), workers: workers}
}

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
			State:          j.State,
			StartedAt:      j.StartedAt,
			FinishedAt:     j.FinishedAt,
			PRURL:          j.PRURL,
			StreamLogPath:  j.StreamLogPath,
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
		PendingEvents:     pending,
		ProcessingEvents:  processing,
		InFlightJobs:      inFlightJobs,
	})
}

func (s *adminServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	dbOK := true
	tok, err := s.db.GetLinearOAuthToken()
	linearOK := err == nil && tok != nil && tok.AccessToken != ""

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(adminHealthResponse{
		OK:              dbOK && linearOK,
		UptimeS:         int(time.Since(s.start).Seconds()),
		DBOK:            dbOK,
		LinearTokenValid: linearOK,
	})
}

// ServeAdminHTTP starts the admin HTTP server on addr.
func ServeAdminHTTP(addr string, db *store.Store, lc *linear.Client, logger *slog.Logger, workers int) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           newAdminMux(db, lc, logger, workers),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

func newAdminMux(db *store.Store, lc *linear.Client, logger *slog.Logger, workers int) *http.ServeMux {
	as := newAdminServer(db, lc, logger, workers)
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/repos", as.listReposHandler)
	mux.HandleFunc("/admin/jobs", as.listJobsHandler)
	mux.HandleFunc("/admin/jobs/", as.getJobHandler)
	mux.HandleFunc("/admin/load", as.loadHandler)
	mux.HandleFunc("/admin/health", as.healthHandler)
	return mux
}
