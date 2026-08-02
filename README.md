![Odin banner](assets/odin-banner.png)

A lean, single-binary agent runtime. Define a persona, give it tools, put it on
a schedule, and reach it over Telegram — running against your own model
providers, including plan/subscription auth, not just metered API keys.

A robust, stable alternative to [OpenClaw](https://github.com/openclaw/openclaw)
and [Hermes](https://github.com/NousResearch/hermes-agent): one static binary
instead of a Node/Python service tree, an explicit tool allowlist instead of an
ever-growing surface, and a scheduler that can't pollute the model's context.

- **One static binary.** `CGO_ENABLED=0 go build` → `scp` → run. No runtime, no
  venv, no dependency tree that resolves differently on the server than it did
  in dev.
- **Provider fallback chain.** Try providers in order; every call restarts from
  the primary, so a recovered primary is used again instead of sticking on a
  fallback.
- **Profiles.** Each agent is a directory (persona, tools, jobs, domain data).
  A tool absent from the allowlist is never constructed — it cannot be called.
- **In-process scheduler.** Cron jobs fire on the profile's runtime timezone,
  independent of host time. Restart Odin after changing it to move every job.
  No `cron` in the DB, no per-job model snapshots to drift out of sync.
- **Guardrails.** A repeated failing tool call is stopped after 3 attempts, not
  looped. Tool schemas are capped small so weaker models can fill them.
- **No context pollution.** The system prompt is assembled once and stays
  byte-identical across turns (so provider caches actually hit); scheduled jobs
  run in isolation and never leak yesterday's state into today's prompt.

## Install

```sh
go install github.com/arcbjorn/odin/cmd/odin@latest
# or from source:
git clone https://github.com/arcbjorn/odin && cd odin && go build ./cmd/odin
```

Cross-compile for a server:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o odin ./cmd/odin
scp odin server:/usr/local/bin/
```

## Quick start

```sh
export OPENAI_API_KEY=sk-...
odin init   --root . --profile default --timezone UTC   # scaffold a profile
odin verify --root . --profile default                  # live provider self-test
odin ask    --root . --profile default "summarize this: <paste>"
```

`init` creates `profiles/<name>/` with a `config.toml`, `SOUL.md`, context,
skills, jobs, migrations, and a SQLite domain database. Edit the committed
profile files, run `odin validate`, then start it.

## Commands

| Command  | What it does                                             |
|----------|---------------------------------------------------------|
| `init`   | Scaffold a profile that loads and runs immediately      |
| `run`    | Start the scheduler and Telegram gateway                |
| `once`   | Run one scheduled job now (`--job NAME`, `--dry-run`)   |
| `ask`    | One turn from the CLI                                    |
| `status` | Print profile, tools, providers, and job schedule       |
| `verify` | Live self-test: call the provider, tools, continuation  |
| `validate` | Offline validation of profile, jobs, skills, and migrations |
| `timezone` | Read or change the machine-local runtime timezone override |
| `auth`   | Device-code OAuth login for a subscription provider     |
| `usage`  | Remaining plan quota per provider                       |
| `models` | List models a provider exposes                          |
| `model`  | Show or switch the model used for chat turns             |
| `backup` | Create a consistent SQLite profile backup               |
| `restore` | Restore the profile database while the agent is stopped |

## Configuration

`profiles/<name>/config.toml`. Secrets are never in this file — it names the
environment variable or the command that yields them.

```toml
toolsets = ["db", "file"]   # allowlist; absent tools cannot be called
timezone = "UTC"
system_files = ["SOUL.md"]   # stable prompt fragments, composed in order

[database]
max_affected_rows = 100       # zero disables the transactional write limit

[agent]
max_turns = 20
max_tokens = 4096
effort = "high"                  # low | medium | high

# Providers are tried in order. The first is primary.
[[providers]]
kind = "openai"                  # openai | anthropic
name = "openai"
model = "gpt-5.6-terra"
base_url = "https://api.openai.com/v1"
api_key_env = "OPENAI_API_KEY"  # or api_key_cmd, never the key itself
# effort = "medium"              # overrides [agent].effort for this provider

# [telegram]
# token_env = "TELEGRAM_TOKEN"
# allowed_users = [123456789]    # required and non-empty; no open gateway
```

**Toolsets:** `db` (SQLite read/write), `file` (scoped notes),
`skills` (markdown procedures), `web` (fetch + optional search), and `shell`
(operator-confined command inspection). `web` search plugs into a self-hosted
SearXNG when `search_url` is set.

**Credentials:** a provider names either `api_key_env` (an environment
variable, injected by systemd from a `0600` `EnvironmentFile`) or
`api_key_cmd` (a command whose stdout is the key). The second exists so the key
can come from the store that already holds it without widening the unit file:

```toml
api_key_cmd = "sops -d --extract '[\"openai\"]' ~/secrets.enc.yaml"
```

It runs once at startup as the agent's own user — the same trust boundary as
the environment variable, not a new one — and its stdout is never logged. A
command that fails stops the start and reports its stderr, because a broken
secret store must be visible at boot rather than at 07:00. The key itself never
belongs in `config.toml`: `api_key`, `token`, `secret`, and an `echo`-ed
`api_key_cmd` are all rejected at load.

**Reasoning effort:** `[agent].effort` is the profile default; `effort` on a
provider overrides it, because one level rarely fits a chain of different model
families. If a model answers the hint with HTTP 400, the transport drops it and
retries once, then stops sending it — without that, a `/model` switch onto a
non-reasoning model would fail every turn, since the chain treats 400 as a
malformed request of our own making and aborts rather than falling back.

**Subscription auth:** providers can authenticate with a plan instead of a
metered key via device-code OAuth — `xai`, `codex`, `claude`, `minimax`. Set
`subscription = "..."` on the provider block and run `odin auth`. Tokens are
stored `0600` and refreshed automatically; the bot token and refresh tokens
never touch a log.

### Switching models

`config.toml` sets the chain. `/model` changes what the *conversation* runs on,
without an edit, a restart, or losing the thread:

```text
/model                       what runs now, its fallbacks, and how to change it
/model list                  live catalogs, per provider
/model gpt-5.5               a bare model — its provider is detected
/model backup                a provider — at its configured model
/model backup/glm-4.9        both, explicitly
/model once backup/glm-4.9   this process only; nothing is stored
/model verify                live protocol check on what is running
/model verify backup/glm-4.9 check it, and switch only if it passes
/model reset                 back to config.toml
```

`verify` runs the same catalog, tool-call, and continuation checks as
`odin verify`, against the target alone rather than the chain — a fallback
answering for it would report a working model that does not work. Pairing it
with a switch is the point: a model that cannot hold up the tool exchange is
found here rather than halfway through a real turn. The two modifiers compose,
so `/model once verify NAME` checks a model, uses it, and stores nothing.

The same thing from the CLI, for a profile that has no gateway:

```sh
odin model --profile default                        # show
odin model --profile default set backup/glm-4.9     # switch
odin model --profile default once backup/glm-4.9    # this process only
odin model --profile default verify backup/glm-4.9  # check, then switch
odin model --profile default reset                  # restore
```

Three properties are worth knowing, because they are choices rather than
accidents:

- **Scheduled jobs never move.** A switch applies to chat turns only; jobs keep
  running the chain in `config.toml`. An exploratory switch at 23:00 becoming
  the model that runs the 07:00 job is the drift a config-driven scheduler
  exists to prevent, and `odin status` prints an active override so it is
  never a surprise.
- **The fallback chain survives.** The selected provider is promoted to
  primary; the others stay behind it at their own models. Switching a model
  does not cost the resilience the chain is for.
- **A `once` switch stores nothing.** `/model` persists by default, because it
  is aimed at a daemon rather than a shell session and an override that
  silently reverts on restart is its own kind of drift. `once` is the opt-out
  for a genuinely exploratory switch — which is the case Hermes makes its
  default, for the same reason.
- **It only selects what is already configured.** `/model` cannot add a
  provider, run an OAuth flow, or take an API key. Those stay in `config.toml`
  and `odin auth`, reviewable in git rather than typed into a chat window.
  (Hermes splits this the same way — its in-session `/model` switches, its
  `hermes model` wizard configures. Odin has no wizard: the config is a file.)

The choice is written to `state/runtime.json` and re-applied at startup — no
catalog call, so a provider that is merely unreachable at boot does not
silently discard it. An override naming a provider that has since left
`config.toml` is dropped with a warning rather than failing the start.

### Profile structure

```text
profiles/<name>/
├── config.toml
├── SOUL.md
├── context/       optional static system-prompt fragments
├── skills/        on-demand procedures
├── jobs/          schedules and isolated job prompts
├── migrations/    immutable <version>-<name>.sql files
├── notes/         model-scoped files
├── state/         runtime overrides, scheduler state, undelivered messages
├── auth/          OAuth credentials
└── db.sqlite      profile-owned domain data
```

`SOUL.md`, context, skills, jobs, and migrations are configuration and belong
in version control. Notes, state, credentials, backups, and the live database
are machine-local. Odin applies pending migrations transactionally at startup
and refuses to run if an applied migration's checksum changes.

`timezone` in `config.toml` is the committed default. Travel changes use
`odin timezone --profile NAME set Area/City`; `reset` returns to the committed
default. Restart the running agent after a change so its schedule is rebuilt.

### Job delivery

Nobody is waiting on a 07:00 brief to notice it never arrived. So a scheduled
job whose Telegram delivery fails does two things: it records the run as
**failed**, and it keeps the text.

Failing loudly is the important half. Delivery used to be fire-and-forget, so
an outage meant the tokens were spent, the output was gone, `odin status` said
`last run ok`, and the watchdog — the one component whose job is catching
silent job failure — had nothing to alert on.

Keeping the text is the cheap half. Undelivered output goes to
`state/outbox.json` and is retried on the gateway's next poll, so a blip costs
a late message rather than the model's work. Only the chunks that never landed
are re-sent, entries survive a restart, and anything delivered more than a few
minutes late is labelled with how late it is — a brief that arrives hours after
its window must not read as if it were written just now. The queue is bounded
by size, attempts, and age; past a day, delivering is the wrong outcome and the
entry is dropped with an error in the log. Interactive replies are never
queued: you are there, and you will just ask again.

## Deploy

`odin run` is a long-lived process — put it under any supervisor. It owns its
own schedule internally, so the supervisor only needs to keep it alive.
`deploy/` holds ready systemd templates (one instance per profile):

```sh
cp odin odin-watchdog /usr/local/bin/
cp deploy/*.service deploy/*.timer /etc/systemd/system/
systemctl enable --now odin@default            # the agent
systemctl enable --now odin-watchdog@default.timer   # optional, see below
```

### Watchdog

The agent's scheduler runs *inside* `odin run`, so it can't report that the
process itself has died. `odin-watchdog` is a separate one-shot binary,
triggered from outside, that reads the scheduler's state file and alerts over
Telegram when a job is silently overdue or failing. Because a scheduler cannot
announce its own crash, the trigger must live outside the agent — a systemd
timer or a cron line:

```cron
# every 30 min: check the agent is running its jobs, alert if not
*/30 * * * * TELEGRAM_TOKEN=... TELEGRAM_CHAT_ID=... \
  odin-watchdog --profile-dir /var/lib/odin/profiles/default
```

Healthy is silent — it only speaks when something is wrong.

## Prior art

Odin is inspired by [OpenClaw](https://github.com/openclaw/openclaw) and
[Hermes](https://github.com/NousResearch/hermes-agent), and owes both a debt —
the config-driven `SOUL.md` persona, the multi-platform gateway idea, and the
skills-as-markdown concept all came from that lineage.

It's a deliberate reaction to running them at scale. Both are capable but
sprawling — large, always-on services (Node and Python respectively) supporting
dozens of platforms and providers, where deployment friction and silent failures
accumulate: a scheduler whose per-job state drifts out of sync with the global
config, a growing tool surface where a capability is disabled-by-config rather
than absent, credential and packaging setup that resolves differently on the
server than in dev. Odin keeps the good ideas and drops the rest: one static
binary, an explicit allowlist, jobs as files, a scheduler that owns its own
clock, and a deliberately dumb external watchdog for the one thing an in-process
scheduler can't catch — its own death.

If you want a batteries-included, many-platform agent, use those. If you want a
small one you can read end to end and deploy with `scp`, use this.

## License

MIT — see [LICENSE](LICENSE).
