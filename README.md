# IssueShell

IssueShell uses a private GitHub Issue as a durable command channel. A server CLI posts commands, a client process executes them in a persistent POSIX shell, and the result is stored in Issue comments for the server to retrieve.

The client runs arbitrary shell commands. Use a dedicated private repository and a narrowly scoped GitHub token. IssueShell deliberately refuses to execute commands from a public repository.

## Requirements

- Go 1.24 or newer
- macOS or Linux on the client machine
- A private GitHub repository
- A fine-grained personal access token with `Metadata: read` and `Issues: read/write` for that repository

The server and client may use separate tokens. Tokens are read only from `GITHUB_TOKEN`; they are never written to an Issue or the local state database.

## Install

```sh
go install ./cmd/issueshell-server ./cmd/issueshell-client
```

Make sure the Go binary directory is on `PATH`. To build into this repository instead:

```sh
mkdir -p bin
go build -o bin/issueshell-server ./cmd/issueshell-server
go build -o bin/issueshell-client ./cmd/issueshell-client
```

## Start a session

Set the repository and token on both machines:

```sh
export GITHUB_TOKEN='github_pat_...'
export ISSUESHELL_REPO='owner/private-repo'
```

Create the session from the server machine:

```sh
issueshell-server create
# 42    https://github.com/owner/private-repo/issues/42
```

Start the client on the machine that will execute commands:

```sh
issueshell-client run --issue 42
```

The client validates that the repository is private and that Issue `42` contains a valid IssueShell session header before it starts a shell.

## Send commands

Send one command and wait for its result:

```sh
issueshell-server send --issue 42 --command 'pwd && uname -a'
```

Read a multiline command from a file or stdin:

```sh
issueshell-server send --issue 42 --file deploy.sh
printf 'cd /srv/app\nmake test\n' | issueshell-server send --issue 42 --file -
```

The server writes only the remote command output to stdout, so normal redirection works:

```sh
issueshell-server send --issue 42 --command 'make test' > test-output.txt
```

Diagnostics and command IDs go to stderr. The server exits with the remote exit code, `130` after cancellation, or `255` for a transport/protocol error.

The client uses one persistent shell for the Issue, so state survives between commands:

```sh
issueshell-server send --issue 42 --command 'cd /srv/app && export STAGE=dev'
issueshell-server send --issue 42 --command 'pwd; printf "%s\n" "$STAGE"'
```

Shell redirection behaves normally on the client. `command > file` writes to a client-side file and does not return that text. Use `command | tee file` to save and return it.

## Cancel and close

Press Ctrl-C while `issueshell-server send` is waiting. The server posts a cancellation request, waits for the client to terminate the command's process group, and then exits with status `130`.

A second terminal can also cancel by the command ID printed by `send`:

```sh
issueshell-server cancel --issue 42 --command-id COMMAND_UUID
```

Close the session when finished:

```sh
issueshell-server close --issue 42
```

Closing the Issue directly has the same stop effect when the client next polls it.

## Execution and recovery model

- Commands are processed in Issue comment order, one at a time.
- Client output is printed locally while the command runs, then uploaded after completion in approximately 48 KiB chunks.
- Result manifests contain the command UUID, exit code, status, duration, chunk count, and SHA-256 hash.
- Client state is stored in SQLite under `~/.issueshell` by default. Set `ISSUESHELL_STATE_DIR` or use `--state-dir` to change it.
- Captured output is temporary and is deleted after the complete result is confirmed on GitHub.
- If the client stops during execution, it uploads any retained partial output as `interrupted` after restart and never reruns the command automatically.
- A canceled or unexpectedly exited shell is recreated. Its previous directory and environment state are necessarily lost.

## Limits

IssueShell supports non-interactive POSIX shell commands. Programs that require a TTY or live input, such as `vim`, password prompts, or interactive REPLs, are not supported. A command is limited to 48 KiB of valid UTF-8 text. Background processes that detach into another process group may outlive cancellation.

Every user able to write a correctly formed protocol comment in the private repository is trusted to execute commands. This version does not add an author allowlist or HMAC signatures.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
```
