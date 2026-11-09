# cap

Keep capability notes honest by testing them.

Teams and AI agents keep notes on what they can do: "the bot phone answers on port 5555", "the Stripe key in the vault works", "`codex` is installed on the build box". The notes go stale. Then an agent believes it can do something it can no longer do, or doubts something it can.

`cap` fixes that with one rule: every note carries a shell command that proves it. `cap check` runs the commands (locally or over ssh), and `cap check --write` records the result in the note's front matter. Run it nightly and the notes say what is true today.

No dependencies. One binary.

## Install

```sh
go install github.com/taktekhq/cap@latest
```

## Pages

`cap` reads a folder of Markdown files. A file counts as a capability page when its YAML front matter has an `id` or a `check` field. Everything else (indexes, lists) is skipped.

```markdown
---
id: bot-phone
description: Android test phone, adb over the tailnet
check: 'nc -z -w 5 100.108.31.45 5555'
check_on: mac
status: unverified
---
## Use
adb -s 100.108.31.45:5555 shell ...
```

| Field | Meaning |
|---|---|
| `id` | The page id. Default: the file name without `.md`. |
| `check` | A shell command on one line. Exit 0 means the capability works. `none` means there is nothing to test (a policy, a how-to). |
| `check_on` | Where to run it: `local` (default) or an ssh host (`mac`, `user@box`). Your own hostname also counts as local. |
| `status` | Written by `cap check --write`: `verified`, `failing`, `unknown` or `doc`. Set `retired` yourself for things that are gone; retired pages never run. |
| `last_verified` | Written: the last day the check passed. |
| `last_checked` | Written: the last day the check ran (pass or not). |
| `last_error` | Written: one line on why it did not pass (exit code, last stderr line). Removed on the next pass. |

## Commands

```sh
cap list  [--dir DIR] [--status S]
cap check [--dir DIR] [--write] [--verbose] [--timeout 30s] [--jobs 4] [id...]
cap version
```

`--dir` defaults to `$CAP_DIR`, else the current folder.

```console
$ cap check --dir capabilities
verified  bot-phone
failing   stripe-key  exit 22
unknown   xcode       host mac unreachable
doc       spend-policy
verified=118 failing=3 unknown=12 doc=60 retired=2 nocheck=0

$ cap list --status failing
ID          STATUS   ON     LAST_VERIFIED  LAST_CHECKED  CHECK
stripe-key  failing  mac    2026-10-30     2026-11-09    yes
```

Exit status: 0 when nothing failed, 1 when a check failed, 2 on a usage or file error. The last line of `cap check` is always the summary, so a script can parse it.

## How a check runs

- Local: `bash -lc '<check>'` in your home folder, so your login PATH applies.
- Remote: `ssh -o BatchMode=yes -o ConnectTimeout=10 <host> "bash -lc '<check>'"`. BatchMode means a check never waits on a password prompt.
- Each check has a timeout (default 30 s). On timeout the whole process group is killed, pipelines and ssh included.
- Up to `--jobs` checks run at once (default 4).
- Before running anything on an ssh host, `cap` tests that the host answers. If it does not, every check on that host is `unknown`, never `failing`: a machine that is asleep says nothing about whether the capability works. A remote check that exits 255 (ssh's own failure code) is re-tested the same way.

## Secrets

Checks often touch credentials, so `cap` is built not to show them:

- A check's stdout is never read. It goes to `/dev/null`.
- By default `cap check` prints only the outcome and exit code. `--verbose` adds the last 400 bytes of stderr for failing checks.
- `last_error` keeps the last stderr line, cut to 160 characters, with token-shaped strings (24+ letters and digits) replaced by `[redacted]`.

That is the second line of defence. The first is how you write checks:

- Send anything secret to `/dev/null`: `vault get stripe-key >/dev/null`.
- Pass tokens on stdin, not on the command line: `{ printf 'Authorization: Bearer '; vault get api-token; } | curl -fsS -o /dev/null -H @- https://api.example.com/me`.
- Prefer exit codes: `curl -f`, `grep -q`, `test -x`, `command -v`, `systemctl is-active`.
- Keep checks read-only: no writes, no messages sent, no paid calls, no login attempts that could lock an account.

## Run it nightly

A systemd user timer, for example:

```ini
# ~/.config/systemd/user/cap-check.service
[Service]
Type=oneshot
ExecStart=%h/go/bin/cap check --write --dir %h/notes/capabilities
SuccessExitStatus=1

# ~/.config/systemd/user/cap-check.timer
[Timer]
OnCalendar=*-*-* 04:30
Persistent=true
[Install]
WantedBy=timers.target
```

`SuccessExitStatus=1` keeps a failing capability from marking the unit failed. Commit the folder afterwards if you keep it in git; `--write` touches only front matter, so diffs stay small.

## Tests

```sh
go test ./...
```

## License

MIT
