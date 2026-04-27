package omc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/creack/pty"

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
		SupportsAwaitEvent: false,
		SupportsIdleState:  false,
		SupportsStallState: false,
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
	return ParseEnvelope(stdout.Bytes(), filterNoise(stderr.Bytes()), runErr)
}

func ParseEnvelope(stdout, stderr []byte, runErr error) (*teamcli.Envelope, error) {
	if len(stdout) == 0 {
		if runErr != nil {
			return nil, fmt.Errorf("omc exec failed: %w (stderr=%s)", runErr, trimTail(stderr))
		}
		return nil, errors.New("omc returned empty stdout")
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

// filterNoise drops `[team] canonicalized duplicate worker entries: ...`
// lines that omc emits to stderr on every team operation. They are not
// errors and otherwise pollute launch-failure error messages.
func filterNoise(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	var out bytes.Buffer
	for _, line := range bytes.Split(b, []byte("\n")) {
		if bytes.Contains(line, []byte("[team] canonicalized duplicate worker entries:")) {
			continue
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return bytes.TrimRight(out.Bytes(), "\n")
}

// Launch shells out `omc team <N>:<agent> "<task>"` under a real PTY.
//
// Why a PTY: omc team's setup phase (creating tmux session, sending
// `claude ...` keys into worker panes via tmux send-keys) needs an open
// stdin during the ~1s window before it exits. With stdin = /dev/null
// it sees EOF immediately, exits before completing send-keys, and the
// worker panes end up empty. A real PTY (master held open here) keeps
// the child's stdin alive until the child decides to exit on its own.
//
// Why we don't use the script(1) pty_command path that omx uses: same
// stdin-EOF problem. `script -q /dev/null cmd` proxies the parent's
// stdin (which from a non-interactive Go process is /dev/null) into
// the child's PTY, so the child still sees prompt EOF.
//
// `mode` and `ptyCommand` are accepted for interface symmetry with omx
// but are not consulted — omc always launches under a creack/pty PTY.
//
// First-run trust folder dialog: when claude runs in a fresh cwd it
// shows a "trust this folder?" prompt that blocks the worker until a
// human picks "1". Admiral cannot resolve this remotely; the user must
// `tmux attach -t omc-team-<...>` once and accept. Afterwards the
// trust is persisted by claude and subsequent admiral launches go
// fully unattended.
func (c *Client) Launch(ctx context.Context, mode teamcli.LaunchMode, ptyCommand []string, spec teamcli.LaunchSpec) error {
	_ = mode
	_ = ptyCommand
	if spec.WorkerCount <= 0 || spec.AgentType == "" || strings.TrimSpace(spec.Task) == "" {
		return fmt.Errorf("launch spec incomplete: %+v", spec)
	}
	cmd := exec.CommandContext(ctx, c.BinPath, spec.CommandArgs()...)
	cmd.Dir = c.CWD

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("launch pty start: %w", err)
	}
	defer ptmx.Close()

	var combined bytes.Buffer
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(&combined, ptmx)
		close(drained)
	}()

	if err := cmd.Wait(); err != nil {
		<-drained
		tail := trimTail(filterNoise(combined.Bytes()))
		if strings.Contains(tail, "stdin is not a terminal") {
			return teamcli.ErrLaunchNeedsTTY
		}
		return fmt.Errorf("launch failed: %w (output=%s)", err, tail)
	}
	<-drained
	return nil
}

func (c *Client) TeamStatusJSON(ctx context.Context) (map[string]any, error) {
	cmd := exec.CommandContext(ctx, c.BinPath, "team", "status", c.TeamName, "--json")
	cmd.Dir = c.CWD
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("team status failed: %w (stderr=%s)", err, trimTail(filterNoise(stderr.Bytes())))
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

func (c *Client) SendMessage(ctx context.Context, in teamcli.SendMessageInput) (*teamcli.Envelope, error) {
	env, err := c.API(ctx, "send-message", map[string]any{
		"from_worker": in.FromWorker,
		"to_worker":   in.ToWorker,
		"body":        in.Body,
	})
	if err == nil && env != nil && env.OK {
		// omc dispatches the trigger text to the worker pane via tmux
		// send-keys, but the fallback path (used when claude is not
		// running with the omc dispatch hook) does not press Enter.
		// claude shows the trigger in its input box and sits there.
		// Send a bare Enter so claude actually submits and starts work.
		// Best-effort: any failure here is logged-by-omission; the user
		// just sees a delayed reply until they manually Enter.
		c.poke(ctx, in.ToWorker)
	}
	return env, err
}

// poke reads the team manifest to find the worker's tmux pane id and
// fires `tmux send-keys -t <pane> Enter`. Used by SendMessage to
// compensate for the fallback dispatch path not auto-submitting.
func (c *Client) poke(ctx context.Context, worker string) {
	manifest := filepath.Join(c.CWD, ".omc", "state", "team", c.TeamName, "manifest.json")
	data, err := os.ReadFile(manifest)
	if err != nil {
		return
	}
	var m struct {
		Workers []struct {
			Name   string `json:"name"`
			PaneID string `json:"pane_id"`
		} `json:"workers"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	var pane string
	for _, w := range m.Workers {
		if w.Name == worker {
			pane = w.PaneID
			break
		}
	}
	if pane == "" {
		return
	}
	_ = exec.CommandContext(ctx, "tmux", "send-keys", "-t", pane, "Enter").Run()
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

// Capability-gated — omc does not expose these; the bridge guards them
// via Caps() before calling, so these are defensive fallbacks.

func (c *Client) AwaitEvent(ctx context.Context, afterEventID string, timeoutMs int) (*teamcli.Envelope, error) {
	return nil, teamcli.ErrUnsupported
}

func (c *Client) ReadIdleState(ctx context.Context) (*teamcli.Envelope, error) {
	return nil, teamcli.ErrUnsupported
}

func (c *Client) ReadStallState(ctx context.Context) (*teamcli.Envelope, error) {
	return nil, teamcli.ErrUnsupported
}
