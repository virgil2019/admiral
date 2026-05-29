// Package planner is the admiral-planner-mcp server core: a stdio
// JSON-RPC server that exposes "planner" tools to a host agent
// (claude / codex / any MCP-aware agent). The host agent is the
// brain — judgment of "does this PR meet the acceptance criteria"
// happens in the agent's LLM context. This server is the agent's
// notebook (read planner state) and its hands (write back to GitHub /
// Linear). No LLM call ever originates from this process.
//
// Lifecycle: spawned as a stdio child by the host agent. Reads
// JSON-RPC requests on stdin, writes responses on stdout, never logs
// to stdout (would corrupt the protocol stream — all logs go to
// stderr). Exits on stdin EOF, which is how the host agent stops it.
package planner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
)

// JSON-RPC 2.0 error codes used by this server. -32601 / -32602 /
// -32603 are reserved by the spec; values > -32000 are
// implementation-defined and currently unused.
const (
	errCodeMethodNotFound = -32601
	errCodeInvalidParams  = -32602
	errCodeInternal       = -32603
)

// rpcMsg is a single JSON-RPC envelope. Request and response share
// the same shape so one struct suffices for both decode and encode;
// unused fields stay zero / omitempty.
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

// Server is the long-lived stdio MCP server. It owns the tool
// registry and the I/O loop. The registry is populated by NewServer
// — tools that need a Store reference close over it at construction
// time.
type Server struct {
	tools   map[string]*toolDef
	scanner *bufio.Scanner
	enc     *json.Encoder
	log     *log.Logger
}

// ServerInfo is what `initialize` returns to the host agent. Bumped
// independently of admiral itself.
var ServerInfo = map[string]any{
	"name":    "admiral-planner-mcp",
	"version": "0.1.0",
}

// MCPProtocolVersion is the protocol revision this server speaks.
// Matches what admiral-mcp-ask uses; bumped when the MCP spec adds
// fields we want to surface in initialize.
const MCPProtocolVersion = "2024-11-05"

// scannerMaxMessageBytes caps inbound JSON-RPC line length. Outbound
// PR diffs go back to the host agent in tool results (Server only
// receives requests, so this affects the *request* side). The
// realistic upper bound is the host agent feeding a prior diff back
// into a tool call's reasoning argument — 32MB is comfortably above
// what gh pr diff produces for even huge PRs. If this still trips,
// the right fix is to switch to json.Decoder (no per-message cap);
// keeping bufio.Scanner for now to preserve newline-delimited
// framing semantics shared with admiral-mcp-ask.
const scannerMaxMessageBytes = 32 << 20

// NewServer wires the tool registry and prepares the I/O streams.
// stdin / stdout / stderr are passed explicitly so tests can drive
// the loop with bytes.Buffer / io.Pipe — production main() supplies
// os.Stdin / os.Stdout / os.Stderr.
func NewServer(stdin io.Reader, stdout io.Writer, stderr io.Writer, tools map[string]*toolDef) *Server {
	logger := log.New(stderr, "[admiral-planner-mcp] ", 0)
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 0, 1<<20), scannerMaxMessageBytes)
	return &Server{
		tools:   tools,
		scanner: sc,
		enc:     json.NewEncoder(stdout),
		log:     logger,
	}
}

// Run blocks until stdin closes or scanning errors. Each line is one
// JSON-RPC message. Parse failures are logged but do not terminate
// the loop — the host agent may recover and send a well-formed
// request afterwards.
func (s *Server) Run(ctx context.Context) error {
	for s.scanner.Scan() {
		line := strings.TrimSpace(s.scanner.Text())
		if line == "" {
			continue
		}
		var msg rpcMsg
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			s.log.Printf("parse error: %v", err)
			continue
		}
		if err := s.handle(ctx, msg); err != nil {
			s.log.Printf("handle %s: %v", msg.Method, err)
		}
	}
	return s.scanner.Err()
}

// handle dispatches one decoded RPC message. Notifications (no ID
// field) never get a response — handle returns nil after processing
// them. Requests with an ID always produce exactly one response,
// even on error.
func (s *Server) handle(ctx context.Context, msg rpcMsg) error {
	switch msg.Method {
	case "initialize":
		return s.respond(msg.ID, map[string]any{
			"protocolVersion": MCPProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      ServerInfo,
		})

	case "notifications/initialized":
		// Notification: spec requires no response.
		return nil

	case "tools/list":
		return s.respond(msg.ID, map[string]any{
			"tools": s.toolList(),
		})

	case "tools/call":
		return s.handleToolCall(ctx, msg)

	case "ping":
		return s.respond(msg.ID, map[string]any{})

	default:
		if msg.ID != nil {
			return s.respondErr(msg.ID, errCodeMethodNotFound, "method not found: "+msg.Method)
		}
		// Unknown notification — silently ignore per JSON-RPC 2.0.
		return nil
	}
}

// toolList returns the descriptor slice expected by tools/list, with
// names sorted so the host agent sees a stable order across restarts.
// Without sorting, Go map iteration would shuffle the list per
// process — annoying when comparing transcripts.
func (s *Server) toolList() []any {
	names := make([]string, 0, len(s.tools))
	for n := range s.tools {
		names = append(names, n)
	}
	// Sort in-place; small N, no allocator pressure worth caring about.
	sortStrings(names)
	out := make([]any, 0, len(names))
	for _, n := range names {
		t := s.tools[n]
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		})
	}
	return out
}

// handleToolCall parses {name, arguments} from params, looks up the
// handler, runs it, and wraps the return value in the MCP
// content-block envelope (`content: [{type:"text", text:<json>}]`).
// Handler errors become MCP "error result" envelopes (isError=true)
// rather than JSON-RPC errors — that's the protocol distinction
// between transport-level and tool-level failure.
func (s *Server) handleToolCall(ctx context.Context, msg rpcMsg) error {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return s.respondErr(msg.ID, errCodeInvalidParams, "invalid params: "+err.Error())
	}
	t, ok := s.tools[params.Name]
	if !ok {
		return s.respondErr(msg.ID, errCodeMethodNotFound, "unknown tool: "+params.Name)
	}

	result, err := t.Handler(ctx, params.Arguments)
	if err != nil {
		// Tool-level error: report as an MCP isError result so the
		// host agent can show it to the LLM as feedback rather than
		// abort the session on a transport-level error.
		return s.respond(msg.ID, map[string]any{
			"content": []any{map[string]any{
				"type": "text",
				"text": err.Error(),
			}},
			"isError": true,
		})
	}

	body, err := json.Marshal(result)
	if err != nil {
		return s.respondErr(msg.ID, errCodeInternal,
			fmt.Sprintf("marshal tool result: %v", err))
	}
	return s.respond(msg.ID, map[string]any{
		"content": []any{map[string]any{
			"type": "text",
			"text": string(body),
		}},
		"isError": false,
	})
}

// isNotification reports whether the inbound message was a JSON-RPC
// notification (no id field). Per JSON-RPC 2.0 §4.1 a server MUST NOT
// send a response to a notification — even for normally-request-style
// methods like "ping" if the client deliberately omits id as a
// keep-alive. handle() runs the side effects regardless; only the
// reply is suppressed.
func isNotification(id json.RawMessage) bool {
	// json.RawMessage decodes a missing field as nil; the literal JSON
	// null also reaches here as the 4 bytes "null". Treat both as
	// notification — a client sending id=null is buggy but we should
	// still not send a paired response with a null id, which §4.2
	// describes only for parse errors (and we already handle those at
	// the scanner layer).
	if len(id) == 0 {
		return true
	}
	return string(id) == "null"
}

func (s *Server) respond(id json.RawMessage, result any) error {
	if isNotification(id) {
		return nil
	}
	return s.enc.Encode(rpcMsg{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

func (s *Server) respondErr(id json.RawMessage, code int, msg string) error {
	if isNotification(id) {
		return nil
	}
	return s.enc.Encode(rpcMsg{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: msg},
	})
}

// sortStrings is an inline tiny insertion sort. Not using sort.Strings
// to avoid the unrelated `sort` import bloat in this small file —
// tool counts top out at ~10, insertion sort is O(N²) on a constant.
func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}
