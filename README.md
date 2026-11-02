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

`pushward notification answers` reads the answers to every notification the key sent, oldest first, after a cursor: one script can handle many questions that way. Without `--after` it only returns the current cursor. A reader that already holds a cursor keeps reading from it. A reader with no cursor yet takes one when it is set up, before it sends anything it waits on (`pushward notification answers --jq .cursor`), or else takes the `answer_cursor` of its first answerable send and keeps it; the `answer_cursor` of later sends adds nothing. Cursors are opaque and never compared. Only a send with a button to answer has an `answer_cursor`, so for any other `notify --jq .answer_cursor` prints `null`.

Other senders on the same key show up in the stream too, so keep the `id` of each send and skip answers to the rest. `--wait 10m` blocks until an answer comes in (exit 7 if none does). `--follow` keeps printing them, one JSON page per line when piped, until you stop it, and then prints the cursor to resume from on stderr. It waits out network errors, 5xx responses and rate limits, 2s at first and doubling up to 30s, with a line on stderr for each retry (none with `-q`). A key or cursor the server refuses, or a used-up quota, ends it with the usual exit code.

```sh
pushward notification answers --follow --jq '.items[] | "\(.notification_id) \(.action_id)"'
```

`--snooze 15m,1h` adds snooze actions: the device hides the notification and brings it back after that long, and nothing reaches the server. `pushward notification retract <collapse-id>` takes back what was sent with that `--collapse-id` once it no longer applies. The devices remove it and `--ack` repeats stop, but it is best effort: an app that was force-quit keeps it until it runs again. iPhone, iPad and Mac need PushWard 1.20 or later; Apple Watch is not sent to.

```sh
pushward notify --title "Garage door open" --body "Since 21:40" --collapse-id garage-door --snooze 15m,1h
pushward notification retract garage-door
```

## Repeat until acknowledged

With `--ack`, `notify` and `schedule create` send the notification again and again until someone taps Acknowledge on one of the devices, or until it expires. It repeats every minute for an hour unless `--ack-repeat` (30s to 1h) and `--ack-expire` (1m to 3h) say otherwise, and the repeats do not count against your quota. Any other button without a url acknowledges it too.

```sh
pushward notify --title "db-1 down" --body "Primary unreachable" --level time-sensitive --ack --ack-repeat 2m --tag db-1
pushward receipt 42 --wait 10m      # acknowledged, or exit 7 once it expires
pushward receipt cancel --tag db-1  # db-1 is back: stop repeating
```

`notify --ack --wait` blocks until it is acknowledged. `pushward receipt get <id>` (or just `pushward receipt <id>`) shows who acknowledged it and when, and `receipt cancel <id>` stops one. `receipt cancel --tag` stops only what the same integration key sent. An account can have 25 repeating at once, with an organization's sends counting against the organization, and a new one sent with the same key and `--collapse-id` replaces the old one's repeats.

`--callback-url https://...` gets a POST when it is acknowledged or expires, signed like [Standard Webhooks](https://www.standardwebhooks.com/). `pushward receipt secret` prints the `whsec_` secret to check the signature with. It is derived from the integration key, nothing is stored, and rolling the key gives a new one. The app's own API tokens (`hla_`) cannot set a callback.

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

The timer template counts down to `--eta` or `--duration`, or with `--direction up` it shows how long something has been going. The server runs the clock, so it needs no updates in between:

```sh
pushward activity start soak --name "Soak test" --template timer --direction up --goal 2h --text Running
pushward activity update soak --paused true
```

`--goal` turns the card orange once it is passed, `--paused false` resumes, and `--show-ruler false` drops the ruler. Counting up and pausing need the iOS app 1.19.0 or later. `countdown`, the template's old name, still works and is what the API reports back.

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

## Coding agents (Claude Code and Codex)

`pushward agent` runs as a Claude Code or Codex hook, so a permission prompt or a question from the agent reaches your phone while you are away from the terminal, and your answer goes back to the session.

```sh
pushward agent install claude    # hooks in ~/.claude/settings.json
pushward agent install codex     # hooks in ~/.codex/hooks.json
pushward agent status
pushward agent test              # three sample questions on one card
```

Install merges its entries next to the hooks already in the file and keeps every other key and hook, and copies the old file to `<file>.bak` first. It writes the file back with one value per line in the file's own indent, so an array or object you kept on one line is spread out (the content stays the same). Running it again changes nothing, and an entry you run through another command (`op run`, say) is left as you wrote it, timeout included; `--dry-run` prints the result instead of writing it. Codex skips a new hook until you trust it, so run `/hooks` in Codex once after installing. `pushward agent uninstall claude` (or `codex`) takes out only what install added.

A permission prompt (a shell command, a file edit, an MCP tool) shows up as an approval Live Activity with Allow and Deny. Claude Code also gets Always when it has a rule to remember, the same one its own dialog would save; Codex has no such option, because its hooks cannot set one. When Claude asks you questions, one card steps through them (1/3, 2/3, 3/3) and the answers go back as if you had picked them in the terminal. A question that takes several answers stays in the terminal and you get a notification saying so. When a turn finishes you get a plain notification, and the next one from that session replaces it; Claude Code's background sessions (agent view) also send one when they start waiting on you. Claude's own idle reminder a minute after a turn is not passed on, so a finished turn pushes once. An install from an earlier version still sends that reminder, which the hook ignores; installing again updates the matcher.

On a Mac the hook first checks how long the keyboard and mouse have been idle. Used in the last 90 seconds (`--desk-idle`), and the prompt stays in the terminal with nothing sent. While a prompt waits on the phone, touching the Mac ends the card and hands the prompt back to the terminal. Linux has no idle check, so every prompt goes to the phone. `--desk away` or `--desk desk` skips the check.

If no device reports starting the card within 15 seconds, the same question also comes as a time-sensitive notification with buttons. iOS silently drops a Live Activity start once a phone has used up its budget for them, and an account with no Apple device has nothing to start it on. For Claude's questions the notification has an Other field for a typed answer. Whichever you answer first wins and the other one is withdrawn. `--via notification` skips the card, `--via card` never falls back.

With no answer after `--max-wait` (25 minutes) the prompt goes back to the terminal, or is denied with `--on-timeout deny`. Every hook flag can also be set in the environment as `PUSHWARD_AGENT_<FLAG>`, for example `PUSHWARD_AGENT_ON_TIMEOUT=deny`, which leaves the command Codex trusted unchanged. To wait longer, reinstall with `pushward agent install claude --max-wait 40m`: the hook's timeout in the settings file has to grow with it. Each card is a create plus one update per question, and both count against your Live Activity quota. A card is deleted a minute after it ends, so prompts do not fill up the account's activities.

What leaves your machine: the command, file path or question is sent to PushWard as the card's text, like any other Live Activity, and activities are not end-to-end encrypted. The finished notice carries the first 200 characters of the agent's last message, and the notice for a question that takes several answers carries that question's text. With an encryption key set, the title and text of the fallback notifications and the notices are encrypted, and one PushWard refuses sealed is not sent again readable. Their buttons are not: the choices of Claude's questions go out as button labels, and PushWard stores those readable either way. The hook logs to stderr (or `--log <file>`) and only writes slugs, ids and statuses there, never a command, typed text, an answer URL or your key. For Codex, install only writes `hooks.json`; hooks you keep in Codex's `config.toml` are left alone, and `agent status` does not see them.

Use a personal key. A tap on Allow from any device the card reaches runs the command, so the hooks check the key before sending anything and send nothing with an organization's key, whose cards go to everyone the organization's routing reaches (`agent status` says when that is the case). When PushWard refuses the key both a card and a notification before the check can tell which kind it is, nothing is sent either. Shares work the same way: someone you share activities with by a pattern that matches `agent-` (`a*`, say) sees your prompts and can answer them.

## Organization keys

With a key from a PushWard organization (a team account), `notify`, `schedule create`, `activity create`, `activity start`, `activity update`, `widget create` and `widget update` take `--target-groups`, `--target-tags` and `--target-members` (comma-separated group names, device tag names or user ids) to narrow who receives it. `activity update --no-target` and `widget update --no-target` clear the target again, which needs a key that reaches the whole organization. A personal key gets a 422 for the target flags.

```sh
pushward notify --title "db-1 disk at 95%" --body "Paging on-call" --target-groups oncall
pushward activity update deploy --target-tags wall
pushward widget create queue --template value --value 42 --label jobs --target-groups oncall
```

An organization widget shows up on members' home screens, and on Apple Watch (watch app and complications) from PushWard 1.18.0. Its pushes share one window per organization of at least 60 seconds, so `--push-throttle` does nothing there; updates in between are kept and arrive with the next push.

Organization email goes only to the organization's own recipients, not to the members' personal ones. They are registered through the API with an organization key that has email permission and is not limited to some groups and tags, and each one confirms the address before the first message ([pushward.app/docs/teams](https://pushward.app/docs/teams)). The CLI has no command for that yet; `pushward api` can call it.

## End-to-end encryption

With an encryption key, `notify`, `notification send` and `schedule create` encrypt the title, subtitle, body and url on your machine (AES-256-GCM, envelope format `pw1`). PushWard stores and forwards only the sealed text, and only devices holding the key can read it. Level, actions, metadata and the rest of what the server needs to deliver it stay readable. Devices need PushWard 1.17.0 or later; older builds show a placeholder.

```sh
pushward e2e generate --save     # prints the key and its Key ID; import the key in the app (Settings > End-to-End Encryption)
pbpaste | pushward e2e import    # or store a key created in the app
pushward e2e key-id              # compare with the Key ID the app shows
pushward notify --title "Prod DB password rotated" --body "New one is in the vault under db/prod"
```

The key comes from `PUSHWARD_E2E_KEY`, then the config file. Once one is set, every notification is encrypted: `--no-encrypt` sends one without it, and `--encrypt` fails instead of sending in the clear when no key is set. `pushward api` never encrypts. `pushward e2e encrypt` and `pushward e2e decrypt` seal and open envelopes for other tools, and [internal/e2e/testdata/vectors-v1.json](internal/e2e/testdata/vectors-v1.json) holds the test vectors for anyone writing their own.

The sealed text has to fit a 3072-character envelope, about 2.2 KB. A longer body is refused before anything is sent; shorten it, or send it with `--no-encrypt`.

An organization key encrypts the same way, with the organization's own key in place of a personal one. An admin creates it (`pushward e2e generate`) and hands it to the members, who add it in PushWard 1.18 or later under Settings > End-to-End Encryption > Organization Keys; 1.17 shows those notifications as locked. The PushWard console never generates or stores it. If a send with an organization key fails with 422 `notification.encryption_unavailable`, PushWard has not enabled encryption for organizations: use `--no-encrypt` or leave the key out of that sender for now. Setup and one key per group: [pushward.app/docs/notifications/encryption](https://pushward.app/docs/notifications/encryption#organizations).

In the GitHub Action, pass the key from a secret as the `e2e-key` input and set `encrypt: true` next to it. A secret that does not exist, or is not passed to a pull request from a fork, comes through empty, and without `encrypt` the step would then send the text readable. With `encrypt: true`, a command that cannot encrypt fails the step too, and with a key set `api` refuses `/notifications` paths. GitHub also prints a step's inputs at the top of its log, so the `title` and `body` inputs are readable there even though the request is encrypted. For text that must stay private, write it to a file in an earlier step and pass `json: '@body.json'`.

```yaml
- uses: mac-lucky/pushward-action@v1
  with:
    token: ${{ secrets.PUSHWARD_TOKEN }}
    e2e-key: ${{ secrets.PUSHWARD_E2E_KEY }}
    encrypt: true
    json: '@body.json'
```

Upgrade note: CLI 1.3.x and earlier keep only the API URL and the integration key when `auth login` or `auth logout` rewrites the config file, so running either with an older binary (another install, an old image) deletes a stored encryption key. `pushward e2e key-id` tells you whether it is still there; `pushward e2e import` it again if not.

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
| 7 | `--wait` ran out, or the notification expired or was canceled before anyone acknowledged it |

Rate limits (429) and 503s are retried for up to a minute, honoring `Retry-After`. Other server errors and network failures are only retried for reads and deletes: retrying a POST could send a notification twice.

## Known limitations

`activity start` makes two calls, a create and then the first update, and both count against your Live Activity quota. The API has no single call that creates an activity with a name and shows it.

Completions: `pushward completion bash|zsh|fish|powershell`. Homebrew installs them for you.

## License

MIT
