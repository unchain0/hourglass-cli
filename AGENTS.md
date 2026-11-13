# Repository guide

Go 1.26 automation that reads Hourglass assignment rejections, persists user and execution state, and serves scheduled plus on-demand Telegram notifications. Four binaries share WebAuthn/Chrome token management, GORM persistence, i18n, and OpenTelemetry integration.

## Structure

```text
.
├── cmd/                              # Daemon and three authentication utilities
├── src/domain_models/                # Shared contracts, section names, result types
├── src/engines/rejection_cache/      # In-memory duplicate suppression
├── src/integrations/
│   ├── auth/webauthn/                # Native WebAuthn, Chrome profile, token persistence
│   ├── database/preferences/         # SQLite/Postgres state and audit records
│   ├── i18n/locales/                 # Embedded en, pt-BR, es, fr TOML messages
│   └── monitoring/telemetry/         # OTLP traces, metrics, logs
└── src/services/                     # Hourglass, scheduler, bot, notification
```

There is no `internal/` tree. Imports use `hourglass-rejections-rpa/src/...`; older README examples may still show pre-migration paths.

## Where to look

| Task | Location | Notes |
|------|----------|-------|
| Daemon startup and run modes | `cmd/rpa/main.go` | `-once` currently delegates to an injected stub that returns “not implemented” |
| Dependency wiring | `cmd/rpa/bootstrap.go` | Selects Postgres from `DATABASE_URL`, otherwise preferences defaults |
| Authentication utilities | `cmd/{setup-auth,save-tokens,token-refresh}/main.go` | Interactive setup/extraction versus unattended renewal |
| Shared domain contracts | `src/domain_models/` | `AllSections` is the canonical section list |
| Hourglass HTTP and analysis | `src/services/hourglass/` | Cookies/XSRF, WebAuthn lifecycle, API-to-domain mapping |
| Scheduled execution | `src/services/scheduler/` | Business-hour policy, concurrent section analysis, cache/persistence |
| Telegram bot lifecycle | `src/services/bot/` | Preferences, manual checks, notifier startup |
| Telegram/email delivery | `src/services/notification/` | Callback ordering, whitelist, rate limit, i18n |
| Preference persistence | `src/integrations/database/preferences/` | GORM models, migrations, SQLite/Postgres selection |
| Browser/WebAuthn renewal | `src/integrations/auth/webauthn/` | Security-sensitive key and cookie handling |

## Code map

Go LSP and codegraph centrality were unavailable. `Refs` comes from ast-grep queries and import tracing, not semantic LSP references.

| Symbol | Type | Location | Refs | Role |
|--------|------|----------|------|------|
| `run` / `runFullMode` | functions | `cmd/rpa/main.go` | entry | Load config, start token manager, bot, scheduler |
| `buildDependencies` | function | `cmd/rpa/bootstrap.go` | 1 | Construct client, analyzer, preference store |
| `Client` | struct | `src/services/hourglass/client.go` | 4 packages | Hourglass HTTP, cookies, token renewal |
| `APIAnalyzer.AnalyzeSection` | method | `src/services/hourglass/analyzer.go` | 3 calls | Map four section families to rejection results |
| `TokenManager` | struct | `src/integrations/auth/webauthn/token_manager.go` | 4 constructors | Load, renew, persist auth cookies |
| `BotRunner.Run` | method | `src/services/bot/runner.go` | 1 call | Start Telegram updates and manual checks |
| `Scheduler.Run` | method | `src/services/scheduler/scheduler.go` | 1 call | Drive periodic concurrent analyses |
| `TelegramNotifier` | struct | `src/services/notification/telegram.go` | bot factory | Send messages and handle preference callbacks |
| `PreferenceStore` | interface | `src/integrations/database/preferences/store.go` | 8 files | Persist user preferences, runs, rejections, audit |

## Rules and invariants

- Treat `src/domain_models.AllSections` as the source of truth; analyzer aliases are input compatibility, not new canonical values.
- Use structured `log/slog`; telemetry supplies the default logger when OTLP is enabled.
- Do not log `hglogin`, XSRF cookies, WebAuthn private keys, Telegram tokens, database passwords, or full auth files.
- Do not weaken owner-only storage: secret directories are `0700`; token, credential, and SQLite files are `0600`.
- Do not convert documented best-effort Telegram or telemetry operations into fatal failures without tracing the caller behavior.
- Do not commit runtime artifacts such as root binaries, `coverage.out`, `.playwright-mcp/`, `.omo/`, Chrome profiles, or captured login requests.
- Local pre-push coverage is 95%; GitHub CI enforces 70% and skips a broader Chrome-dependent test set.

## Commands

```bash
make build          # build all four binaries
make test-short     # fast race suite with command/Chrome exclusions
make test           # full race suite and coverage.out
make fmt-check      # formatting target; note that it invokes go fmt
make lint           # golangci-lint v2
make ci-local       # local CI order plus build
make run-once       # build and invoke rpa -once
```

## Known gaps

- `Dockerfile` ships `rpa` and `token-refresh` only (no Chromium). Interactive auth CLIs stay host-side via `make build` / `make setup-auth`.
- Full mode constructs a scheduler but does not call `Scheduler.SetNotifier`; verify intended scheduled-delivery wiring before changing this path.
- Working tests use `httptest`, hand-written interface mocks, `t.TempDir`, `t.Setenv`, and injected filesystem/browser functions. WebAuthn coverage is mostly mocked, but named Chrome cases remain excluded by hooks/CI.
- Read the nearest child `AGENTS.md` before editing a documented boundary; child guidance overrides this root file for its package.\n