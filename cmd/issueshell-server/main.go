package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xeonliu/IssueShell/internal/githubapi"
	"github.com/xeonliu/IssueShell/internal/server"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	api, err := githubapi.New(os.Getenv("GITHUB_TOKEN"), os.Getenv("GITHUB_API_URL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "issueshell-server:", err)
		return 255
	}
	if api.InsecureTLS() {
		fmt.Fprintln(os.Stderr, "WARNING: TLS certificate verification is disabled; GitHub tokens and commands may be intercepted")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "create":
		flags := flag.NewFlagSet("create", flag.ContinueOnError)
		flags.SetOutput(os.Stderr)
		repo := flags.String("repo", envRepo(), "GitHub repository (OWNER/REPO)")
		title := flags.String("title", "", "Issue title")
		if flags.Parse(args[1:]) != nil {
			return 2
		}
		issue, err := server.Create(ctx, api, *repo, *title)
		if err != nil {
			return report(err)
		}
		fmt.Printf("%d\t%s\n", issue.Number, issue.HTMLURL)
		return 0
	case "send":
		return runSend(api, args[1:])
	case "cancel":
		flags := flag.NewFlagSet("cancel", flag.ContinueOnError)
		flags.SetOutput(os.Stderr)
		repo := flags.String("repo", envRepo(), "GitHub repository (OWNER/REPO)")
		issue := flags.Int("issue", 0, "Issue number")
		commandID := flags.String("command-id", "", "Command UUID")
		if flags.Parse(args[1:]) != nil {
			return 2
		}
		if err := server.Cancel(ctx, api, *repo, *issue, *commandID); err != nil {
			return report(err)
		}
		return 0
	case "close":
		flags := flag.NewFlagSet("close", flag.ContinueOnError)
		flags.SetOutput(os.Stderr)
		repo := flags.String("repo", envRepo(), "GitHub repository (OWNER/REPO)")
		issue := flags.Int("issue", 0, "Issue number")
		if flags.Parse(args[1:]) != nil {
			return 2
		}
		if err := server.Close(ctx, api, *repo, *issue); err != nil {
			return report(err)
		}
		return 0
	default:
		usage()
		return 2
	}
}

func runSend(api *githubapi.Client, args []string) int {
	flags := flag.NewFlagSet("send", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repo := flags.String("repo", envRepo(), "GitHub repository (OWNER/REPO)")
	issue := flags.Int("issue", 0, "Issue number")
	command := flags.String("command", "", "Shell command text")
	file := flags.String("file", "", "Read command from file; use - for stdin")
	poll := flags.Duration("poll-interval", 5*time.Second, "GitHub polling interval")
	if flags.Parse(args) != nil {
		return 2
	}
	if (*command == "") == (*file == "") {
		return report(errors.New("exactly one of --command or --file is required"))
	}
	commandText := *command
	if *file != "" {
		var data []byte
		var err error
		if *file == "-" {
			data, err = io.ReadAll(io.LimitReader(os.Stdin, server.MaxCommandBytes+1))
		} else {
			var source *os.File
			source, err = os.Open(*file)
			if err == nil {
				data, err = io.ReadAll(io.LimitReader(source, server.MaxCommandBytes+1))
				closeErr := source.Close()
				if err == nil {
					err = closeErr
				}
			}
		}
		if err != nil {
			return report(err)
		}
		commandText = string(data)
	}
	interrupts := make(chan os.Signal, 2)
	signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupts)
	exitCode, err := server.Send(context.Background(), api, *repo, *issue, commandText, *poll, os.Stdout, os.Stderr, interrupts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "issueshell-server:", err)
		return 255
	}
	return normalizeExitCode(exitCode)
}

func normalizeExitCode(code int) int {
	if code < 0 || code > 255 {
		return 255
	}
	return code
}

func envRepo() string { return os.Getenv("ISSUESHELL_REPO") }

func report(err error) int {
	fmt.Fprintln(os.Stderr, "issueshell-server:", err)
	return 255
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: issueshell-server <create|send|cancel|close> [options]")
}
