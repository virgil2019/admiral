package omx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

type Envelope struct {
	OK        bool            `json:"ok"`
	Operation string          `json:"operation"`
	Data      json.RawMessage `json:"data,omitempty"`
	Error     *ErrorDetail    `json:"error,omitempty"`
}

type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ErrorDetail) ErrorString() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

type Client struct {
	BinPath  string
	CWD      string
	TeamName string
}

func New(binPath, cwd, teamName string) *Client {
	return &Client{BinPath: binPath, CWD: cwd, TeamName: teamName}
}

func (c *Client) API(ctx context.Context, operation string, input map[string]any) (*Envelope, error) {
	if input == nil {
		input = map[string]any{}
	}
	if _, ok := input["team_name"]; !ok {
		input["team_name"] = c.TeamName
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal input: %w", err)
	}
	cmd := exec.CommandContext(ctx, c.BinPath, "team", "api", operation, "--input", string(payload), "--json")
	cmd.Dir = c.CWD
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return ParseEnvelope(stdout.Bytes(), stderr.Bytes(), err)
	}
	return ParseEnvelope(stdout.Bytes(), stderr.Bytes(), nil)
}

func ParseEnvelope(stdout, stderr []byte, runErr error) (*Envelope, error) {
	if len(stdout) == 0 {
		if runErr != nil {
			return nil, fmt.Errorf("omx exec failed: %w (stderr=%s)", runErr, trimTail(stderr))
		}
		return nil, errors.New("omx returned empty stdout")
	}
	var env Envelope
	if err := json.Unmarshal(stdout, &env); err != nil {
		return nil, fmt.Errorf("parse envelope: %w (stdout=%s)", err, trimTail(stdout))
	}
	if !env.OK && env.Error == nil {
		return &env, errors.New("envelope ok=false but no error body")
	}
	return &env, nil
}

func trimTail(b []byte) string {
	const max = 300
	s := string(bytes.TrimSpace(b))
	if len(s) > max {
		return s[len(s)-max:]
	}
	return s
}

// Launch shells out `omx launch <team>` using `script` to provide a PTY,
// since the omx launcher refuses to run without a terminal. This is a
// spec deviation documented in README / task comment.
func (c *Client) Launch(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "/usr/bin/script", "-q", "/dev/null",
		c.BinPath, "launch", c.TeamName)
	cmd.Dir = c.CWD
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("launch failed: %w (stderr=%s)", err, trimTail(stderr.Bytes()))
	}
	return nil
}

// TeamStatusJSON returns the parsed output of `omx team status <team> --json`.
func (c *Client) TeamStatusJSON(ctx context.Context) (map[string]any, error) {
	cmd := exec.CommandContext(ctx, c.BinPath, "team", "status", c.TeamName, "--json")
	cmd.Dir = c.CWD
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("team status failed: %w (stderr=%s)", err, trimTail(stderr.Bytes()))
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("parse team status: %w", err)
	}
	return out, nil
}

// Helper typed wrappers -------------------------------------------------------

func (c *Client) GetSummary(ctx context.Context) (*Envelope, error) {
	return c.API(ctx, "get-summary", nil)
}

func (c *Client) ReadIdleState(ctx context.Context) (*Envelope, error) {
	return c.API(ctx, "read-idle-state", nil)
}

type SendMessageInput struct {
	FromWorker string
	ToWorker   string
	Body       string
}

func (c *Client) SendMessage(ctx context.Context, in SendMessageInput) (*Envelope, error) {
	return c.API(ctx, "send-message", map[string]any{
		"from_worker": in.FromWorker,
		"to_worker":   in.ToWorker,
		"body":        in.Body,
	})
}

func (c *Client) AwaitEvent(ctx context.Context, afterEventID string, timeoutMs int) (*Envelope, error) {
	input := map[string]any{
		"timeout_ms": timeoutMs,
	}
	if afterEventID != "" {
		input["after_event_id"] = afterEventID
	}
	// Apply a bounded CommandContext covering the expected poll + slack.
	ctx2, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs+10000)*time.Millisecond)
	defer cancel()
	return c.API(ctx2, "await-event", input)
}

func (c *Client) MailboxList(ctx context.Context, worker string, includeDelivered bool) (*Envelope, error) {
	return c.API(ctx, "mailbox-list", map[string]any{
		"worker":            worker,
		"include_delivered": includeDelivered,
	})
}

func (c *Client) MailboxMarkDelivered(ctx context.Context, worker, messageID string) (*Envelope, error) {
	return c.API(ctx, "mailbox-mark-delivered", map[string]any{
		"worker":     worker,
		"message_id": messageID,
	})
}

func (c *Client) WriteShutdownRequest(ctx context.Context, worker, requestedBy string) (*Envelope, error) {
	return c.API(ctx, "write-shutdown-request", map[string]any{
		"worker":       worker,
		"requested_by": requestedBy,
	})
}

func (c *Client) ReadShutdownAck(ctx context.Context, worker string) (*Envelope, error) {
	return c.API(ctx, "read-shutdown-ack", map[string]any{
		"worker": worker,
	})
}
