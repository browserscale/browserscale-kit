// Package kit runs browserscale bots. It reads the settings, hands the jobs
// to threads, rents a browser for a job when it uses one, through the
// configured proxies, and reports every step and end as it happens.
//
// A bot is a [Bot] value passed to [Main]. Its settings say what the run is
// for in the user's terms, such as how many accounts to create, and how many
// jobs run at once:
//
//	type Settings struct {
//		Accounts int    `json:"accounts"`
//		Threads  int    `json:"threads"`
//		Proxies  string `json:"proxies"`
//	}
//
//	func main() {
//		kit.Main(kit.Bot[Settings, Account]{
//			Name: "outlook-register",
//			Settings: func(f *form.Form, s *Settings) {
//				f.Int("accounts", &s.Accounts).Label("Accounts to create").Default(1).Min(1)
//				f.Int("threads", &s.Threads).Label("Threads").Help("How many signups run at once").Default(1).Min(1)
//				f.Proxies("proxies", &s.Proxies).Label("Proxies").Kinds(proxy.KindGateway, proxy.KindManaged).Fresh()
//			},
//			Jobs:    func(r *kit.Run, s *Settings) iter.Seq[Account] { return kit.Until(r, s.Accounts, newAccount, "registered") },
//			Threads: func(s *Settings) int { return s.Threads },
//			Job:     register,
//			Ends: kit.Ends{
//				"registered":     kit.Good,
//				"phone_required": kit.Neutral,
//				"blocked":        kit.Bad.ChangeProxy(),
//			},
//		})
//	}
//
// # Jobs and threads
//
// Jobs says what the run works through: [Times] for a number of jobs,
// [Until] for jobs until enough ended a given way (accounts created, however
// many signups got blocked on the way), [Lines] and [Rows] for an input
// file, [Forever], [Every] and [At] for a run that keeps going, and [Queue]
// for jobs that other jobs hand over. Threads is how many jobs run at once.
// Each job gets its own browser, rented the first time it calls
// [Job.Browser] or [Job.Page] and given back when it ends; a job that never
// does, such as one that reads a mailbox or calls an API, runs without one.
// Browsers limits how many browsers the jobs have at once, for bots whose
// jobs need one only sometimes.
//
// # Programs and tasks
//
// A [Program] has several [Task]s: kinds of jobs that run side by side in
// one run, each with its own source, threads and ends, such as signups in
// browsers next to a task that watches the mailbox, or a service that runs
// until it is stopped and takes its work from a queue:
//
//	kit.Main(kit.Program[Settings]{
//		Name:     "outlook-register",
//		Settings: settings,
//		Tasks: []kit.Tasker[Settings]{
//			kit.Task[Settings, Account]{Name: "signup", Jobs: accounts, Threads: threads, Job: register, Ends: ends},
//			kit.Task[Settings, int]{Name: "inbox", Jobs: everyTenSeconds, Job: readInbox},
//		},
//	})
//
// Tasks hand each other work in two ways. [Run.Push] puts an item into a
// queue that a task reads with [Queue]: a confirmation link the inbox found,
// for a task that opens it in a browser. [Run.Post] leaves a value under a
// key that one job takes with [Await]: the code for an address, for the
// signup waiting on it. A job reaches both through [Job.Run].
//
// A run ends once every task ran out of jobs; a task fed by a queue runs
// out when nothing more can come. A task with [Forever], [Every] or [At]
// keeps the run going until it is stopped: a program that runs as a service.
// -task runs only some of a program's tasks, e.g. to try one.
//
// # Steps
//
// A job may be written as one function, or as steps: functions that return
// the step to go on with. [Job.Flow] runs them, reports which step each job
// is in, and lets a run start at a given step (-from). A step that ends the
// job returns [Job.End].
//
// # What goes wrong
//
// Errors a job returns are sorted into three kinds:
//
//   - Trouble with the browser or the proxy: the kit retries the job with a
//     fresh browser, and with another proxy when the proxy was at fault. A
//     proxy that failed rests for a while before it is used again. A job can
//     say so itself with [Job.Retry] and [Job.BadProxy], for instance when a
//     site rejects the proxy's address although the proxy works.
//   - Outcomes of the site: the job ends with a name of the bot's own, such
//     as "blocked", and [Ends] says whether that reads as good, neutral or
//     bad, whether to retry and whether to change the proxy.
//   - Anything else, such as a wait that ran out with nothing it knew of on
//     the page: the job ends as "error" with its step, the error code and a
//     screenshot. When the same step fails this way again and again the run
//     stops, since the site has likely changed.
//
// A missing API key or credits stop the run right away.
//
// # Proxies
//
// The proxies setting, a [form.Form.Proxies] field, names a saved source,
// proxy:<id> or proxy:<id>/<profile>, which Studio keeps in its Proxies tab
// and points the bot at with BROWSERSCALE_PROXIES; or a list file in data, a
// source spec (.json), or browserscale's managed proxies ("managed",
// "managed:de"; also when empty). A source is a list used in turn, a
// provider gateway written as a template, static addresses with ids, or
// mobile proxies with a rotation URL; see package proxy. Lines may be http
// or socks5(h), and a gateway may name a MASQUE proxy for HTTP/3.
//
// The field says what the bot needs of them, and the settings form holds
// the user's choice against it: Kinds turns other kinds down, while Fresh (a
// new address for every job's browser), Keeps (an account keeps its address) and Country
// add a note where a choice doesn't quite fit. Managed proxies get the
// field's Country unless the setting names one.
//
// How a failed proxy is treated follows its kind:
//
//   - a list entry or static address rests, longer each time it fails again;
//   - a gateway or line with {session} or {port:a-b} gives the next browser
//     a new address, and rests only after failing several times in a row;
//   - a mobile proxy is asked for a new address before it's used again.
//
// [Job.UseProxy] pins a job to a static address by id, for accounts that
// keep their address; [Job.ProxyID] says which one the browser went
// through, to remember it with the account.
//
// # Files
//
//	data/config.json      the settings
//	data/*                input files such as proxies.txt
//	data/store.db         what jobs keep across runs (Job.Store)
//	runs/<run>/events.jsonl  everything that happened, one JSON event per line
//	runs/<run>/settings.json the settings the run went with, without secret ones
//	runs/<run>/status.csv    one row per job: end, tone, step, detail, task
//	runs/<run>/<name>.csv    what jobs saved with Job.Save
//	runs/<run>/shots/        the page of every job that failed unexpectedly
//
// # While developing
//
// -from starts the first job at a step and -until stops it after one (in a
// program, the first job whose flow has that step), -jobs stops each task
// after a number of jobs, -threads sets each task's threads,
// -attach runs it in a browser that is already open and leaves that open,
// and -hold keeps the browser of a job that failed unexpectedly or stopped
// after -until, and stops the run, to look at the page.
//
// -settings sets values over data/config.json's for one run, e.g.
// -settings '{"proxies":"list1.txt"}', and -label names it, so runs that
// differ can go side by side and be compared. Runs at the same time share
// data/ and store.db; ones that read the same input file work on the same
// entries, unless each job first claims its entry with an Update in the
// store, which is atomic across processes.
package kit
