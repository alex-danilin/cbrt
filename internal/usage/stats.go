package usage

import (
	"sort"
	"time"
)

// Counts holds invocations split by outcome.
type Counts struct {
	Completed int64 `json:"completed"`
	Partial   int64 `json:"partial"`
	Cancelled int64 `json:"cancelled"`
	Failed    int64 `json:"failed"`
}

// Invocations returns the total number of invocations.
func (c Counts) Invocations() int64 { return c.Completed + c.Partial + c.Cancelled + c.Failed }

func (c *Counts) add(o Counts) {
	c.Completed += o.Completed
	c.Partial += o.Partial
	c.Cancelled += o.Cancelled
	c.Failed += o.Failed
}

func (c *Counts) inc(o Outcome) {
	switch o {
	case OutcomeCompleted:
		c.Completed++
	case OutcomePartial:
		c.Partial++
	case OutcomeCancelled:
		c.Cancelled++
	default:
		c.Failed++
	}
}

// Tokens holds token counts split by usage source.
type Tokens struct {
	PromptReported      int64 `json:"prompt_reported"`
	PromptEstimated     int64 `json:"prompt_estimated"`
	CompletionReported  int64 `json:"completion_reported"`
	CompletionEstimated int64 `json:"completion_estimated"`
	Reasoning           int64 `json:"reasoning"`
	Cached              int64 `json:"cached"`
	UnknownCount        int64 `json:"unknown_count"` // records whose usage is unknown
}

// Total returns prompt plus completion tokens from both sources.
func (t Tokens) Total() int64 {
	return t.PromptReported + t.PromptEstimated + t.CompletionReported + t.CompletionEstimated
}

func (t *Tokens) add(o Tokens) {
	t.PromptReported += o.PromptReported
	t.PromptEstimated += o.PromptEstimated
	t.CompletionReported += o.CompletionReported
	t.CompletionEstimated += o.CompletionEstimated
	t.Reasoning += o.Reasoning
	t.Cached += o.Cached
	t.UnknownCount += o.UnknownCount
}

func (t *Tokens) addRecord(r *Record) {
	switch r.UsageSource {
	case SourceReported:
		t.PromptReported += r.PromptTokens
		t.CompletionReported += r.CompletionTokens
	case SourceEstimated:
		t.PromptEstimated += r.PromptTokens
		t.CompletionEstimated += r.CompletionTokens
	default:
		t.UnknownCount++
	}
	t.Reasoning += r.ReasoningTokens
	t.Cached += r.CachedTokens
}

// ProviderStats summarises one provider.
type ProviderStats struct {
	Provider    string `json:"provider"`
	Location    string `json:"location"`
	Today       Counts `json:"today"`
	Month       Counts `json:"month"` // last 30 days including today
	TokensToday Tokens `json:"tokens_today"`
	TokensMonth Tokens `json:"tokens_month"`
}

// ModelStats summarises one provider/model pair.
type ModelStats struct {
	Provider     string  `json:"provider"`
	Model        string  `json:"model"`
	Today        Counts  `json:"today"`
	Month        Counts  `json:"month"`
	TokensMonth  Tokens  `json:"tokens_month"`
	P50LatencyMS int64   `json:"p50_latency_ms"` // bucket upper bound, completed+partial over the month
	FailureRate  float64 `json:"failure_rate"`   // failed / invocations over the month
}

// DayStats is one day of the 14-day chart.
type DayStats struct {
	Day                   string           `json:"day"`
	TokensByProvider      map[string]int64 `json:"tokens_by_provider"`
	InvocationsByProvider map[string]int64 `json:"invocations_by_provider"`
}

// Health reports persistence problems.
type Health struct {
	Dropped       int64 `json:"dropped"`
	PersistErrors int64 `json:"persist_errors"`
	CorruptLines  int64 `json:"corrupt_lines"`
}

// Stats is the dashboard view of the aggregates.
type Stats struct {
	GeneratedAt time.Time       `json:"generated_at"`
	Providers   []ProviderStats `json:"providers"` // sorted by provider
	Models      []ModelStats    `json:"models"`    // month invocations desc, then provider/model
	Daily       []DayStats      `json:"daily"`     // 14 days, oldest first, ending today
	Health      Health          `json:"health"`
}

// Latency histogram upper bounds in ms; the last bucket is +inf.
var latencyBounds = [...]int64{100, 250, 500, 1000, 2000, 5000, 10000, 30000, 60000, 120000, 300000}

// numBuckets includes the +inf bucket.
const numBuckets = len(latencyBounds) + 1

type histogram [numBuckets]int64

func (h *histogram) observe(ms int64) {
	for i, b := range latencyBounds {
		if ms <= b {
			h[i]++
			return
		}
	}
	h[numBuckets-1]++
}

func (h *histogram) add(o histogram) {
	for i := range h {
		h[i] += o[i]
	}
}

// p50 returns the upper bound of the bucket holding the median, 0 if empty.
func (h *histogram) p50() int64 {
	var total int64
	for _, c := range h {
		total += c
	}
	if total == 0 {
		return 0
	}
	var cum int64
	for i, c := range h {
		cum += c
		if 2*cum >= total {
			if i < len(latencyBounds) {
				return latencyBounds[i]
			}
			break
		}
	}
	return latencyBounds[len(latencyBounds)-1]
}

const dayFormat = "2006-01-02"

// dayOffset returns the UTC day string d days before t.
func dayOffset(t time.Time, d int) string {
	return t.UTC().AddDate(0, 0, -d).Format(dayFormat)
}

// Stats returns a snapshot of the aggregates for the dashboard.
func (s *Store) Stats() Stats {
	now := s.now()
	today := dayOffset(now, 0)
	monthStart := dayOffset(now, 29)
	chartStart := dayOffset(now, 13)

	st := Stats{
		GeneratedAt: now,
		Providers:   []ProviderStats{},
		Models:      []ModelStats{},
		Health: Health{
			Dropped:       s.dropped.Load(),
			PersistErrors: s.persistErrors.Load(),
			CorruptLines:  s.corruptLines.Load(),
		},
	}
	daily := make(map[string]*DayStats, 14)
	for i := 13; i >= 0; i-- {
		d := &DayStats{Day: dayOffset(now, i), TokensByProvider: map[string]int64{}, InvocationsByProvider: map[string]int64{}}
		daily[d.Day] = d
		st.Daily = append(st.Daily, DayStats{}) // placeholder, filled below
	}

	type modelKey struct{ provider, model string }
	type modelAcc struct {
		ModelStats
		hist histogram
	}
	provs := map[string]*ProviderStats{}
	models := map[modelKey]*modelAcc{}

	s.mu.Lock()
	for p, loc := range s.locations {
		provs[p] = &ProviderStats{Provider: p, Location: loc}
	}
	for k, b := range s.aggs {
		if k.day < monthStart || k.day > today {
			continue
		}
		ps := provs[k.provider]
		if ps == nil {
			ps = &ProviderStats{Provider: k.provider, Location: s.locations[k.provider]}
			provs[k.provider] = ps
		}
		mk := modelKey{k.provider, k.model}
		ma := models[mk]
		if ma == nil {
			ma = &modelAcc{ModelStats: ModelStats{Provider: k.provider, Model: k.model}}
			models[mk] = ma
		}
		ps.Month.add(b.Counts)
		ps.TokensMonth.add(b.Tokens)
		ma.Month.add(b.Counts)
		ma.TokensMonth.add(b.Tokens)
		ma.hist.add(b.Latency)
		if k.day == today {
			ps.Today.add(b.Counts)
			ps.TokensToday.add(b.Tokens)
			ma.Today.add(b.Counts)
		}
		if k.day >= chartStart {
			d := daily[k.day]
			d.TokensByProvider[k.provider] += b.Tokens.Total()
			d.InvocationsByProvider[k.provider] += b.Counts.Invocations()
		}
	}
	s.mu.Unlock()

	for _, ps := range provs {
		st.Providers = append(st.Providers, *ps)
	}
	sort.Slice(st.Providers, func(i, j int) bool { return st.Providers[i].Provider < st.Providers[j].Provider })

	for _, ma := range models {
		m := ma.ModelStats
		m.P50LatencyMS = ma.hist.p50()
		if n := m.Month.Invocations(); n > 0 {
			m.FailureRate = float64(m.Month.Failed) / float64(n)
		}
		st.Models = append(st.Models, m)
	}
	sort.Slice(st.Models, func(i, j int) bool {
		a, b := st.Models[i], st.Models[j]
		if ai, bi := a.Month.Invocations(), b.Month.Invocations(); ai != bi {
			return ai > bi
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Model < b.Model
	})

	for i := range st.Daily {
		st.Daily[i] = *daily[dayOffset(now, 13-i)]
	}
	return st
}
