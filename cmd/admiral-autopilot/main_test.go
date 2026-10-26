package main

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/georgehuang/admiral/internal/config"
	"github.com/georgehuang/admiral/internal/store"
)

func TestSeedRepos_ValidatesGhUser(t *testing.T) {
	cfg := &config.Autopilot{Repos: []config.RepoConfig{
		{ProjectID: "p1", ProjectName: "klink", RepoDir: "/r/klink", BaseBranch: "main", GhUser: "virgil2019"},
		{ProjectID: "p2", ProjectName: "other", RepoDir: "/r/other", BaseBranch: "main", GhUser: "chababa9"},
	}}

	t.Run("all valid seeds gh_user", func(t *testing.T) {
		db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		ok := func(context.Context, string, string) error { return nil }
		if err := seedRepos(db, cfg, ok, slog.Default()); err != nil {
			t.Fatalf("seedRepos: %v", err)
		}
		r, _ := db.GetRepoByProjectID("p1")
		if r == nil || r.GhUser != "virgil2019" {
			t.Fatalf("expected seeded gh_user=virgil2019, got %+v", r)
		}
	})

	t.Run("invalid account fails boot and seeds nothing", func(t *testing.T) {
		db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		validate := func(_ context.Context, user, _ string) error {
			if user == "chababa9" {
				return errors.New(`gh account "chababa9" cannot see virgil2019/other`)
			}
			return nil
		}
		err = seedRepos(db, cfg, validate, slog.Default())
		if err == nil || !strings.Contains(err.Error(), "other") || !strings.Contains(err.Error(), "chababa9") {
			t.Fatalf("expected error naming the repo and account, got %v", err)
		}
		if repos, _ := db.ListRepos(); len(repos) != 0 {
			t.Fatalf("expected no repos seeded on validation failure, got %d", len(repos))
		}
	})
}
