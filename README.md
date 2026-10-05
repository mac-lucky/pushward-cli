# pushward-cli

`pushward` is a command-line client for [PushWard](https://pushward.app): push notifications, Live Activities, home screen widgets, scheduled notifications and email, from a shell script, a cron job or CI. It covers every operation in the public API, and anything newer is reachable through `pushward api`.

The same binary is the runtime of [pushward-action](https://github.com/mac-lucky/pushward-action) for GitHub Actions.

## Install

```sh
brew install mac-lucky/tap/pushward
```

Or `go install github.com/mac-lucky/pushward-cli/cmd/pushward@latest`, or grab an archive from [Releases](https://github.com/mac-lucky/pushward-cli/releases). The macOS binaries in those archives are not signed, so a copy downloaded with a browser needs `xattr -d com.apple.quarantine pushward` before macOS will run it; Homebrew and `go install` never hit that. There is also an image:

```sh
docker run --rm -e PUSHWARD_API_TOKEN ghcr.io/mac-lucky/pushward-cli notify --title Hi --body There
```

## Agent skill

```sh
npx skills add mac-lucky/pushward-cli --skill pushward
```

This installs a skill for Claude Code, Codex, Cursor and other agents that read `SKILL.md` files. It teaches them which command fits "ping me when the tests pass", "ask me before you push" or "show the migration on my lock screen", and to end every Live Activity they start. The agent uses the key the CLI already has, so run `pushward auth login` yourself first. To keep it away from your other activities, start it with a key of its own limited to `agent-*` slugs; the skill has the `pushward key create` line for that, and you run it, not the agent, since its output is a new key.

## Key

Copy an integration key (`hlk_...`) from the app (Settings, Integration Key), then either export it or store it once:

```sh
export PUSHWARD_API_TOKEN=hlk_...
pushward auth login          # prompts, checks the key, writes ~/.config/pushward/config.json (0600)
pushward auth status
```

The environment variable wins over the stored key. There is no `--token` flag on purpose, so the key stays out of shell history and `ps`.

With the account's default key you can create, change, roll and revoke its other keys, for example to give each bridge or cron job its own key limited to what it needs:

```sh
pushward key list
pushward key create backup --notifications=send --activity-slugs 'backup-*' --jq .key   # prints only the new key
pushward key create alerts --activities none --notifications=send
pushward key create dashboard --activities read --widgets=read --expires 90d
pushward key update <key-id> --all-activities --widgets=write --no-expiry
pushward key roll <key-id>
pushward key revoke <key-id>
```

Each key has one level per resource: `--activities none|read|update|manage`, `--notifications=none|send|schedule`, `--widgets=none|read|write` and `--emails=none|send`. The last three take their level after `=`; on their own (or with `=true`) they mean the highest level as before (`--notifications` gives send when the default key itself can only send) and `=false` means none; next to another level flag they need a level of their own (`--activities none --notifications=send`). On create, a resource you leave out gets none, except activities, which stay at update unless you pass `--activities` (`--activities none` for a key that only sends notifications). `--activity-slugs` and `--widget-slugs` limit a key to some slugs, and `--expires` takes an RFC 3339 time, unix seconds, or a duration from now.

A new key is shown once and cannot have a permission above the default key's own. It keeps working if the default key is later revoked or rolled. Any other key gets a 403 (`integration_key.default_key_required`) from these commands.

## Notifications

```sh
pushward notify --title "Backup done" --body "412 GB in 38m"
make 2>&1 | tail -20 | pushward notify --title "Build log" --body -
pushward notify --title "Disk" --body "/data at 91%" --level time-sensitive --url https://grafana.example.com
```

Add buttons with `--action`, and `--wait` blocks until one is tapped:

```sh
answer=$(pushward notify --title "Deploy to prod?" --body v2.4.0 \
  --action deploy=Deploy --action skip=Skip --wait 15m --jq .action_id)
[ "$answer" = deploy ] && ./deploy.sh
```

Nobody answering in time exits 7. `pushward notification answer <id> --wait 5m` picks up the same answer later.

## Live Activities

```sh
pushward activity start deploy --name "Deploy api" --template steps --step 1/3 --step-labels Build,Test,Ship --text Building
pushward activity update deploy --step 2/3 --text Testing
pushward activity end deploy --status success
```

`activity end --status success|failure|cancelled` shows a final frame first (green check, red cross or grey stop, with the progress filled on success), holds it for `--display-time` (4s by default) and then ends the card, so the Lock Screen shows the outcome instead of a card that just vanishes. `--text`, `--icon` and `--color` override the preset.

All ten templates work. For the ones without a dedicated flag, set content fields directly:

```sh
pushward activity update ci -F content.progress=0.4 -f content.state="Compiling"
pushward activity update board --data @board.json
```

An approval card waits for a decision the same way a notification does:

```sh
pushward activity start release --template approval --text "Ship 2.4?" --option ship=Ship --option hold=Hold
pushward activity wait release --timeout 30m --jq .content.answer.option
```

## Widgets, schedules, email

```sh
pushward widget create cpu --template gauge --min 0 --max 100 --unit % --value 12
pushward widget update cpu --value 57

pushward schedule create --in 2h --title "Stand up" --body Stretch
pushward schedule create --cron "0 9 * * 1-5" --tz Europe/Warsaw --title Standup --body "In 5 minutes"
pushward schedule list
pushward schedule cancel 1234 --purge

pushward email send --to ops@example.com --subject "Nightly report" --text-file report.txt
```

Email only goes to recipients you have verified in the app.

## Organization keys

With a key from a PushWard organization (a team account), `notify`, `schedule create`, `activity create`, `activity start` and `activity update` take `--target-groups`, `--target-tags` and `--target-members` (comma-separated group names, device tag names or user ids) to narrow who receives it. `activity update --no-target` clears the target again, which needs a key that reaches the whole organization. A personal key gets a 422 for the target flags.

```sh
pushward notify --title "db-1 disk at 95%" --body "Paging on-call" --target-groups oncall
pushward activity update deploy --target-tags wall
```

## Request bodies

Every write command takes the same four layers, later ones winning: `--data` (JSON literal, `@file` or `-` for stdin), the command's own flags, `-f key=value` (always a string) and `-F key=value` (typed: numbers, `true`/`false`/`null`, JSON literals, `@file`). Keys are dotted paths; `key[]` appends to an array and `key[2]` sets an index.

```sh
pushward notify --title T --body B -F push=false -f metadata.host=web-1
pushward activity update rack -F content.template=board \
  -F 'content.tiles[]={"label":"CPU","value":"12","unit":"%"}' -F 'content.tiles[]={"label":"Fans","value":"on"}'
```

## Output

On a terminal you get a short summary or a table. Piped, or with `--json`, you get the API response as JSON; `--jq` filters it and `-q` prints nothing.

```sh
pushward activity list --state ongoing --jq '.items[].slug'
pushward me --jq .live_activity_updates_used
```

Exit codes are stable, so scripts can branch on them:

| Code | Meaning |
|---|---|
| 0 | ok |
| 1 | any other error, such as a 422 validation failure |
| 2 | bad usage |
| 3 | missing, invalid or unauthorized key |
| 4 | not found |
| 5 | rate limited or out of quota |
| 6 | server error or network failure |
| 7 | `--wait` ran out |

Rate limits (429) and 503s are retried for up to a minute, honoring `Retry-After`. Other server errors and network failures are only retried for reads and deletes: retrying a POST could send a notification twice.

## Known limitations

`activity start` makes two calls, a create and then the first update, and both count against your Live Activity quota. The API has no single call that creates an activity with a name and shows it.

Completions: `pushward completion bash|zsh|fish|powershell`. Homebrew installs them for you.

## License

MIT
