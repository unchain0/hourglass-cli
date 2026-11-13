# Hourglass Rejections RPA

Lightweight Go automation that checks Hourglass for rejected assignments, stores operational history in SQLite or PostgreSQL, and delivers notifications through a Telegram bot with per-user preferences.

## What it does

- reads Hourglass through the official API
- sends Telegram notifications only when there are new changes
- lets each user choose which sections they want to follow
- persists preferences and rejection history in the configured database
- supports static tokens or automatic token renewal with WebAuthn

This repository is a good fit if you want a self-hosted monitor for Hourglass rejections with a bot interface. It is a bad fit if you need a generic browser scraper or a multi-tenant SaaS service.

## Quickstart

### Option 1: bootstrap automatic renewal from token and cookie

1. Copy the environment template.

   ```bash
   cp .env.example .env
   ```

2. Fill at least these variables in `.env`:

   ```env
   TELEGRAM_BOT_TOKEN=your_bot_token
   TELEGRAM_WHITELIST=123456789
   HOURGLASS_XSRF_TOKEN=your_xsrf_token
   HOURGLASS_HGLOGIN_COOKIE=your_hglogin_cookie
   ```

3. Start the app.

   ```bash
   make run
   ```

With `AUTO_REFRESH_TOKENS=true` (the default), a valid token/cookie pair is used only to register a WebAuthn credential on the first start. The daemon then obtains fresh cookies without Chrome and stores the credential and renewed tokens under `~/.hourglass-rpa/`. Success means the logs contain `registered WebAuthn credential` followed by `WebAuthn authentication succeeded`, and the bot is ready to accept `/start`.

Keep that directory on persistent storage. If the directory is discarded on every deploy, Hourglass receives a new passkey on every start.

### Option 2: bootstrap from a visible browser

If this will run on a VPS or long-lived container, generate persisted auth files first:

```bash
make setup-auth
```

That command opens a normal Chrome window so you can sign in with Google once, then it creates `auth-tokens.json` and `webauthn-credentials.json` under `~/.hourglass-rpa/`. After that first bootstrap, the daemon renews access automatically in headless mode by preferring the stored WebAuthn credential and retrying once if Hourglass invalidates the session early. `CHROME_PROFILE_DIR` remains an optional fallback when you want to reuse the persisted browser profile as well.

Use this alternative when the token/cookie pair is already expired or unavailable. For VPS use, copy at least these files to the server and keep them persistent across restarts:

- `~/.hourglass-rpa/auth-tokens.json`
- `~/.hourglass-rpa/webauthn-credentials.json`
- optional fallback: `~/.hourglass-rpa/chrome-profile/`

Then point `TOKENS_PATH`, `WEBAUTHN_CREDENTIALS_PATH`, and optionally `CHROME_PROFILE_DIR` at those files before starting the daemon.

## Requirements

- Go 1.26 for local development
- Telegram bot token from [@BotFather](https://t.me/BotFather)
- Hourglass authentication, either:
  - one valid `HOURGLASS_XSRF_TOKEN` + `HOURGLASS_HGLOGIN_COOKIE` pair for automatic first-start bootstrap, or
  - persisted token files plus WebAuthn credentials from the Google bootstrap flow
- Chromium/Chrome available when you use the auth setup tools

## Main workflows

### Run the daemon

```bash
make run
```

The main binary starts both the Telegram bot and the scheduler. The scheduler checks more frequently during the day and less frequently overnight.

### Start with Docker Compose

```bash
# On a workstation with a visible Chrome/Chromium session:
make setup-auth
make copy-to-vps VPS=user@your-seedbox

# On the seedbox, import both files into the persistent Docker volume:
AUTH_SOURCE_DIR="$HOME/.hourglass-rpa" docker compose --profile bootstrap run --rm auth-bootstrap

# Copy .env.example to .env and set a strong, persistent POSTGRES_PASSWORD,
# along with the required Telegram and Hourglass credentials. Then rebuild
# from the current runtime base and start the services.
export APP_VERSION="$(git describe --always --dirty)"
export GIT_COMMIT="$(git rev-parse --short=12 HEAD)"
docker compose build --pull --no-cache rpa
docker compose up -d rpa
```

The default compose file starts the main `rpa` service with a persisted authentication volume and requires `POSTGRES_PASSWORD` in `.env`; reuse the same value while the PostgreSQL volume exists. When valid Hourglass token/cookie variables are present, a fresh deployment creates its WebAuthn credential directly from that session; importing files is optional. Leave `CHROME_PROFILE_DIR` unset unless you also imported a previously authenticated browser profile.

For an existing `postgres_data` volume whose database still uses a previous password, set both the new `POSTGRES_PASSWORD` and the current `POSTGRES_OLD_PASSWORD` in `.env` before starting `rpa`. The `db-password-migrate` service updates the `hourglass` role in place and verifies the new password before `rpa` starts. After it completes successfully, remove `POSTGRES_OLD_PASSWORD` from `.env`. Do not delete the volume; no data reset is required.

The main service already renews tokens automatically. The optional `autorefresh` profile should only be used when `AUTO_REFRESH_TOKENS=false`, so two processes do not renew the same files concurrently:

```bash
AUTO_REFRESH_TOKENS=false docker compose --profile autorefresh up -d
```

### Prepare authentication for servers

```bash
make setup-auth
make copy-to-vps VPS=user@your-vps.com
```

Useful helper binaries are also available through the Makefile:

- `make save-tokens` for manual token capture
- `make token-refresh` for a standalone refresh attempt

### Deploy with Coolify

Configure the required Telegram and Hourglass variables in Coolify, keep `AUTO_REFRESH_TOKENS=true`, and add persistent storage mounted at:

```text
/home/rpa/.hourglass-rpa
```

Do not set `CHROME_PROFILE_DIR` for the normal passkey flow. The first deployment consumes the valid Hourglass token/cookie pair to register the RPA credential; subsequent deployments reuse `webauthn-credentials.json` and `auth-tokens.json` from the volume. After the first successful start, the original environment token and cookie may expire without interrupting renewal.

The container health check reports whether the `rpa` daemon is alive. Authentication readiness is intentionally separate: token and cookie may come from Coolify variables before the persistent token file is created, while renewal failures remain visible in the application logs.

The Docker build is tuned for small VPS instances by limiting Go compiler concurrency, caching modules and compilation, and waiting for the binaries before installing Chromium. Reserve at least 512 MiB of available memory for deployments. A build that stops at `go build` with exit 255 or `compile: signal: killed` indicates host memory pressure; use a Coolify build server or temporarily stop competing workloads if less memory is available.

Keep exactly one running `rpa` instance for each Telegram bot token. Telegram long polling permits only one `getUpdates` consumer, so a second Coolify replica or a local daemon using the production token will cause HTTP 409 conflicts. Use the automated test suite or a separate development bot token for local validation, and keep the Coolify replica count at one.

## Configuration

The full variable list lives in `.env.example`. In practice, these are the keys most people need first:

```env
TELEGRAM_BOT_TOKEN=your_bot_token
TELEGRAM_WHITELIST=123456789
HOURGLASS_XSRF_TOKEN=your_xsrf_token
HOURGLASS_HGLOGIN_COOKIE=your_hglogin_cookie

# Files created automatically from the initial token/cookie pair.
# They must live on persistent storage in a VPS/container.
TOKENS_PATH=/home/user/.hourglass-rpa/auth-tokens.json
WEBAUTHN_CREDENTIALS_PATH=/home/user/.hourglass-rpa/webauthn-credentials.json
# CHROME_PROFILE_DIR is only an optional browser-profile fallback.

DB_TYPE=postgres
DATABASE_URL=postgres://user:password@host:5432/postgres

OTEL_EXPORTER_OTLP_ENDPOINT=https://otel.example.com
OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer token,stream-name=default
OTEL_SERVICE_NAME=hourglass-rejections-rpa
OTEL_SERVICE_VERSION=1.0.0
LOG_LEVEL=info
TZ=America/Sao_Paulo
```

`TELEGRAM_WHITELIST` contains only the administrator's Telegram Chat ID. The
administrator is the only user configured through the environment and is always
authorized. From Telegram, use `/allow <chat_id>` to authorize a non-admin user,
`/deny <chat_id>` to revoke access, and `/users` to list authorized users. These
permissions are persisted in the configured database and survive restarts.

Important notes:

- `AUTO_REFRESH_TOKENS` defaults to `true`.
- when Hourglass returns `401`, the client forces one fresh renewal before failing the run.
- database schema is created automatically on startup when missing
- OpenTelemetry export is optional but recommended for logs, traces, and metrics

## Telegram bot commands

The bot currently registers these commands:

| Command | Purpose |
| --- | --- |
| `/start` | Show the welcome message |
| `/configure` | Choose which sections each user follows |
| `/status` | Show current preferences |
| `/stats` | Show bot statistics |
| `/whoami` | Show account details |
| `/language` | Change the bot language |
| `/help` | List available commands |
| `/checknow` | Trigger an immediate check |
| `/allow <chat_id>` | Authorize a user (administrator only) |
| `/deny <chat_id>` | Revoke a user's access (administrator only) |
| `/users` | List authorized users (administrator only) |

## Development

Common local commands:

```bash
make fmt
make fmt-check
make tidy
make vet
make lint
make test
make vulncheck
make build
```

`make test` runs the race-enabled suite and enforces 100% statement coverage.
`make ci` runs the complete CI command sequence. Before every push, Lefthook
also runs the full test, build, lint, and vulnerability gates locally; a
failing gate prevents the push from proceeding.

After the daemon is running, use `/checknow` in Telegram for an immediate manual check.

This repository builds five binaries from `cmd/`:

- `rpa`
- `hourglass` (read-focused CLI; protected assignee swap)
- `save-tokens`
- `token-refresh`
- `setup-auth`

### Hourglass CLI

Build with `make build` (binary `hourglass`). Read commands emit JSON with
resolved names alongside IDs. Mutating HTTP methods and send/publish/notify
paths stay blocked unless `HOURGLASS_ALLOW_SEND=1`. Keep that flag unset for
normal use.

Read examples:

```bash
hourglass auth status
hourglass users --json
hourglass schedule mm --from 2026-03-01 --to 2026-03-31 --json
hourglass schedule av --from 2026-03-01 --to 2026-03-31 --json
hourglass schedule wm --from 2026-03-01 --to 2026-03-31 --json
hourglass schedule notifications --from 2026-03-01 --to 2026-03-31 --json
hourglass schedule privileges --json
hourglass schedule check-rules --json
```

### Copiar a programação de um mês para outro

```bash
hourglass schedule cart copy --from 2026-11 --to 2026-12
```

Cada designação de novembro é replicada em dezembro mantendo o dia da semana e o horário: a 2ª quinta-feira das 08:00 continua sendo a 2ª quinta-feira das 08:00. O comando devolve `entries` com `LocationID`, `Datetime`, `DurationHours` e `PublisherID`, e lista em `problems` os casos que exigem decisão: mês de destino sem a 5ª semana correspondente, horário já ocupado no local e publicador em dois locais ao mesmo tempo.

A gravação usa `PUT /scheduling/pw/schedule?locID=<id>` com uma designação por chamada e só acontece com `HOURGLASS_ALLOW_CART=1`:

```bash
HOURGLASS_ALLOW_CART=1 hourglass schedule cart copy --from 2026-11 --to 2026-12 --apply
```

Sem essa variável, todo PUT continua bloqueado. O comando grava na ordem do plano, para no primeiro erro e relê o mês para confirmar quantas designações ficaram. Ele não chama nenhum endpoint de envio, então nenhuma notificação é disparada.

### Programação de carrinho por planilha

O modelo fica em `docs/carrinho-modelo.csv`, com as colunas `local,data,inicio,fim,publicador1,publicador2`.
Datas aceitam `dd/mm/aaaa` ou `aaaa-mm-dd`; horários usam `hh:mm`; `publicador2` é opcional.

```bash
hourglass schedule cart import --file docs/carrinho-modelo.csv
```

O comando lê os locais, os publicadores e a programação existente direto do Hourglass e devolve um JSON com:

- `entries`: uma linha por publicador, com `LocationID`, `Datetime` em UTC, `DurationHours` e `PublisherID` já resolvidos;
- `problems`: local inexistente ou desativado, nome ambíguo, data ou horário inválido, linha repetida, horário já ocupado no local e publicador escalado em dois lugares ao mesmo tempo.

Qualquer problema bloqueia a importação, e nada é gravado. A gravação em si ainda não está habilitada: o endpoint de escrita da programação de testemunho público não está confirmado, e o comando responde com erro explícito quando recebe `--apply`.

Protected assignee swap (AV slot only; never publish/notify/send):

```bash
# 1) Preview before/after and capture the confirm token from JSON.
hourglass assign swap --slot 12345 --to 67890 --from 2026-03-01 --until 2026-03-31 --dry-run

# 2) Apply only with a separate enable flag and the matching confirm token.
# HOURGLASS_ALLOW_SEND must stay unset. Token is bound to slot, current
# assignee, target assignee, and slot updated_at/date (stale previews fail).
HOURGLASS_ALLOW_ASSIGN=1 hourglass assign swap --slot 12345 --to 67890 \
  --from 2026-03-01 --until 2026-03-31 --confirm '<token-from-dry-run>'
```

`--to` accepts a user id or an exact display name. Omit `--from`/`--until` to
search a default window (30 days back through one year ahead). Without
`HOURGLASS_ALLOW_ASSIGN=1` and `--confirm`, the command stays dry-run only.

## Project layout

```text
cmd/                    entrypoints for the daemon and auth utilities
src/domain_models/      shared contracts and result types
src/engines/rejection_cache/ in-memory duplicate suppression
src/integrations/auth/webauthn/ browser-assisted auth and token renewal
src/integrations/database/ preference and operational-history storage
src/integrations/i18n/ localized bot messages
src/services/hourglass/ Hourglass API client and analysis logic
src/services/bot/       Telegram bot lifecycle and manual checks
src/services/notification/ Telegram/email delivery handlers
src/services/scheduler/ periodic analysis and notification dispatch
```

## Additional project docs

- `AGENTS.md` for a high-level maintainer map
- `src/AGENTS.md` for shared source-tree conventions
- `src/services/hourglass/AGENTS.md` for API client details
- `src/integrations/auth/webauthn/AGENTS.md` for auth and token renewal internals
- `src/services/notification/AGENTS.md` for Telegram notifier details

## License

MIT. See [LICENSE](LICENSE).
