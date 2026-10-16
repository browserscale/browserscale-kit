package kit

import (
	"cmp"
	"slices"
)

// Tone is how an end reads in reports: good, bad or neutral. It colors the
// end and decides what Until counts; nothing else depends on it.
type Tone string

const (
	ToneGood    Tone = "good"
	ToneNeutral Tone = "neutral"
	ToneBad     Tone = "bad"
)

// End says what a named end means for the run. Start from [Good], [Neutral]
// or [Bad] and add to it:
//
//	kit.Bad.Retry(2).ChangeProxy()
type End struct {
	tone        Tone
	retries     int
	changeProxy bool
}

var (
	Good    = End{tone: ToneGood}
	Neutral = End{tone: ToneNeutral}
	Bad     = End{tone: ToneBad}
)

// Retry tries the job again up to n times when it ends this way, each time
// with a fresh browser.
func (e End) Retry(n int) End {
	e.retries = n
	return e
}

// ChangeProxy lets the proxy rest after this end, so the next try or job
// gets another one. Without it the proxy counts as one that works.
func (e End) ChangeProxy() End {
	e.changeProxy = true
	return e
}

// Tone is how the end reads in reports.
func (e End) Tone() Tone { return e.tone }

// Ends names the ways a job can end and what each means. The names are the
// bot's own, such as "registered", "phone_required" or "price_changed"; a
// bot has as many as it needs, of whatever tones. They are what a run
// reports and counts.
//
// The kit ends jobs itself as "done" when a job returns without calling End
// (good), "error", "proxy_failed" and "gave_up" (bad), and "stopped" for
// jobs cut short by the end of the run (neutral). Listing one of these here
// changes what it means. An end used but not listed is neutral.
type Ends map[string]End

// Names of the ends the kit reports itself.
const (
	EndDone        = "done"
	EndError       = "error"
	EndProxyFailed = "proxy_failed"
	EndGaveUp      = "gave_up"
	EndStopped     = "stopped"
)

func (ends Ends) of(name string) End {
	if e, ok := ends[name]; ok {
		if e.tone == "" {
			e.tone = ToneNeutral
		}
		return e
	}
	switch name {
	case EndDone:
		return Good
	case EndError, EndProxyFailed, EndGaveUp:
		return Bad
	}
	return Neutral
}

// EndInfo is an end as runs announce it, with its task in a program with
// named tasks.
type EndInfo struct {
	Name string `json:"name"`
	Tone Tone   `json:"tone"`
	Task string `json:"task,omitempty"`
}

var toneOrder = map[Tone]int{ToneGood: 0, ToneNeutral: 1, ToneBad: 2}

// list is the bot's ends, good ones first, then by name.
func (ends Ends) list() []EndInfo {
	out := make([]EndInfo, 0, len(ends))
	for name := range ends {
		out = append(out, EndInfo{Name: name, Tone: ends.of(name).tone})
	}
	slices.SortFunc(out, func(a, b EndInfo) int {
		return cmp.Or(cmp.Compare(toneOrder[a.Tone], toneOrder[b.Tone]), cmp.Compare(a.Name, b.Name))
	})
	return out
}
