// admiral-planner-mcp is the stdio MCP server a host agent
// (claude / codex / ...) calls during requirement decomposition and
// PR-verification flows. It records features → issues → acceptance
// criteria, and later submits verdicts back to GitHub and Linear.
//
// All judgment ("does this PR meet the criteria") happens in the
// host agent's LLM — this server holds the memory and acts as the
// agent's hands. No LLM call originates here.
//
// Configure the host agent (e.g. ~/.claude.json) to launch this
// binary with the ADMIRAL_DB_PATH env var pointing at the admiral
// SQLite file (same DB admiral itself uses). Linear / GitHub OAuth
// tokens are read from that DB, not from env, so they aren't leaked
// to the host agent's process tree.
package main

import (
	"context"
	"log"
	"os"

	ghpkg "github.com/georgehuang/admiral/internal/github"
	"github.com/georgehuang/admiral/internal/planner"
	"github.com/georgehuang/admiral/internal/store"
)

func main() {
	// All logs go to stderr; stdout is reserved for JSON-RPC.
	log.SetOutput(os.Stderr)
	log.SetPrefix("[admiral-planner-mcp] ")
	log.SetFlags(0)

	dbPath := os.Getenv("ADMIRAL_DB_PATH")
	if dbPath == "" {
		log.Fatal("ADMIRAL_DB_PATH not set — point at the admiral SQLite file")
	}
	db, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// GitHub token is read from env to keep this binary independent of
	// admiral's config.yaml. Optional: tools that need it (pr_get_materials)
	// return a tool-level error when gh is nil, so a host agent doing
	// only feature_* reads still works.
	var gh planner.PRDiffer
	if tok := os.Getenv("ADMIRAL_GH_TOKEN"); tok != "" {
		gh = ghpkg.NewClient(tok)
	} else {
		log.Print("ADMIRAL_GH_TOKEN not set — pr_get_materials will return an error until configured")
	}

	tools := planner.BuildTools(db, gh)
	srv := planner.NewServer(os.Stdin, os.Stdout, os.Stderr, tools)
	if err := srv.Run(context.Background()); err != nil {
		log.Fatalf("server: %v", err)
	}
}
