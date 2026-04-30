// admiral-oauth is a CLI tool that performs the Linear OAuth authorization
// code flow and stores the resulting access/refresh tokens in the SQLite
// token store, so users don't need to run an external script to bootstrap
// their admiral setup.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/georgehuang/admiral/internal/config"
	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

func main() {
	var cfgPath string
	var listenAddr string
	flag.StringVar(&cfgPath, "config", config.DefaultConfigPath(), "path to config.yaml")
	flag.StringVar(&listenAddr, "listen", "", "local listen address for the OAuth callback HTTP server (e.g. :8080). When empty, defaults to the port in redirect_uri for localhost/127.0.0.1, or :8080 otherwise. Use this when redirect_uri is a public tunnel URL (e.g. cloudflared) so the local server binds an unprivileged port.")
	flag.Parse()

	cfg, err := config.LoadAutopilot(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	if err := runLogin(cfg, listenAddr); err != nil {
		fmt.Fprintf(os.Stderr, "OAuth login failed: %v\n", err)
		os.Exit(1)
	}
}

func runLogin(cfg *config.Config, listenAddrOverride string) error {
	// Validate required config fields
	if strings.TrimSpace(cfg.Linear.ClientID) == "" {
		return fmt.Errorf("linear.client_id is required (create a Linear OAuth app first)")
	}
	if strings.TrimSpace(cfg.Linear.ClientSecret) == "" {
		return fmt.Errorf("linear.client_secret is required (create a Linear OAuth app first)")
	}

	// Open SQLite store
	db, err := store.Open(cfg.Storage.SQLitePath)
	if err != nil {
		return fmt.Errorf("open sqlite store: %w", err)
	}
	defer db.Close()

	// Parse redirect URI to determine listen port
	redirectURI := cfg.Linear.RedirectURI
	if strings.TrimSpace(redirectURI) == "" {
		redirectURI = "http://127.0.0.1:8080/callback"
	}
	u, err := url.Parse(redirectURI)
	if err != nil {
		return fmt.Errorf("invalid redirect_uri: %w", err)
	}
	// Listen address derivation. The redirect_uri tells us where Linear sends
	// the user back to (public, may be a tunnel URL); the local HTTP server
	// has to bind some local port that the tunnel forwards to. When the
	// caller passes -listen, that wins. Otherwise: a localhost redirect_uri
	// uses its own port (no tunnel), and any other host falls back to :8080
	// (assume a tunnel is in front; never try to bind :443, which is
	// privileged and almost certainly wrong here).
	listenAddr := strings.TrimSpace(listenAddrOverride)
	if listenAddr == "" {
		host := u.Hostname()
		if host == "127.0.0.1" || host == "localhost" {
			port := u.Port()
			if port == "" {
				port = "8080"
			}
			listenAddr = ":" + port
		} else {
			listenAddr = ":8080"
		}
	}

	// Generate CSRF state
	state, err := generateState(16)
	if err != nil {
		return fmt.Errorf("generate state: %w", err)
	}

	// Build authorization URL
	authURL := linear.BuildAuthorizeURL(cfg.Linear.ClientID, redirectURI, linear.OAuthScopes, state)

	// Setup done channel and callback server
	done := make(chan struct{}, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", handleCallback(cfg, db, state, redirectURI, done, errCh))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Not found", http.StatusNotFound)
	})

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Start server in background
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("server: %w", err)
		}
	}()

	// Open browser
	fmt.Println("Opening browser to authorize admiral on Linear...")
	fmt.Println()
	if err := openBrowser(authURL); err != nil {
		fmt.Println("Failed to open browser. Please open this URL manually:")
		fmt.Println(authURL)
	} else {
		fmt.Println("If the browser doesn't open, visit this URL:")
		fmt.Println(authURL)
	}
	fmt.Println()
	fmt.Println("Waiting for callback from Linear...")

	// Wait for callback or error
	select {
	case err := <-errCh:
		return fmt.Errorf("server error: %w", err)
	case <-done:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}

	fmt.Println()
	fmt.Println("OAuth login successful!")
	fmt.Println("Access and refresh tokens have been saved to the token store.")
	fmt.Println()
	fmt.Println("Next: run `admiral-autopilot` to start the autopilot daemon.")
	return nil
}

func handleCallback(cfg *config.Config, db *store.Store, expectedState, redirectURI string, done chan<- struct{}, errCh chan<- error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		// Check for error response (user denied)
		if errParam := q.Get("error"); errParam != "" {
			errDesc := q.Get("error_description")
			if errParam == "access_denied" {
				fmt.Fprintf(w, "<h1>Authorization Declined</h1><p>You denied the authorization request.</p><p>Run <code>admiral-oauth login</code> again to retry.</p>")
				errCh <- fmt.Errorf("authorization declined (user denied)")
				return
			}
			fmt.Fprintf(w, "<h1>OAuth Error</h1><p><strong>%s:</strong> %s</p>", errParam, errDesc)
			errCh <- fmt.Errorf("oauth error: %s — %s", errParam, errDesc)
			return
		}

		// Validate state (CSRF protection)
		state := q.Get("state")
		if state != expectedState {
			fmt.Fprintf(w, "<h1>CSRF Mismatch</h1><p>State mismatch. Re-run <code>admiral-oauth login</code>.</p>")
			errCh <- fmt.Errorf("CSRF state mismatch")
			return
		}

		code := q.Get("code")
		if code == "" {
			fmt.Fprintf(w, "<h1>Missing Code</h1><p>No authorization code received.</p>")
			errCh <- fmt.Errorf("no authorization code in callback")
			return
		}

		// Exchange code for token
		tokenEndpoint := "https://api.linear.app/oauth/token"
		resp, err := linear.ExchangeCode(r.Context(), tokenEndpoint, cfg.Linear.ClientID, cfg.Linear.ClientSecret, redirectURI, code)
		if err != nil {
			fmt.Fprintf(w, "<h1>Token Exchange Failed</h1><p><strong>Error:</strong> %v</p>", err)
			errCh <- fmt.Errorf("exchange code: %w", err)
			return
		}

		// Calculate expires_at
		expiresAt := ""
		if resp.ExpiresIn > 0 {
			expiresAt = time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
		}

		// Save token to store (overwrite existing — user is intentionally re-OAuthing)
		if err := db.SaveLinearOAuthToken(resp.AccessToken, resp.RefreshToken, expiresAt); err != nil {
			fmt.Fprintf(w, "<h1>Token Save Failed</h1><p>Received token but could not save it: %v</p>", err)
			errCh <- fmt.Errorf("save token: %w", err)
			return
		}

		// Return success page
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<h1>OAuth OK</h1><p>Check your terminal. Close this tab.</p>`)
		close(done)
	}
}

func generateState(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
	return cmd.Run()
}