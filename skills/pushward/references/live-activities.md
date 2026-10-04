# Live Activity templates

Every template takes `--text` (the status line), `--subtitle`, `--icon` (SF Symbol or
`mdi:<name>`), `--color` and `--compact-label` (up to 4 characters for the Dynamic Island), and
all but approval take `--url`. The rest differs per template. Every `start` below carries
`--ended-ttl` for the reason in SKILL.md: ended activities otherwise count against the
account's 50 for 30 days. Fields without a flag go in with `-F content.<field>=...`
(typed) or `-f content.<field>=...` (string). Updates are merge patches: what you leave out keeps
its stored value, `null` clears it, and arrays are replaced whole.

Full field lists: https://pushward.app/docs/live-activities

## generic: one job with a fraction done

```sh
pushward activity start agent-build --name "Build api" --text Compiling --progress 0 --ended-ttl 30m
pushward activity update agent-build --progress 0.6 --text Linking --eta 2m
```

`--eta` (or `--duration`) gives the card a finish time. Add `-F content.live_progress=true` and
the device animates the bar toward it between your updates, so you can update less often.

## steps: a sequence

```sh
pushward activity start agent-deploy --name Deploy --template steps --step 1/3 --step-labels Build,Test,Ship --ended-ttl 30m
pushward activity update agent-deploy --step 2/3 --text Testing
```

`--step N/TOTAL` sets both numbers; TOTAL goes up to 64. Labels are optional and get cut short
when there are many steps.

## countdown: a timer the server runs

```sh
pushward activity start agent-cooldown --name "Rate limit" --template countdown --duration 25m --ended-ttl 30m \
  -F content.warning_threshold=300 -f content.completion_message="Retrying now"
```

One start is enough: the server sends the warning (accent turns orange `warning_threshold`
seconds before the end) and the completion itself.

## alert: something is wrong

```sh
pushward activity start agent-prod-errors --name "Prod errors" --template alert --severity critical --ended-ttl 1h \
  --text "5xx rate 4.1%" --url https://grafana.example.com/d/api
```

`--severity` is `critical`, `warning` or `info`.

## gauge: a reading inside a range

```sh
pushward activity start agent-disk --name Disk --template gauge --value 71 --min 0 --max 100 --unit % --ended-ttl 30m
pushward activity update agent-disk --value 74
```

The server works out the bar from value, min and max.

## timeline: a value over time

```sh
pushward activity start agent-latency --name "p95 latency" --template timeline --unit ms -F content.value.api=182 --ended-ttl 30m
pushward activity update agent-latency -F content.value.api=240
```

`value` is an object keyed by series name, and each update adds one point; the server keeps the
history and draws the sparkline. Several series: `-F content.value.api=240 -F content.value.db=31`.

## board: up to four tiles

```sh
pushward activity start agent-queue --name Queues --template board --ended-ttl 30m \
  -F 'content.tiles[]={"label":"Pending","value":"128"}' \
  -F 'content.tiles[]={"label":"Failed","value":"3","color":"red"}'
```

Tile values are strings. Tiles are replaced on every update, so send all of them each time.

## log: the newest lines of a feed

```sh
pushward activity start agent-migrate --name "Migration" --template log --ended-ttl 30m \
  -F 'content.lines[]={"text":"copied users (41,203 rows)","level":"info"}'
```

1 to 20 lines, newest first, each with `text`, optional `level` (`info`, `warn`, `error`) and
`at` (unix seconds). The array is replaced on every update: prepend the new line and send the
whole list again, which is easiest from a file: `-F "content.lines=$(cat lines.json)"`
(`-F key=@file` would send the file as a string).

## approval: a question with 2 to 4 buttons

```sh
slug=agent-drop-table-$(date +%s)
pushward activity start "$slug" --name Cleanup --template approval --text "Drop table legacy_sessions?" \
  --option drop=Drop --option keep=Keep -f content.source=Agent --ended-ttl 15m \
  -F 'content.details[]={"label":"Rows","value":"1.2M"}' --eta 20m -f content.on_expire=keep
pushward activity wait "$slug" --timeout 9m --jq .content.answer
```

Use a new slug for every question: started again with the same buttons, an approval keeps its
previous answer. `details` adds up to two label/value rows. `--eta` puts a deadline pill on the
card, and `on_expire` names the option the server records if nobody answers by then (or
`none`); `answer.by` is `expired` in that case and `user` for a real tap, so check it before
acting on the option. Three or more options need an icon each, which `--option` cannot set, so
add them by index: `-f content.options[0].icon=trash -f content.options[1].icon=archivebox`.

## media: a now-playing card

Cover art, a scrubber and transport buttons that call your webhooks. It is for players rather
than agent work; build the body from https://pushward.app/docs/live-activities/media and send
it with `--data @media.json`.
