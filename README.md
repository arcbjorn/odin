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
- **Profiles.** Each agent is a directory (persona, tools, jobs, SQLite state).
  A tool absent from the allowlist is never constructed — it cannot be called.
- **In-process scheduler.** Cron jobs fire on the database's own timezone,
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

`init` creates `profiles/<name>/` with a `config.toml`, `SOUL.md` (the system
prompt), and a SQLite db. Edit `SOUL.md` and `config.toml`, then run.

## Commands

| Command  | What it does                                             |
|----------|---------------------------------------------------------|
| `init`   | Scaffold a profile that loads and runs immediately      |
| `run`    | Start the scheduler and Telegram gateway                |
| `once`   | Run one scheduled job now (`--job NAME`, `--dry-run`)   |
| `ask`    | One turn from the CLI                                    |
| `status` | Print profile, tools, providers, and job schedule       |
| `verify` | Live self-test: call the provider, tools, continuation  |
| `auth`   | Device-code OAuth login for a subscription provider     |
| `usage`  | Remaining plan quota per provider                       |
| `models` | List models a provider exposes                          |

## Configuration

`profiles/<name>/config.toml`. Secrets are never in this file — they are named
by the environment variable that holds them.

```toml
toolsets = ["db", "file"]   # allowlist; absent tools cannot be called
timezone = "UTC"

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
api_key_env = "OPENAI_API_KEY"

# [telegram]
# token_env = "TELEGRAM_TOKEN"
# allowed_users = [123456789]    # required and non-empty; no open gateway
```

**Toolsets:** `db` (SQLite read/write), `file` (scoped notes),
`skills` (markdown procedures), `web` (fetch + optional search). `web` search
plugs into a self-hosted SearXNG when `search_url` is set.

**Subscription auth:** providers can authenticate with a plan instead of a
metered key via device-code OAuth — `xai`, `codex`, `claude`, `minimax`. Set
`subscription = "..."` on the provider block and run `odin auth`. Tokens are
stored `0600` and refreshed automatically; the bot token and refresh tokens
never touch a log.

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
