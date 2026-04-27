package teamcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Envelope is the `{ok, operation, data, error}` JSON shape emitted by
// both omx and omc `team api` commands.
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

// Capabilities describes which provider features are available. Bridge
// code reads this to pick between long-poll and polling event delivery,
// and to decide whether capability-gated endpoints are safe to call.
type Capabilities struct {
	// SupportsAwaitEvent: `team api await-event` long-poll endpoint.
	// omx: true. omc: false.
	SupportsAwaitEvent bool
	// SupportsIdleState: `team api read-idle-state`. omx-only.
	SupportsIdleState bool
	// SupportsStallState: `team api read-stall-state`. omx-only.
	SupportsStallState bool
}

type LaunchMode string

const (
	LaunchDirect LaunchMode = "direct"
	LaunchPty    LaunchMode = "pty"
)

// LaunchSpec describes how to spawn a team via `<bin> team N:agent "<task>"`.
// omx and omc sanity-tested to accept the same argv shape.
type LaunchSpec struct {
	WorkerCount int
	AgentType   string
	Task        string
}

// CommandArgs returns the argv (excluding the binary) for `team N:agent "<task>"`.
func (s LaunchSpec) CommandArgs() []string {
	return []string{"team", fmt.Sprintf("%d:%s", s.WorkerCount, s.AgentType), s.Task}
}

type SendMessageInput struct {
	FromWorker string
	ToWorker   string
	Body       string
}

// ErrLaunchNeedsTTY is returned when a provider's Launch in direct mode
// exits with the "stdin is not a terminal" error.
var ErrLaunchNeedsTTY = errors.New("provider requires a TTY")

// ErrUnsupported is returned by Provider methods that correspond to
// unsupported capabilities. Callers should guard with Caps() first; this
// is a defensive fallback.
var ErrUnsupported = errors.New("operation not supported by provider")

// Provider abstracts a team-cli backend (omx or omc). Concrete providers
// wrap a CLI binary and shell out `<bin> team api <op> --input ... --json`.
type Provider interface {
	Caps() Capabilities

	// Launch spawns the team. In pty mode, ptyCommand is prepended to the
	// argv so the binary sees a TTY. In direct mode, pass ptyCommand=nil.
	// Returns ErrLaunchNeedsTTY if direct mode fails with the TTY error.
	Launch(ctx context.Context, mode LaunchMode, ptyCommand []string, spec LaunchSpec) error

	// API is the escape hatch for arbitrary team-api operations. Typed
	// helpers below should be preferred.
	API(ctx context.Context, operation string, input map[string]any) (*Envelope, error)

	// TeamStatusJSON parses `<bin> team status <team> --json`.
	TeamStatusJSON(ctx context.Context) (map[string]any, error)

	// Universal typed wrappers.
	GetSummary(ctx context.Context) (*Envelope, error)
	SendMessage(ctx context.Context, in SendMessageInput) (*Envelope, error)
	MailboxList(ctx context.Context, worker string, includeDelivered bool) (*Envelope, error)
	MailboxMarkDelivered(ctx context.Context, worker, messageID string) (*Envelope, error)
	WriteShutdownRequest(ctx context.Context, worker, requestedBy string) (*Envelope, error)
	ReadShutdownAck(ctx context.Context, worker string) (*Envelope, error)

	// Capability-gated — callers must check Caps() before calling.
	AwaitEvent(ctx context.Context, afterEventID string, timeoutMs int) (*Envelope, error)
	ReadIdleState(ctx context.Context) (*Envelope, error)
	ReadStallState(ctx context.Context) (*Envelope, error)
}
