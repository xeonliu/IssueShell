# IssueShell

IssueShell uses a private GitHub Issue as a durable command channel. A server CLI posts commands, a client process executes them in a persistent POSIX shell, and the result is stored in Issue comments for the server to retrieve.

The client runs arbitrary shell commands. Use a dedicated private repository and a narrowly scoped GitHub token. IssueShell deliberately refuses to execute commands from a public repository.

## Requirements

- Go 1.24 or newer
- macOS or Linux on the client machine
- A private GitHub repository
- A fine-grained personal access token with `Metadata: read` and `Issues: read/write` for that repository

IssueShell uses only the Go standard library. Building from this checkout does not download third-party Go modules.

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

## Configure the GitHub token

IssueShell requires a token on both the server and client because both sides read and write Issue comments. A fine-grained personal access token is recommended:

1. Open GitHub **Settings** -> **Developer settings** -> **Personal access tokens** -> **Fine-grained tokens**.
2. Select **Generate new token** and set an expiration date.
3. Set **Resource owner** to the user or organization that owns the private repository.
4. Under **Repository access**, choose **Only select repositories**, then select the IssueShell repository.
5. Under **Repository permissions**, set **Issues** to **Read and write**. **Metadata** remains **Read-only** automatically.
6. Generate the token and store it in a password manager or another secret store. GitHub shows it only once.

No `Contents`, `Actions`, administration, or account-level permission is required. A classic token with the broad `repo` scope also works, but is not recommended.

For an organization repository, the token may remain unusable until an organization administrator approves it. If the organization enforces SAML SSO, authorize the token for that organization as well.

Set the token and repository in the environment on each machine that runs IssueShell:

```sh
export GITHUB_TOKEN='github_pat_...'
export ISSUESHELL_REPO='owner/private-repo'
```

Avoid placing a real token in this repository, command arguments, Issue comments, screenshots, or shared shell history. IssueShell reads it only from `GITHUB_TOKEN`, so the server and client can use different tokens with access to the same repository.

Verify the token before starting a session:

```sh
curl -sS -o /dev/null -w '%{http_code}\n' \
  -H "Accept: application/vnd.github+json" \
  -H "Authorization: Bearer $GITHUB_TOKEN" \
  -H "X-GitHub-Api-Version: 2022-11-28" \
  "${GITHUB_API_URL:-https://api.github.com}/repos/$ISSUESHELL_REPO"
```

The expected response is `200`. `401` means the token is invalid or expired, `403` usually means approval/SSO or policy restrictions, and `404` usually means the repository name is wrong or the token was not granted access to that private repository.

For GitHub Enterprise Server, point IssueShell and the verification request at the instance API URL:

```sh
export GITHUB_API_URL='https://github.example.com/api/v3'
```

### TLS interception and restricted networks

If IssueShell reports `x509: certificate signed by unknown authority`, the network is usually intercepting TLS with a private CA, or GitHub Enterprise is using a private certificate. The preferred fix is to install that CA certificate into the operating system trust store.

If installing the CA is not possible, certificate verification can be disabled explicitly on each affected machine:

```sh
export GITHUB_INSECURE_SKIP_VERIFY=true
```

Then run IssueShell normally:

```sh
issueshell-client run --issue 42
issueshell-server send --issue 42 --command 'uname -a'
```

Both programs print a warning while insecure TLS is enabled. This setting allows a network intermediary to read the GitHub token, inspect results, and replace commands or responses. Use it only on a network you understand, never make it a project default, and disable it after leaving that environment:

```sh
unset GITHUB_INSECURE_SKIP_VERIFY
```

Only `true` or `false` should be used. Omitting the variable or setting it to `false` restores normal certificate and hostname verification.

## Start a session

After configuring the repository and token on both machines, create the session from the server machine:

```sh
export GITHUB_TOKEN='github_pat_...'
export ISSUESHELL_REPO='owner/private-repo'
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
- Client state is stored as an atomically updated JSON file under `~/.issueshell` by default. Set `ISSUESHELL_STATE_DIR` or use `--state-dir` to change it.
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
