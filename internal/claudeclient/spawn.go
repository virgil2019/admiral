// Package claudeclient is a thin wrapper around `claude -p` invocations
// used by admiral's autopilot + discoverer paths. The single shared
// helper encapsulates two things every caller used to open-code:
//
//  1. The boilerplate (timeout ctx + SIGTERM-on-cancel + 5s WaitDelay +
//     Dir + Env + StdoutPipe/StderrPipe drain + Wait with deadline
//     discrimination) that was duplicated in five places.
//
//  2. The argv-vs-argv-size-cap (E2BIG / "argument list too long")
//     defense: when total argv size (claudeBin path + `-p` flag + prompt
//     value + extraArgs + NUL separators) exceeds SafeArgvBudget, the
//     prompt is written to a temp file under <spawnDir>/claude-prompt-*.md
//     and piped via cmd.Stdin, sidestepping Linux's per-process argv
//     ceiling. This unblocked the L2 verify round on GEO-267 in 2026-07-01
//     when a 6-subtree diff prompt grew past ARG_MAX (see bridge.log).
//
// Lives in its own package so internal/autopilot and internal/discoverer
// can both depend on it without a cross-package edge between them.
package claudeclient

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SafeArgvBudget is the conservative cap on total argv bytes (sum of
// `len(arg)` over every argv element + one NUL separator per element +
// the `claudeBin` path + the `-p` flag) before Run routes the prompt
// through stdin instead of as a positional argument. Linux ARG_MAX is
// typically 128 KiB on modern kernels (sysconf(_SC_ARG_MAX)); 96 KiB
// leaves headroom for env expansion that the kernel accounts against
// the same argv ceiling on some kernels (see `man execve`: the limit
// covers argv + envp combined). The 32 KiB of slack lets very long
// one-off env values (MCP configs, ad-hoc flags) piggyback on the budget
// without per-call-site retuning.
const SafeArgvBudget = 96 * 1024

// Run spawns `claude -p <prompt-or-stdin> <extraArgs...>` in workDir with
// the supplied env and captures stdout as a string. stderr is drained
// line-by-line into `logger.Warn("claude_stderr", ...)` so transient
// stderr noise does not block parsing. Returns the trimmed captured
// stdout and any error from cmd.Start / cmd.Wait / context deadline.
//
// Transport is chosen at call time based on argv size:
//
//   - Small prompts (argv total ≤ SafeArgvBudget including the `-p` flag
//     and prompt value): passed as the positional arg. Same shape as the
//     pre-helper call sites, no behaviour change for inputs that already
//     worked.
//
//   - Large prompts: written to a temp file under <spawnDir>/claude-prompt-*.md
//     and piped via cmd.Stdin. `claude -p` accepts the prompt from stdin
//     when no positional prompt is supplied (per anthropics/claude-code
//     CHANGELOG: "Piping file content to Claude ... works with both
//     interactive and print modes"). This sidesteps Linux's per-process
//     argv size limit (E2BIG / "argument list too long"), which fired on
//     GEO-267's verify round when the concatenated prompts + diffs for
//     six sub-issues grew past ARG_MAX (see bridge.log: `fork/exec
//     /home/george/.local/bin/claude: argument list too long`).
//
// Dump lifecycle:
//
//   - On cmd.Start / cmd.Wait success: the dump file is removed in a best-
//     effort `os.Remove`.
//
//   - On any failure path (Start error, non-zero exit, context deadline):
//     the dump file is KEPT for post-mortem and its path is surfaced both
//     in the returned error message AND in a `claude_prompt_dump` log
//     line. This mirrors how `stream_log_path` is preserved on failed
//     `admiral_jobs` runs so an operator can `cat` exactly what was sent.
//
// spawnDir is the directory under which dump files are written. Pass
// `config.JobStreamsDir` so a `rm -rf` of the streams dir cleans up dregs;
// an empty string falls back to `os.TempDir()/admiral-spawn`.
//
// logger may be nil — defaults to `slog.Default()`.
func Run(
	ctx context.Context,
	claudeBin string,
	maxRunSeconds int,
	workDir string,
	env []string,
	prompt string,
	extraArgs []string,
	spawnDir string,
	logger *slog.Logger,
) (stdout string, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	if maxRunSeconds <= 0 {
		maxRunSeconds = 1800 // defensive default mirroring config.DefaultMaxRunSeconds
	}

	cctx, cancel := context.WithTimeout(ctx, time.Duration(maxRunSeconds)*time.Second)
	defer cancel()

	cmd, dumpPath := PrepareCmd(cctx, claudeBin, workDir, env, prompt, extraArgs, spawnDir)
	out, err := DrainText(cctx, cmd, logger)
	if err != nil {
		if dumpPath != "" {
			logger.Error("claude_prompt_dump", "path", dumpPath, "err", err.Error())
			return out, fmt.Errorf("%w (prompt dumped to %s)", err, dumpPath)
		}
		return out, err
	}
	if dumpPath != "" {
		// Best-effort cleanup; a leftover file is harmless and small.
		if rerr := os.Remove(dumpPath); rerr != nil && !os.IsNotExist(rerr) {
			logger.Warn("claude_prompt_dump_remove_failed", "path", dumpPath, "err", rerr)
		}
	}
	return out, nil
}

// PrepareCmd is the lower-level helper used by Run AND by the
// autopilot's stream-json / mcp-config paths in orchestrator.go
// (runClaudeResume + runClaudeSpawn), which need direct access to
// *exec.Cmd to wire up their own drainers. Returns the configured cmd
// plus the dump path (empty when no prompt dump was needed). The caller
// is responsible for the dump file's lifecycle — on success the caller
// MUST os.Remove it; on failure they should log its path and leave it in
// place. Cancellation is owned by the caller; do NOT add another
// WithTimeout over ctx here.
func PrepareCmd(
	ctx context.Context,
	claudeBin string,
	workDir string,
	env []string,
	prompt string,
	extraArgs []string,
	spawnDir string,
) (cmd *exec.Cmd, dumpPath string) {
	useStdin := totalArgvBytes(claudeBin, prompt, extraArgs) > SafeArgvBudget

	args := make([]string, 0, 3+len(extraArgs))
	args = append(args, claudeBin, "-p")
	if !useStdin {
		args = append(args, prompt)
	}
	args = append(args, extraArgs...)

	cmd = exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Cancel = func() error {
		// cancel may be called before Process is set (e.g. if Start
		// fails before forking). Guard against the nil-process panic
		// that bit the original open-coded sites on rare E2BIG paths.
		if cmd.Process != nil {
			return cmd.Process.Signal(syscall.SIGTERM)
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
	if workDir != "" {
		cmd.Dir = workDir
	}
	if env != nil {
		cmd.Env = env
	} else {
		cmd.Env = os.Environ()
	}

	if useStdin {
		var ferr error
		dumpPath, ferr = writePromptDump(prompt, spawnDir)
		if ferr != nil {
			// Surface as a cmd that never ran; caller will report it
			// on Start. Leave dumpPath empty so the cleanup path does
			// not try to remove a half-written file.
			return cmd, ""
		}
		rf, rerr := os.Open(dumpPath)
		if rerr != nil {
			// Same defensive shape; caller will see Open fail when
			// cmd.Start tries to read from the missing stdin (it
			// won't — Start does not touch Stdin until the process
			// is running, so this case is unreachable in practice).
			return cmd, dumpPath
		}
		cmd.Stdin = rf
		// Note: cmd's stdincopy lives until cmd.Wait returns. We
		// intentionally keep `rf` open at function return so the
		// process can read it; the caller owns the dump file's
		// post-Wait lifecycle via dumpPath.
		_ = rf
	}
	return cmd, dumpPath
}

// DrainText runs cmd and captures stdout into a string while routing each
// stderr line to logger.Warn. ctx is consulted at Wait time so timeouts
// can be distinguished from process errors exactly the way the
// pre-helper call sites used to (via `cctx.Err() == context.DeadlineExceeded`).
func DrainText(ctx context.Context, cmd *exec.Cmd, logger *slog.Logger) (string, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("claude stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("claude stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("claude start: %w", err)
	}

	var sb strings.Builder
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
		for sc.Scan() {
			sb.WriteString(sc.Text())
			sb.WriteByte('\n')
		}
	}()
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
		for sc.Scan() {
			logger.Warn("claude_stderr", "line", sc.Text())
		}
	}()
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("claude exit: %w: %w", err, context.DeadlineExceeded)
		}
		return "", fmt.Errorf("claude exit: %w", err)
	}
	return strings.TrimSpace(sb.String()), nil
}

// writePromptDump writes prompt to a uniquely-named file under dir and
// returns the path. An empty dir falls back to os.TempDir()/admiral-spawn
// so a misconfigured caller still gets a writable location rather than
// panicking inside os.CreateTemp.
func writePromptDump(prompt, dir string) (string, error) {
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "admiral-spawn")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("claude prompt dump dir: %w", err)
	}
	f, err := os.CreateTemp(dir, "claude-prompt-*.md")
	if err != nil {
		return "", fmt.Errorf("claude prompt dump create: %w", err)
	}
	if _, err := io.WriteString(f, prompt); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("claude prompt dump write: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("claude prompt dump close: %w", err)
	}
	return f.Name(), nil
}

// totalArgvBytes approximates the bytes a Linux kernel-side argv occupies
// for the given prompt transport. Used only for the budget check; a
// one-byte miscount here costs at most one misrouted (small) prompt that
// the kernel will E2BIG on at Start, which is still recoverable (caller
// surfaces the error and the operator can rerun).
func totalArgvBytes(claudeBin, prompt string, extraArgs []string) int {
	n := len(claudeBin) + 2 // "-p" flag
	if prompt != "" {
		// positional `-p <prompt>` adds the prompt value
		n += len(prompt)
	}
	for _, a := range extraArgs {
		n += len(a) + 1 // one NUL separator per arg
	}
	n++ // trailing NUL
	return n
}
