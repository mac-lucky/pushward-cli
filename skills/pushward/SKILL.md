---
name: pushward
description: Reach the user's iPhone through the pushward CLI - push notifications when a task finishes or gets stuck, a question with buttons that waits for their tap, a Live Activity on the Lock Screen and Dynamic Island showing progress, a home screen widget value, a scheduled reminder, or an email. Use when the user says things like "notify me when done", "ping my phone", "ask me before you deploy", "show progress on my lock screen", "remind me tomorrow", and a shell can run `pushward`. If PushWard MCP tools are connected instead, use those.
---

# PushWard CLI

`pushward` sends to the user's own devices through the PushWard API. This skill is the
part that decides what to send and when; the CLI does the HTTP, retries and waiting.

## Before the first send

```sh
pushward version
pushward auth status
```

Not installed: `brew install mac-lucky/tap/pushward`, or
`go install github.com/mac-lucky/pushward-cli/cmd/pushward@latest`. Ask before installing.
Everything below works on 1.2.0 and later; the organization target flags need 1.3.0, and
encryption and `--ack` need 1.4.0.

`auth status` exiting 3 means no usable key. The user has to fix that themselves, in their own
terminal: `pushward auth login` prompts for the `hlk_` integration key from the app's
Settings, or they export `PUSHWARD_API_TOKEN`. Never ask them to paste the key into the chat,
and never put it on a command line; the CLI has no token flag for that reason.

If they want the agent on a short leash, they can give it a key of its own. They run this
themselves, in the shell they start the agent from, because the output is a new secret:

```sh
export PUSHWARD_API_TOKEN=$(pushward key create agent --activities manage --activity-slugs 'agent-*' \
  --widgets=write --widget-slugs 'agent-*' --notifications=schedule --expires 90d --jq .key)
```

The variable wins over the stored key, so the agent then acts only on activities and widgets
whose slug starts with `agent-`, which is why the examples below use that prefix. Never run
`pushward key` commands yourself unless the user asks.

## Pick the surface

| The user wants | Send |
|---|---|
| to know when something finished or failed | `pushward notify` |
| to be told right away that you are blocked | `pushward notify --level time-sensitive` |
| an alert that must not go unseen (an outage, a failed backup) | `pushward notify --ack`, repeating until they acknowledge it |
| to approve, pick an option, or type a reply | `pushward notify --action ...`, then `notification answer --wait`; or an approval Live Activity |
| to watch a long task move | a Live Activity: `activity start`, a few `activity update`, `activity end` |
| a number they can glance at later (coverage, queue length, spend) | `pushward widget create` once, then `widget update` |
| a reminder later or on a repeat | `pushward schedule create --in` / `--at` / `--cron` |
| a report or log they will read at a desk | `pushward email send` (only to addresses they verified in the app) |

One notification at the end beats five along the way. Do not notify about things the user is
watching you do in the terminal right now; notify when they would otherwise have to come back
and check.

## Notifications

```sh
pushward notify --title "Tests pass on feat/login" --body "412 passed in 3m10s" -q
pushward notify --title "Blocked: migration needs a decision" --body "users.email has 31 duplicates" --level time-sensitive
tail -c 4000 summary.txt | pushward notify --title "Refactor done" --body -
pushward notify --title "Preview ready" --body "Deployed to staging" --url https://staging.example.com
```

Titles carry the news; bodies carry the one detail that matters (counts, durations, the
branch). A body holds at most 4096 characters. `--level` is `passive`, `active` (default),
`time-sensitive` or `critical`; keep `time-sensitive` for "I am stuck until you answer" and
never use `critical` unless asked. `--collapse-id` replaces an earlier notification with the
same id instead of stacking a new one.

## Asking and waiting

Send the question first and keep its id, then wait in steps:

```sh
id=$(pushward notify --title "Merge PR #42?" --body "CI green, 3 files changed" \
  --action merge=Merge --action 'hold=Not yet' --jq .id)
pushward notification answer "$id" --wait 9m --jq .action_id
```

Exit 0 prints the chosen action id. Exit 7 means no answer yet: run the same `notification
answer` command again, which waits on the same question and sends nothing. Keep every wait
shorter than the timeout of the tool that runs it: Claude Code kills a command after 2 minutes
unless you give it a longer timeout (10 minutes at most), so run the one above with a
10-minute timeout. A killed wait leaves you with no answer and no exit code. If they never
answer, report that as "no decision", not as a no.

Actions must not carry a URL if you want the answer back. To let them type a reply, add a
button with a text field, `-F 'actions[]={"id":"reply","title":"Reply","text_input":true}'`,
and read the reply with `--jq .text`.

For a question that should sit on the Lock Screen until answered, use an approval card. Give
every question a new slug: an approval that is started again with the same buttons keeps the
answer from last time.

```sh
slug=agent-release-ask-$(date +%s)
pushward activity start "$slug" --name Release --template approval --text "Ship 2.4 to production?" \
  --option ship=Ship --option hold=Hold --ended-ttl 15m
pushward activity wait "$slug" --timeout 9m --jq .content.answer.option
```

`activity wait` exits 7 on timeout like `notification answer`; run it again to keep waiting.
The server ends the card shortly after the tap. If you give up, `pushward activity end "$slug"`
so a stale question does not stay on the Lock Screen.

## Repeating until acknowledged

`--ack` sends the notification again (every minute by default) until someone taps Acknowledge
on one of their devices, or until it expires (after an hour by default). Keep it for alerts
that must not be missed; a finished task is not one.

```sh
id=$(pushward notify --title "Backup failed on nas-1" --body "rsync exit 23, 0 of 412 GB copied" \
  --level time-sensitive --ack --ack-repeat 5m --ack-expire 2h --tag nas-1 --jq .id)
pushward receipt get "$id" --wait 9m --jq .status
pushward receipt cancel --tag nas-1
```

`--ack-repeat` takes 30s to 1h and `--ack-expire` 1m to 3h; repeats do not use up the quota.
`--action` buttons without a url acknowledge it too, and the receipt's `action_id` says which
one was tapped. `receipt get --wait` exits 7 when the wait runs out, as `notification answer`
does, and also when the alert expired or was canceled unacknowledged (`--jq .status` tells
which). When the problem clears on its own, cancel the repeats by id
(`pushward receipt cancel 42`) or by tag. A key can have 25 alerts repeating at once
(`notification_receipt.limit_exceeded` beyond that), and a new one with the same
`--collapse-id` replaces the old one's repeats. `--callback-url` and `pushward receipt secret`
are for the user's own webhook receivers.

## Progress on the Lock Screen

```sh
pushward activity start agent-auth-refactor --name "Auth refactor" --template steps \
  --step 1/4 --step-labels Plan,Edit,Test,Commit --text Planning --stale-ttl 2h --ended-ttl 30m
pushward activity update agent-auth-refactor --step 3/4 --text "Running 412 tests"
pushward activity end agent-auth-refactor --status success --text "Merged as 3f2c1a9"
```

- Make the slug from the task (`agent-<repo>-<task>`), so a retry updates the same card
  instead of starting a second one. Slugs take letters, digits, `-` and `_`, up to 128, so
  turn `feat/login` into `feat-login`. Starting an existing slug restarts it.
- Always pass `--ended-ttl` to `activity start`. An account holds 50 activities and an ended
  one counts for 30 days unless `--ended-ttl` deletes it sooner; one card per task fills that
  in a few weeks, and then every integration on the account gets `activity.limit_exceeded`.
- Always end what you start: `--status success`, `failure` or `cancelled`, including when
  you hit an error or the user stops you. A forgotten activity sits on their Lock Screen until
  `--stale-ttl` ends it, which is why you set one. `activity end` is safe to run again: on an
  activity that already ended it does nothing and exits 0.
- `end --status` shows a final frame (green check, red cross, grey stop) for 4 seconds before
  ending, so the last thing on screen is the outcome.
- Update on milestones, not on every log line. Each update is a push, free accounts have a
  monthly update allowance (`pushward me` prints usage), and `activity start` costs two.
- Use `generic` with `--progress 0.4` for one long job with a known fraction, `steps` for a
  sequence. The other templates and their fields: [references/live-activities.md](references/live-activities.md).

## Widgets, schedules, email

```sh
pushward widget create agent-coverage --name Coverage --template progress --value 0.81 --label api
pushward widget update agent-coverage --value 0.84

pushward schedule create --in 2h --title "Check the canary" --body "Error rate after the 14:00 deploy"
pushward schedule create --cron "0 9 * * 1-5" --tz Europe/Warsaw --title Standup --body "In 5 minutes"
pushward schedule list
pushward schedule cancel <id> --purge

pushward email send --to ops@example.com --subject "Nightly report" --text-file report.txt
```

A scheduled notification is held by the server, so nothing needs to keep running. Cron
sends must be at least 15 minutes apart, and an account can have 25 pending. Keep the `id`
from the create output if you may need to cancel it.

## Organization keys

With a key from a PushWard organization (a team account), sends go to the members' devices
that the organization's routing rules allow. These flags need CLI 1.3.0 and narrow that
further, by group name, device tag name or member user id:

```sh
pushward notify --title "db-1 disk at 95%" --body "Paging on-call" --level time-sensitive --target-groups oncall
pushward activity start agent-deploy-api --name "Deploy api" --text Building --target-tags wall
pushward activity update agent-deploy-api --target-groups oncall,sre
pushward activity update agent-deploy-api --no-target
```

`notify`, `schedule create`, `activity create`, `activity start` and `activity update` take
`--target-groups`, `--target-tags` and `--target-members`. On an update the new target
replaces the stored one and `--no-target` sends the activity back to everyone the rules
allow; devices that lose it end it, devices that gain it start it. A key the admins limited
to some groups and tags must stay inside them (403 otherwise) and only sees activities sent
inside them: anything else answers 404. A personal key gets 422 for any target.

## Encrypted notifications

When the user has set up an encryption key (`pushward e2e key-id` exits 0), `notify` and
`schedule create` encrypt the title, subtitle, body and url before sending, with no change to
how you call them. Only their devices holding the key can read that text. Level, actions,
metadata, thread, source and target stay readable to the server and to Apple, so never put a
secret in those. Encrypted text has room for about 2,200 bytes in total, much less than the
4096 a plain body can take.

```sh
pushward e2e key-id
pushward notify --title "Prod DB password rotated" --body "New one is in the vault under db/prod" --encrypt
```

`--encrypt` makes the send fail when no key is set rather than go out readable; use it when the
user asked for encryption. `--no-encrypt` sends one in the clear, for example with an
organization key, which cannot send encrypted (`notification.encryption_unavailable`). Leave
`pushward e2e generate` and `pushward e2e import` to the user: they print or take the key
itself. `pushward api` never encrypts. Needs CLI 1.4.0.

## Reading output

Piped, every command prints the API response as JSON; on a terminal it prints a summary. Some
agent shells look like a terminal, so pass `--jq <expr>` or `--json` whenever you parse the
output. `-q` prints nothing. The `--jq` expression is checked before the request goes out.

| Exit | Meaning |
|---|---|
| 0 | ok |
| 1 | other error, for example a 422 validation failure or a full account |
| 2 | bad usage |
| 3 | the key is missing or invalid, or it may not do this (a 403) |
| 4 | not found |
| 5 | rate limited or out of quota |
| 6 | server error or network failure |
| 7 | a wait ran out, or an alert sent with `--ack` ended unacknowledged |

On exit 3, read the error code on stderr before asking the user to log in again: a slug
outside the key's `agent-*` limit or a missing permission gives a 403 with its own code.

429s and 503s are already retried for up to a minute. Do not wrap a send in your own retry
loop: a POST that failed with exit 6 may still have delivered, and a second try sends the
notification twice.

Anything without a dedicated flag goes in with `-f key=value` (string), `-F key=value` (typed:
numbers, booleans, null, JSON) or `--data` (a JSON body, `@file` or `-`). `--data` is applied
first, then the command's own flags, then `-f`, then `-F`; later ones win. `-F key=@file` reads
the file as a string; for JSON from a file use `-F "key=$(cat file.json)"` or `--data @file`.
`pushward api <path>` reaches endpoints that have no command yet, and
`pushward <command> --help` is the reference for every flag.
