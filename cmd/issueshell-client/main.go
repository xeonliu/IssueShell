//go:build darwin || linux

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	clientapp "github.com/xeonliu/IssueShell/internal/client"
	"github.com/xeonliu/IssueShell/internal/githubapi"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: issueshell-client run [options]")
		os.Exit(2)
	}
	flags := flag.NewFlagSet("issueshell-client", flag.ExitOnError)
	repo := flags.String("repo", os.Getenv("ISSUESHELL_REPO"), "GitHub repository (OWNER/REPO)")
	issue := flags.Int("issue", 0, "Issue number")
	shell := flags.String("shell", "", "POSIX shell executable (defaults to $SHELL or /bin/sh)")
	poll := flags.Duration("poll-interval", 5*time.Second, "GitHub polling interval")
	stateDir := flags.String("state-dir", defaultStateDir(), "Local state directory")
	_ = flags.Parse(os.Args[2:])
	api, err := githubapi.New(os.Getenv("GITHUB_TOKEN"), os.Getenv("GITHUB_API_URL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "issueshell-client:", err)
		os.Exit(1)
	}
	if api.InsecureTLS() {
		fmt.Fprintln(os.Stderr, "WARNING: TLS certificate verification is disabled; GitHub tokens and commands may be intercepted")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = clientapp.Run(ctx, clientapp.Config{
		API: api, Repo: *repo, Issue: *issue, Shell: *shell,
		PollInterval: *poll, StateDir: *stateDir, Output: os.Stdout, ErrorOutput: os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "issueshell-client:", err)
		os.Exit(1)
	}
}

func defaultStateDir() string {
	if value := os.Getenv("ISSUESHELL_STATE_DIR"); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".issueshell"
	}
	return filepath.Join(home, ".issueshell")
}
