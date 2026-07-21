# odin

A lean, single-binary personal agent. Runs scheduled jobs and a Telegram chat
gateway against your own model providers — including plan/subscription auth,
not just metered API keys.

- **One static binary.** `CGO_ENABLED=0 go build` → `scp` → run. No runtime, no
  venv, no dependencies on the host.
- **Provider fallback chain.** Try providers in order; every call restarts from
  the primary, so a recovered primary is used again instead of sticking on a
  fallback.
- **Profiles.** Each agent is a directory (persona, tools, jobs, SQLite state).
  A tool absent from the allowlist is never constructed — it cannot be called.
- **In-process scheduler.** Cron jobs fire on the tracker's own timezone
  (switchable live), so a timezone change moves every job. No `cron` in the DB.
- **Guardrails.** A repeated failing tool call is stopped after 3 attempts, not
  looped. Tool schemas are capped small so weaker models can fill them.

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
prompt), and a SQLite tracker. Edit `SOUL.md` and `config.toml`, then run.

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
toolsets = ["tracker", "file"]   # allowlist; absent tools cannot be called
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

**Toolsets:** `tracker` (SQLite read/write), `file` (scoped notes),
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

## License

MIT — see [LICENSE](LICENSE).
