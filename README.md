# browserscale-kit

[![Go Reference](https://pkg.go.dev/badge/github.com/browserscale/browserscale-kit.svg)](https://pkg.go.dev/github.com/browserscale/browserscale-kit)

The Go framework browserscale bots run on. It reads the settings, hands the jobs to threads, rents a browser for a job when it uses one, through the configured proxies, and reports every step and end as it happens. The bot itself only says what one job does in the browser, and what each way it can end means.

Bots written in [browserscale studio](https://github.com/browserscale/browserscale) are built on it; it works just as well on its own, next to [browserscale-go](https://github.com/browserscale/browserscale-go).

```sh
go get github.com/browserscale/browserscale-kit@latest
```

## A bot

```go
package main

import (
	"iter"

	kit "github.com/browserscale/browserscale-kit"
	"github.com/browserscale/browserscale-kit/form"
	"github.com/browserscale/browserscale-kit/proxy"
)

type Settings struct {
	Accounts int    `json:"accounts"`
	Threads  int    `json:"threads"`
	Proxies  string `json:"proxies"`
}

func main() {
	kit.Main(kit.Bot[Settings, Account]{
		Name: "outlook-register",
		Settings: func(f *form.Form, s *Settings) {
			f.Int("accounts", &s.Accounts).Label("Accounts to create").Default(1).Min(1)
			f.Int("threads", &s.Threads).Label("Threads").Help("How many signups run at once").Default(1).Min(1)
			f.Proxies("proxies", &s.Proxies).Label("Proxies").Kinds(proxy.KindGateway, proxy.KindManaged).Fresh()
		},
		Jobs:    func(r *kit.Run, s *Settings) iter.Seq[Account] { return kit.Until(r, s.Accounts, newAccount, "registered") },
		Threads: func(s *Settings) int { return s.Threads },
		Job:     register,
		Ends: kit.Ends{
			"registered":     kit.Good,
			"phone_required": kit.Neutral,
			"blocked":        kit.Bad.ChangeProxy(),
		},
	})
}
```

`register` is the job, `func(j *kit.Job, s *Settings, a Account) error`: it opens the job's page with `j.Page()`, goes through the signup, and ends the job by one of the names in `Ends` with `j.End`, or returns an error. Everything around it is the kit's.

## What it does for you

- **Settings.** A typed form (`form`) that renders as terminal prompts, as JSON and as the settings screen in Studio, saved to `data/config.json`.
- **Jobs and threads.** `Times`, `Until`, `Lines` and `Rows` for a number of jobs or an input file, `Forever`, `Every` and `At` for a run that keeps going, `Queue` for jobs other jobs hand over. Each job gets its own browser the first time it asks for one.
- **Programs.** Several tasks side by side in one run, such as signups next to a task that watches the mailbox, handing each other work with `Run.Push` and `Run.Post`.
- **Steps.** A job can be written as steps with `Job.Flow`; the kit reports which step each job is in, and a run can start at one (`-from`) or stop after one (`-until`).
- **Errors.** Trouble with the browser or the proxy is retried with a fresh browser, and another proxy when the proxy was at fault. Outcomes of the site end the job by the bot's own names. Anything else ends it as `error` with its step and a screenshot, and stops the run when the same step keeps failing.
- **Proxies.** Lists, provider gateways, static addresses and mobile proxies, or browserscale's managed proxies; http and socks5, with MASQUE for HTTP/3. A failed proxy rests or rotates according to its kind (`proxy`).
- **Store.** What jobs keep across runs, in `data/store.db`, with atomic updates that hold across processes (`store`).
- **Runs.** Every run writes its events, its settings, one row per job and what jobs saved to `runs/<run>/`.
- **Mail.** An IMAP code fetcher for one-time verification codes (`mail`).

The package documentation (`go doc github.com/browserscale/browserscale-kit`) covers all of it, including the flags for developing a bot.

## Packages

| Package | |
|---|---|
| `kit` | Bots, programs, jobs, runs, ends, proxies and events |
| `form` | The settings form |
| `configurator` | Asking for and saving the settings |
| `proxy` | Proxy formats, sources and the pool |
| `store` | The persistent key/value store |
| `input` | List and delimited file readers, and a work queue |
| `mail` | Verification codes from a mailbox |

## Earlier versions

Up to v0.2.0 the kit had a different API, built around the `module` and `harness` packages. It is no longer developed; those releases stay available under their tags.

## License

MIT
