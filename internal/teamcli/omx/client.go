package omx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/georgehuang/admiral/internal/teamcli"
)

type Client struct {
	BinPath  string
	CWD      string
	TeamName string
}

func New(binPath, cwd, teamName string) *Client {
	return &Client{BinPath: binPath, CWD: cwd, TeamName: teamName}
}

var _ teamcli.Provider = (*Client)(nil)

func (c *Client) Caps() teamcli.Capabilities {
	return teamcli.Capabilities{
		SupportsAwaitEvent: true,
		SupportsIdleState:  true,
		SupportsStallState: true,
	}
}

func (c *Client) API(ctx context.Context, operation string, input map[string]any) (*teamcli.Envelope, error) {
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
	runErr := cmd.Run()
	return ParseEnvelope(stdout.Bytes(), stderr.Bytes(), runErr)
}

func ParseEnvelope(stdout, stderr []byte, runErr error) (*teamcli.Envelope, error) {
	if len(stdout) == 0 {
		if runErr != nil {
			return nil, fmt.Errorf("omx exec failed: %w (stderr=%s)", runErr, trimTail(stderr))
		}
		return nil, errors.New("omx returned empty stdout")
	}
	var env teamcli.Envelope
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

// Launch shells out `omx team <N>:<agent> "<task>"`. In "pty" mode the
// ptyCommand (e.g. ["/usr/bin/script","-q","/dev/null"]) is prepended to
// the argv so omx sees a TTY; combined stdout+stderr is captured since
// script(1) redirects child stderr into the pty. In "direct" mode no
// wrapper is used — if omx exits with the TTY-required error,
// teamcli.ErrLaunchNeedsTTY is returned.
func (c *Client) Launch(ctx context.Context, mode teamcli.LaunchMode, ptyCommand []string, spec teamcli.LaunchSpec) error {
	if spec.WorkerCount <= 0 || spec.AgentType == "" || strings.TrimSpace(spec.Task) == "" {
		return fmt.Errorf("launch spec incomplete: %+v", spec)
	}
	args := spec.CommandArgs()

	var cmd *exec.Cmd
	switch mode {
	case teamcli.LaunchPty:
		if len(ptyCommand) == 0 {
			return fmt.Errorf("pty launch: pty_command is empty")
		}
		wrapped := append([]string{}, ptyCommand[1:]...)
		wrapped = append(wrapped, c.BinPath)
		wrapped = append(wrapped, args...)
		cmd = exec.CommandContext(ctx, ptyCommand[0], wrapped...)
	case teamcli.LaunchDirect:
		cmd = exec.CommandContext(ctx, c.BinPath, args...)
	default:
		return fmt.Errorf("unknown launch mode %q", mode)
	}
	cmd.Dir = c.CWD

	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	if err := cmd.Run(); err != nil {
		tail := trimTail(combined.Bytes())
		if strings.Contains(tail, "stdin is not a terminal") {
			return teamcli.ErrLaunchNeedsTTY
		}
		return fmt.Errorf("launch failed: %w (output=%s)", err, tail)
	}
	return nil
}

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

func (c *Client) GetSummary(ctx context.Context) (*teamcli.Envelope, error) {
	return c.API(ctx, "get-summary", nil)
}

func (c *Client) ReadIdleState(ctx context.Context) (*teamcli.Envelope, error) {
	return c.API(ctx, "read-idle-state", nil)
}

func (c *Client) ReadStallState(ctx context.Context) (*teamcli.Envelope, error) {
	return c.API(ctx, "read-stall-state", nil)
}

func (c *Client) SendMessage(ctx context.Context, in teamcli.SendMessageInput) (*teamcli.Envelope, error) {
	return c.API(ctx, "send-message", map[string]any{
		"from_worker": in.FromWorker,
		"to_worker":   in.ToWorker,
		"body":        in.Body,
	})
}

func (c *Client) AwaitEvent(ctx context.Context, afterEventID string, timeoutMs int) (*teamcli.Envelope, error) {
	input := map[string]any{
		"timeout_ms": timeoutMs,
	}
	if afterEventID != "" {
		input["after_event_id"] = afterEventID
	}
	ctx2, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs+10000)*time.Millisecond)
	defer cancel()
	return c.API(ctx2, "await-event", input)
}

func (c *Client) MailboxList(ctx context.Context, worker string, includeDelivered bool) (*teamcli.Envelope, error) {
	return c.API(ctx, "mailbox-list", map[string]any{
		"worker":            worker,
		"include_delivered": includeDelivered,
	})
}

func (c *Client) MailboxMarkDelivered(ctx context.Context, worker, messageID string) (*teamcli.Envelope, error) {
	return c.API(ctx, "mailbox-mark-delivered", map[string]any{
		"worker":     worker,
		"message_id": messageID,
	})
}

func (c *Client) WriteShutdownRequest(ctx context.Context, worker, requestedBy string) (*teamcli.Envelope, error) {
	return c.API(ctx, "write-shutdown-request", map[string]any{
		"worker":       worker,
		"requested_by": requestedBy,
	})
}

func (c *Client) ReadShutdownAck(ctx context.Context, worker string) (*teamcli.Envelope, error) {
	return c.API(ctx, "read-shutdown-ack", map[string]any{
		"worker": worker,
	})
}
