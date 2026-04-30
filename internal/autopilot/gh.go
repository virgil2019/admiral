package autopilot

import (
	"context"
	"strings"
)

// GhAuthStatus is the result of gh auth status for a given host.
type GhAuthStatus struct {
	OK              bool
	CurrentUser     string
	StatusOutput    string
	GHTokenValid    bool
}

// checkGhAuth runs `gh auth status -h <host>` in repoDir to verify the user
// is authenticated for that GitHub host. host should be the hostname
// (e.g. "github.com" or the enterprise server URL).
func checkGhAuth(ctx context.Context, repoDir, ghBin, host string) (*GhAuthStatus, error) {
	// gh auth status -h <host> exits 0 if authenticated for that host, 1 otherwise
	out, err := captureCmd(ctx, repoDir, ghBin, "auth", "status", "-h", host)
	result := &GhAuthStatus{
		OK:           err == nil,
		StatusOutput: strings.TrimSpace(out),
	}
	if err != nil {
		// Not authenticated or gh errored
		return result, nil
	}
	// Parse current user from output. gh auth status prints a line like:
	//   github.com
	//     ✓ Logged in to github.com as <username> (...)
	// Extract the username when present.
	lines := strings.Split(result.StatusOutput, "\n")
	for _, line := range lines {
		if strings.Contains(line, "Logged in to") || strings.Contains(line, "✓") {
			parts := strings.Fields(line)
			for i, p := range parts {
				if (p == "as" || p == "to") && i+1 < len(parts) {
					result.CurrentUser = parts[i+1]
					result.GHTokenValid = true
					break
				}
			}
			break
		}
	}
	return result, nil
}
