<div align="center">

# browserscale-kit

**The batteries-included Go toolkit for building browser-automation bots on [browserscale](https://browserscale.cloud).**

Config forms, an interactive configurator, a multi-process key/value store, work queues, proxy parsing, a tree logger, and an IMAP OTP fetcher — everything a real automation module needs *around* the browser, so you only write the browser flow.

[![Go Reference](https://pkg.go.dev/badge/github.com/browserscale/browserscale-kit.svg)](https://pkg.go.dev/github.com/browserscale/browserscale-kit)
![Go](https://img.shields.io/badge/go-1.25-00ADD8?logo=go&logoColor=white)
![Pure Go](https://img.shields.io/badge/cgo-free-success)
![License](https://img.shields.io/badge/license-MIT-blue)

[Install](#install) · [Quickstart](#quickstart) · [Packages](#packages) · [Ecosystem](#ecosystem)

</div>

---

## Why

The [`browserscale-go`](https://github.com/browserscale/browserscale-go) SDK drives a real cloud Chromium session. But a production bot is never *just* the browser flow — it needs config the operator can edit, persistent per-account state, proxy lists, work queues across threads, structured logging, and often a verification-code inbox.

`browserscale-kit` is that surrounding layer. Drop it next to the SDK and a module shrinks to one method — `doTask(ctx, browser)` — while the kit handles setup, config, persistence, and I/O. It is a **convenience, not a framework**: every package is usable à la carte, and a module author who wants different wiring can ignore `harness` and call the pieces directly.

> Fastest path: don't wire this by hand. Run [`browserscale init`](https://github.com/browserscale/browserscale-cli) and you get a runnable module that already imports the kit, with an `AGENTS.md` + offline docs that prime an AI coding agent to fill in the flow.

## Install

```bash
go get github.com/browserscale/browserscale-kit@latest
```

Requires **Go 1.25+**. Pure Go — no CGO, no system libraries, no SQLite DLL (the store uses [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite)), so `go build` yields a single static binary on every OS.

## Quickstart

A complete, runnable single-file module. The `harness` parses flags, lays out the working directory, runs the interactive configurator, opens the store, and calls `Run` — you own everything inside `Run`.

```go
package main

import (
	"context"
	"log"

	browserscale "github.com/browserscale/browserscale-go"
	"github.com/browserscale/browserscale-kit/form"
	"github.com/browserscale/browserscale-kit/harness"
	"github.com/browserscale/browserscale-kit/logger"
	"github.com/browserscale/browserscale-kit/module"
)

type Config struct {
	APIKey  string `json:"apiKey"`
	Threads int    `json:"threads"`
}

type Bot struct {
	cfg    *Config
	schema *form.Schema
}

func (b *Bot) Name() string    { return "hello_bot" }
func (b *Bot) Version() string { return "1.0.0" }

// Schema binds a typed config struct to an interactive form. It MUST return
// the same instance on every call so the configurator's edits reach Run.
func (b *Bot) Schema() *form.Schema {
	if b.schema != nil {
		return b.schema
	}
	b.cfg = &Config{}
	f := form.New(b.Name(), "Hello Bot", b.cfg)
	f.String("apiKey", &b.cfg.APIKey).Label("browserscale API key").Required()
	f.Int("threads", &b.cfg.Threads).Label("Threads").Default(4).Min(1)
	b.schema = f.Build()
	return b.schema
}

func (b *Bot) Run(ctx context.Context, env module.Env) error {
	l := logger.NewLogger("[hello_bot]", env.Logger)

	bc := browserscale.NewBrowserConfig(b.cfg.APIKey, 120, "", 0, "", "")
	browser, err := browserscale.RentBrowser(ctx, bc)
	if err != nil {
		return err
	}
	defer browser.Close()

	if _, err := browser.Navigate(ctx, "https://playground.browsercloud.cloud", 30000); err != nil {
		return err
	}
	l.Success("done")
	return nil
}

func main() {
	if err := harness.Run("hello_bot", []module.Module{&Bot{}}); err != nil {
		log.Fatal(err)
	}
}
```

```bash
go run .          # opens the configurator: fill the API key, name the task, run
go run . -h       # -module / -task / -config / -data flags for headless runs
```

## Packages

| Package | Import path | What it does |
| --- | --- | --- |
| **module** | `…/browserscale-kit/module` | The `Module` contract every bot implements, plus the `Env` of runtime services. |
| **harness** | `…/browserscale-kit/harness` | One-call program wiring: flags → dirs → configurator → store → `Run`, with Ctrl-C cancellation. |
| **form** | `…/browserscale-kit/form` | Fluent, type-safe config DSL that renders to prompts, JSON, and (later) a web GUI. |
| **configurator** | `…/browserscale-kit/configurator` | Flow control for the config phase: pick module → load → edit → validate → save → name the task. |
| **store** | `…/browserscale-kit/store` | Process-wide persistent key/value store (SQLite/WAL), namespaced, with atomic typed updates. |
| **input** | `…/browserscale-kit/input` | Error-honest list-file readers, a delimited/CSV parser, and a thread-safe work `Queue[T]`. |
| **proxy** | `…/browserscale-kit/proxy` | Parse every common proxy format, hand them out via a `Pool`, and liveness-check them. |
| **logger** | `…/browserscale-kit/logger` | Timestamped, colored, prefix-scoped tree logger that can tee to a file. |
| **mail** | `…/browserscale-kit/mail` | Long-lived IMAP `CodeFetcher` for pulling one-time verification codes out of a mailbox. |

Import them together:

```go
import "github.com/browserscale/browserscale-kit/{form,store,input,proxy,mail,logger,module,harness}"
```

---

### module — the contract

A module is a self-contained automation unit with a typed config and one entry point. There is no `Initialize`/`Stop` split — teardown is a plain `defer` inside `Run`.

```go
type Module interface {
	Name() string              // canonical id, e.g. "example_bot" (JSON filename + routing key)
	Version() string           // semver of the module implementation
	Schema() *form.Schema      // config form bound to the typed struct (same instance every call)
	Run(ctx context.Context, env Env) error
}
```

`Run` receives an `Env` of runtime services (any field may be zero in a minimal host — nil-check optional ones):

| Field | Type | Purpose |
| --- | --- | --- |
| `FilesDirectory` | `string` | Absolute dir holding operator-supplied input files. Resolve with `filepath.Join(env.FilesDirectory, cfg.SomeFile)`. |
| `Output` | `OutputFunc` | `Output(file, line, mode)` streams a result line to the task's output dir (`"w"`/`"write"` truncates, `"a"`/`"append"` appends). Writes are serialized. |
| `Status` | `StatusFunc` | Updates the host's status line (terminal title). |
| `Logger` | `*logger.Logger` | Parent logger to derive module-scoped children from. |
| `Store` | `*store.Store` | Process-wide persistent store; carve out `env.Store.Namespace(m.Name())`. |

Cancellation is delivered through `ctx` (the host cancels on SIGINT/SIGTERM). Honour it in your worker loops and return when done.

### harness — batteries-included wiring

```go
func harness.Run(appName string, modules []module.Module) error
```

`Run` is the whole `main()`. It:

1. Parses launch flags (see below).
2. Creates the working directory layout under `-data` (default `./data`): `Configs/`, `Files/`, `Tasks/`, `Stores/`.
3. Runs the `configurator` (auto-skips the picker when only one module is registered).
4. Opens the persistent store at `Stores/<appName>.db`.
5. Builds the `module.Env` and runs the selected module until it finishes or the process is interrupted (Ctrl-C / SIGTERM cancels `ctx`).

It returns `nil` on a clean finish **and** on user cancellation (`harness.ErrCanceled` is re-exported if you want to distinguish).

Launch flags (also available standalone via `harness.ParseLaunchArgs`) let a run skip interactive setup — handy for CI and agents:

| Flag | Meaning |
| --- | --- |
| `-module NAME` | Module to launch (only needed when several are registered). |
| `-task NAME` | Task / project folder name under `Tasks/`; empty prompts. |
| `-config PATH` | Config JSON to load (absolute, relative, or a bare name resolved against `Configs/`). Skips the editor and never writes back. |
| `-data DIR` | Working-directory root (default `./data`). |

### form — the config DSL

Bind a typed struct to a form with a fluent builder. The resulting `Schema` is consumed by the configurator for prompts, persisted as JSON, and can be exported as JSON Schema for a future GUI.

```go
cfg := &MyConfig{}
f := form.New("my_module", "My Module", cfg)
f.Int("threads", &cfg.Threads).Label("Threads").Required().Default(1).Min(1)
f.Bool("useProxies", &cfg.UseProxies).Label("Use Proxies").Default(false)
f.File("proxiesFile", &cfg.ProxiesFile).
	Label("Proxies File").
	Tag(form.TagProxies).           // validate: file parses to ≥1 proxy
	ShowWhenTruthy("useProxies").   // only shown when useProxies is true
	Required()
f.Select("region", &cfg.Region).Label("Region").
	Option("us", "United States").Option("de", "Germany").Default("us")
schema := f.Build()
```

**Field starters** on `*Form` (each returns a typed builder): `String`, `Int`, `Float`, `Bool`, `Select`, `File`, `StringList`.

**Builder methods** (chainable, common to all): `Label`, `Help`, `Required`, `Default(v)`, `Validate(fn)`, and conditional visibility — `ShowWhen(func() bool)`, `ShowWhenEq(field, value)`, `ShowWhenIn(field, values…)`, `ShowWhenTruthy(field)`. Kind-specific: `Min`/`Max` (`Int`, `Float`), `Option(value, label)` (`Select`), `Tag(tag)` + `ValidateFile(fn)` (`File`).

**File tags** drive validation: `TagRaw` (exists), `TagLines` (≥1 non-empty line), `TagLinesPath`, `TagProxies` (≥1 parseable proxy).

**Schema** operations: `Build()`, `Validate(filesDir) ValidationErrors`, `ApplyDefaults()`, `LoadJSON`/`LoadMap`, `SaveJSON`/`ToMap`, `VisibleFields()`, `Field(name)`. Hidden fields keep their values on save, so toggling a controlling field back on never loses data.

### configurator — the config phase

Pure flow control (no UI of its own — UI lives behind a `Renderer`): pick module → apply defaults → load JSON → edit → validate → save → name the task. `harness` uses the bundled `NewHuhRenderer()` (a [Charm huh](https://github.com/charmbracelet/huh) TUI). Non-interactive overrides (`PreselectedModuleName`, `ConfigPathOverride`, `PreselectedTaskName`) skip the matching step, so you can run fully headless.

### store — persistent, multi-process key/value

SQLite in WAL mode via pure-Go `modernc.org/sqlite`: many concurrent readers across processes, one writer at a time. Foreign / corrupt DB files are moved aside and recreated transparently.

```go
s, _ := store.Open(filepath.Join(root, "state.db"))
defer s.Close()

ns := s.Namespace("example_bot")              // isolated partition; keys never collide across namespaces
sessions := store.NewJSON[Session](ns)        // typed JSON codec on top

// Atomic read-modify-write in a BEGIN IMMEDIATE tx — safe across goroutines AND processes.
sessions.Update("user@example.com", func(cur Session, exists bool) (Session, error) {
	cur.Status = "logged_in"
	return cur, nil
})
```

- **`*Store`**: `Open(path)`, `Close`, `Path`, `Namespace(name)`.
- **`*Namespace`** (raw bytes): `Get`, `Put`, `Delete`, `Update`, `Keys(prefix)`, `ForEach(prefix, fn)`, `Count`, `DropNamespace`.
- **`*JSON[T]`** (typed): `NewJSON[T](ns)` then `Get`, `Put`, `Update`, `Delete`, `Keys`, `ForEach`.

`Get` returns `(value, ok, err)` — "present with empty value" is distinct from "absent". Return `(nil, nil)` from an `Update` fn to delete a key.

### input — files & work queues

Error-honest replacements for the usual "read a list file" grab-bag: they return errors instead of swallowing them.

```go
q, _ := input.LineQueue("accounts.txt")   // thread-safe Queue[string] of trimmed, non-empty lines
for {
	acct, ok := q.Next()                  // each item handed to exactly one worker
	if !ok { break }                      // drained
	// ... process acct ...
}
```

- Readers: `Lines`, `NonEmptyLines`, `Files(dir)`, `Columns(path, sep)`, `CSV(path)`.
- **`Queue[T]`**: `NewQueue`, `Next` (drain in order), `Random` (sample with replacement, never empties), `Remaining`, `Reset`, `Len`; `LineQueue(path)` convenience.

### proxy — parse, pool, check

```go
pool, _ := proxy.LoadPool("proxies.txt")   // parses every common format, skips bad lines
p, ok := pool.Next()                        // round-robin; pool.Random() samples with replacement
_ = proxy.Check(p, 5*time.Second)           // optional liveness gate via ip-api.com
url := p.URL()                              // scheme://[user:pass@]host:port
```

`Parse` accepts `host:port`, `host:port:user:pass`, `user:pass@host:port`, `host:port@user:pass`, and `scheme://[user:pass@]host:port`, returning descriptive errors. `Proxy` exposes `HasAuth`, `Addr`, `URL`; parse in bulk with `ParseLines` / `ParseFile`.

### logger — scoped, colored, tee-able

```go
l := logger.NewLogger("[App]", nil)          // root
w := logger.NewLogger("[Worker-3]", l)        // child: prefix "[App] [Worker-3]", shares the mutex + file
w.Println("started"); w.Success("ok"); w.Error("boom")
l.SetLogPath("run.log")                       // also tee every line to a file
```

Loggers form a tree; children share the parent's mutex (concurrent writes never interleave) and file destination. All methods are goroutine-safe.

### mail — one-time verification codes

A long-lived, concurrency-safe IMAP poller for pulling OTP / verification codes out of a (often catch-all) mailbox. Open one per mailbox at start; call `FetchCode` from every worker — requests multiplex over a single kept-alive, auto-reconnecting connection. Pure-Go IMAP over implicit TLS, no external dependency.

```go
f, _ := mail.NewCodeFetcher(host, port, user, pass)
defer f.Stop()

// The mailTime idiom — capture BEFORE the action that triggers the email.
mailTime := time.Now().UnixMilli()
browser.Click(ctx, browserscale.CSS("#send-code"))     // triggers the send

code, err := f.FetchCode(ctx,
	"noreply@service.com", // fromEmail  ("" = any sender)
	toEmail,               // toEmail    (the account address on a catch-all mailbox; "" = any)
	"",                    // subjectKeyword ("" = skip)
	`(\d{6})`,             // codeRegex  (ONE capture group; first submatch returned)
	mailTime-60_000,       // sinceUnixMilli (mailTime minus ~60s clock-skew buffer)
	60_000,                // maxSearchTimeMs
	true,                  // deleteAfterFetch (keeps a shared mailbox clean)
)
```

> **Why `mailTime`?** IMAP's `SINCE` search is day-granular, so `FetchCode` filters on the millisecond-precision `INTERNALDATE` client-side. Passing the timestamp captured *before* the trigger (minus a skew buffer) is what guarantees you match the fresh code and never a stale one left in the mailbox. `EnableDebug()` turns on verbose connection logging; the package is silent by default.

## Ecosystem

`browserscale-kit` is one of three companion projects:

| Project | Role |
| --- | --- |
| [**browserscale-go**](https://github.com/browserscale/browserscale-go) | The SDK — rent and drive a real cloud Chromium session (navigate, wait, click, fill, read, evaluate, captcha, cookies…). |
| [**browserscale-cli**](https://github.com/browserscale/browserscale-cli) | `browserscale init` — scaffold a runnable module that already imports this kit and the SDK, with agent-ready docs. |
| **browserscale-kit** (you are here) | The toolkit *around* the browser: config, store, queues, proxies, logging, mail. |

Also available: [**browserscale-ts**](https://github.com/browserscale/browserscale-ts), the TypeScript SDK.

## Design notes

- **Pure Go, single binary.** No CGO anywhere — the store's SQLite is `modernc.org/sqlite`.
- **Concurrency-first.** `store`, `input.Queue`, `proxy.Pool`, `logger`, and `mail.CodeFetcher` are all safe for use from many goroutines; the store is safe across processes too.
- **The kit helps, it doesn't dictate.** Every package stands alone. Reach for `harness` for the fast path, or wire the pieces yourself.

## License

MIT © browserscale
