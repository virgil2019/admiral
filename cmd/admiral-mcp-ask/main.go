// admiral-mcp-ask is a minimal MCP stdio server that provides the ask_user
// tool for claude runs spawned by admiral-autopilot. When claude calls
// ask_user, this server:
//   1. Inserts a pending_question row into the admiral SQLite DB.
//   2. Posts an elicitation activity to the Linear agent thread.
//   3. Returns {"status":"pending","pending_id":"<uuid>"} to claude.
//
// Claude's system prompt instructs it to stop work and exit when it receives
// a pending response, which transitions the admiral_tasks row to AWAITING_INPUT.
// The orchestrator resumes the claude session once the user replies.
//
// Environment variables (all required unless noted):
//   ADMIRAL_DB_PATH          - path to the SQLite database
//   ADMIRAL_ISSUE_ID         - Linear issue UUID
//   ADMIRAL_ISSUE_IDENTIFIER - human-readable identifier (e.g. "GEO-42")
//   ADMIRAL_LINEAR_SESSION   - Linear agent session ID for PostAgentActivity
//   ADMIRAL_CLAUDE_SESSION   - Claude session ID (for resume after reply)
//   ADMIRAL_WORKTREE_PATH    - absolute path to the active git worktree
//   ADMIRAL_LINEAR_ENDPOINT  - Linear GraphQL endpoint (optional; default: https://api.linear.app/graphql)
//
// Linear OAuth token is loaded from the DB (not from env) to avoid leaking it
// into all subprocesses spawned by the claude run.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("[admiral-mcp-ask] ")
	log.SetFlags(0)

	dbPath := mustEnv("ADMIRAL_DB_PATH")
	issueID := mustEnv("ADMIRAL_ISSUE_ID")
	issueIdentifier := os.Getenv("ADMIRAL_ISSUE_IDENTIFIER")
	linearSession := mustEnv("ADMIRAL_LINEAR_SESSION")
	claudeSession := os.Getenv("ADMIRAL_CLAUDE_SESSION")
	worktreePath := os.Getenv("ADMIRAL_WORKTREE_PATH")
	linearEndpoint := os.Getenv("ADMIRAL_LINEAR_ENDPOINT")
	if linearEndpoint == "" {
		linearEndpoint = "https://api.linear.app/graphql"
	}

	db, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	tok, err := db.GetLinearOAuthToken()
	if err != nil || tok == nil || tok.AccessToken == "" {
		log.Fatalf("linear oauth token not found in db: %v", err)
	}

	s := &server{
		db:              db,
		lc:              linear.NewClient(linearEndpoint, tok.AccessToken),
		issueID:         issueID,
		issueIdentifier: issueIdentifier,
		linearSession:   linearSession,
		claudeSession:   claudeSession,
		worktreePath:    worktreePath,
		scanner:         bufio.NewScanner(os.Stdin),
		enc:             json.NewEncoder(os.Stdout),
	}
	if err := s.run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("%s not set", key)
	}
	return v
}

// --- MCP JSON-RPC server ---

type server struct {
	db              *store.Store
	lc              *linear.Client
	issueID         string
	issueIdentifier string
	linearSession   string
	claudeSession   string
	worktreePath    string
	scanner         *bufio.Scanner
	enc             *json.Encoder
}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *server) run() error {
	s.scanner.Buffer(make([]byte, 0, 1<<20), 4<<20)
	for s.scanner.Scan() {
		line := strings.TrimSpace(s.scanner.Text())
		if line == "" {
			continue
		}
		var msg rpcMsg
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			log.Printf("parse error: %v", err)
			continue
		}
		if err := s.handle(msg); err != nil {
			log.Printf("handle %s: %v", msg.Method, err)
		}
	}
	return s.scanner.Err()
}

func (s *server) respond(id json.RawMessage, result any) error {
	return s.enc.Encode(rpcMsg{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

func (s *server) respondErr(id json.RawMessage, code int, msg string) error {
	return s.enc.Encode(rpcMsg{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: msg},
	})
}

func (s *server) handle(msg rpcMsg) error {
	switch msg.Method {
	case "initialize":
		return s.respond(msg.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "admiral-ask-mcp", "version": "0.1.0"},
		})

	case "notifications/initialized":
		return nil // notification — no response

	case "tools/list":
		return s.respond(msg.ID, map[string]any{
			"tools": []any{askUserToolDef()},
		})

	case "tools/call":
		return s.handleToolCall(msg)

	case "ping":
		return s.respond(msg.ID, map[string]any{})

	default:
		if msg.ID != nil {
			return s.respondErr(msg.ID, -32601, "method not found: "+msg.Method)
		}
		return nil
	}
}

func askUserToolDef() map[string]any {
	return map[string]any{
		"name": "ask_user",
		"description": "Ask the user a question via the Linear thread. " +
			"Calling this tool pauses the current task: admiral posts your question to the Linear thread " +
			"and resumes automatically once the user replies. " +
			"Use only when you genuinely need human input to proceed — not for confirmations you can infer.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"question": map[string]any{
					"type":        "string",
					"description": "The question to ask the user.",
				},
				"options": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional list of choices to present (e.g. [\"v1\", \"v2\"]).",
				},
			},
			"required": []string{"question"},
		},
	}
}

func (s *server) handleToolCall(msg rpcMsg) error {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return s.respondErr(msg.ID, -32602, "invalid params: "+err.Error())
	}
	if params.Name != "ask_user" {
		return s.respondErr(msg.ID, -32601, "unknown tool: "+params.Name)
	}

	var args struct {
		Question string   `json:"question"`
		Options  []string `json:"options"`
	}
	if err := json.Unmarshal(params.Arguments, &args); err != nil {
		return s.respondErr(msg.ID, -32602, "invalid arguments: "+err.Error())
	}
	if args.Question == "" {
		return s.respondErr(msg.ID, -32602, "question is required")
	}

	var optJSON []byte
	if len(args.Options) == 0 {
		optJSON = []byte("[]")
	} else {
		optJSON, _ = json.Marshal(args.Options)
	}
	pendingID := uuid.NewString()

	q := store.PendingQuestion{
		ID:                 pendingID,
		IssueID:            s.issueID,
		IssueIdentifier:    s.issueIdentifier,
		ClaudeSessionID:    s.claudeSession,
		LastEventSessionID: s.linearSession,
		WorktreePath:       s.worktreePath,
		Question:           args.Question,
		OptionsJSON:        string(optJSON),
		CreatedAt:          time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.db.InsertPendingQuestion(q); err != nil {
		log.Printf("insert pending_question: %v", err)
		return s.respondErr(msg.ID, -32603, "failed to persist question: "+err.Error())
	}
	// Transition task state immediately so a fast Linear reply doesn't race
	// with the orchestrator's parkAwaitingInput call (M1 fix).
	if err := s.db.SetAdmiralTaskAwaitingInput(s.issueID, pendingID); err != nil {
		log.Printf("set awaiting_input: %v", err)
	}

	body := formatElicitation(args.Question, args.Options)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.lc.PostAgentActivity(ctx, s.linearSession, linear.Elicitation(body)); err != nil {
		log.Printf("post elicitation: %v", err)
		// Non-fatal: the question is already persisted; the user can still reply.
	}

	result, _ := json.Marshal(map[string]string{
		"status":     "pending",
		"pending_id": pendingID,
	})
	return s.respond(msg.ID, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": string(result)}},
		"isError": false,
	})
}

// formatElicitation builds the Linear comment body for the elicitation post.
func formatElicitation(question string, options []string) string {
	if len(options) == 0 {
		return fmt.Sprintf("admiral asks: %s", question)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "admiral asks: %s\n\nOptions:", question)
	for _, o := range options {
		fmt.Fprintf(&sb, "\n- %s", o)
	}
	return sb.String()
}
