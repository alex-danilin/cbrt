// Package gateway routes Chat Completions requests across providers with failover.
package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/alex-danilin/cbrt/internal/config"
	"github.com/alex-danilin/cbrt/internal/events"
	"github.com/alex-danilin/cbrt/internal/provider"
	"github.com/alex-danilin/cbrt/internal/router"
	"github.com/alex-danilin/cbrt/internal/usage"
)

// runtime is the part of the state that a config reload replaces atomically.
type runtime struct {
	cfg      *config.Config
	ups      map[string]*provider.Upstream
	patterns map[string]router.Patterns
	globs    []string // glob route names, most specific (longest) first
}

// Served describes the last target that answered for a route or session.
type Served struct {
	Route      string    `json:"route"`
	LastTarget string    `json:"last_target"`
	LastAt     time.Time `json:"last_at"`
	Fallback   bool      `json:"fallback"`
	Reason     string    `json:"reason,omitempty"`
}

type Gateway struct {
	rt     atomic.Pointer[runtime]
	health *router.Health
	hub    *events.Hub
	store  *usage.Store
	log    *slog.Logger
	now    func() time.Time

	mu       sync.Mutex
	byRoute  map[string]Served
	bySess   map[string]Served
	sessList []string       // insertion order, for bounding bySess
	learned  map[string]int // target key -> context size learned from upstream errors
	models   map[string][]string

	reqSeq atomic.Uint64

	downgraded    atomic.Int64 // client features dropped/approximated by translation, and repaired upstream output
	rejectedTypes atomic.Int64 // client requests rejected as untranslatable

	dgMu          sync.Mutex
	seenDowngrade map[string]struct{} // downgrade kinds already logged once (at most maxDowngradeKinds)
	dgCapped      bool                // the cap was hit and announced
}

const (
	maxSessions       = 1000
	maxDowngradeKinds = 256 // distinct downgrade kinds logged once each
	maxDowngradeKind  = 64  // bytes of a kind kept for logging
)

func New(cfg *config.Config, health *router.Health, hub *events.Hub, store *usage.Store, log *slog.Logger) (*Gateway, error) {
	if log == nil {
		log = slog.Default()
	}
	g := &Gateway{
		health: health, hub: hub, store: store, log: log, now: time.Now,
		byRoute: map[string]Served{}, bySess: map[string]Served{},
		learned: map[string]int{}, models: map[string][]string{},
	}
	if err := g.Reload(cfg); err != nil {
		return nil, err
	}
	return g, nil
}

// Reload swaps in a new configuration. Providers disabled by auth errors are re-enabled.
func (g *Gateway) Reload(cfg *config.Config) error {
	rt := &runtime{cfg: cfg, ups: map[string]*provider.Upstream{}, patterns: map[string]router.Patterns{}}
	for _, p := range cfg.Providers {
		var pat router.Patterns
		for _, s := range p.QuotaPatterns {
			re, err := regexp.Compile("(?i)" + s)
			if err != nil {
				return fmt.Errorf("provider %s: %w", p.ID, err)
			}
			pat.Quota = append(pat.Quota, re)
		}
		for _, s := range p.AuthPatterns {
			re, err := regexp.Compile("(?i)" + s)
			if err != nil {
				return fmt.Errorf("provider %s: %w", p.ID, err)
			}
			pat.Auth = append(pat.Auth, re)
		}
		rt.patterns[p.ID] = pat
		rt.ups[p.ID] = provider.New(p, provider.Offline(cfg.Offline))
		if p.IsEnabled() && p.Location == "local" && !provider.LocalLooking(p.BaseURL) {
			g.log.Warn("provider is labelled local but its base_url host does not look local; offline mode still refuses non-local addresses",
				"provider", p.ID)
		}
	}
	for name := range cfg.Routes {
		if strings.ContainsAny(name, "*?[") {
			rt.globs = append(rt.globs, name)
		}
	}
	sort.Slice(rt.globs, func(i, j int) bool {
		if len(rt.globs[i]) != len(rt.globs[j]) {
			return len(rt.globs[i]) > len(rt.globs[j])
		}
		return rt.globs[i] < rt.globs[j]
	})
	if old := g.rt.Swap(rt); old != nil {
		for _, u := range old.ups {
			u.CloseIdle()
		}
		g.health.ResetAuth()
	}
	return nil
}

// Config returns the active configuration.
func (g *Gateway) Config() *config.Config { return g.rt.Load().cfg }

// resolve maps a requested model to a route: an exact alias, a glob route, or direct "provider:model".
func (rt *runtime) resolve(model string) (string, []config.Target, bool) {
	if ts, ok := rt.cfg.Routes[model]; ok {
		return model, ts, true
	}
	for _, g := range rt.globs {
		if ok, _ := path.Match(g, model); ok {
			return g, rt.cfg.Routes[g], true
		}
	}
	if p, m, ok := strings.Cut(model, ":"); ok {
		if _, known := rt.ups[p]; known && m != "" {
			t := config.Target{Provider: p, Model: m}
			// inherit the profile from any route that already lists this target
			for _, ts := range rt.cfg.Routes {
				for _, rtT := range ts {
					if rtT.Key() == t.Key() {
						t = rtT
					}
				}
			}
			return "direct", []config.Target{t}, true
		}
	}
	return "", nil, false
}

func (g *Gateway) contextFor(t config.Target) int {
	g.mu.Lock()
	learned := g.learned[t.Key()]
	g.mu.Unlock()
	switch {
	case t.MaxContext > 0 && learned > 0:
		return min(t.MaxContext, learned)
	case learned > 0:
		return learned
	default:
		return t.MaxContext
	}
}

func (g *Gateway) learnContext(t config.Target, n int) {
	if n <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if cur, ok := g.learned[t.Key()]; !ok || n < cur {
		g.learned[t.Key()] = n
		g.log.Info("learned target context size", "target", t.Key(), "n_ctx", n)
	}
}

// markServed updates last-served state and emits a switch event when a route changes target.
func (g *Gateway) markServed(route, session, target string, fallback bool, reason, firstTried string) {
	s := Served{Route: route, LastTarget: target, LastAt: g.now(), Fallback: fallback, Reason: reason}
	g.mu.Lock()
	prev, had := g.byRoute[route]
	g.byRoute[route] = s
	if session != "" {
		if _, ok := g.bySess[session]; !ok {
			g.sessList = append(g.sessList, session)
			if len(g.sessList) > maxSessions {
				delete(g.bySess, g.sessList[0])
				g.sessList = g.sessList[1:]
			}
		}
		g.bySess[session] = s
	}
	g.mu.Unlock()

	from := prev.LastTarget
	if !had && fallback {
		from = firstTried
	}
	if from != "" && from != target {
		g.hub.Emit(events.Event{Kind: events.KindSwitch, Route: route, From: from, To: target, Reason: reason})
	}
}

// Routes returns the last-served state per route, sorted by route.
func (g *Gateway) Routes() []Served {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Served, 0, len(g.byRoute))
	for _, s := range g.byRoute {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Route < out[j].Route })
	return out
}

// Session returns what last served a client session (Claude Code / Codex session id).
func (g *Gateway) Session(id string) (Served, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, ok := g.bySess[id]
	return s, ok
}

// Counters reports translation downgrade and rejection totals (dashboard health line).
func (g *Gateway) Counters() (downgraded, rejected int64) {
	return g.downgraded.Load(), g.rejectedTypes.Load()
}

// noteDowngraded logs each kind of downgraded client feature once per process.
// Kinds may carry client-chosen text, so each is truncated and the set of
// remembered kinds is bounded; past the bound, new kinds are not logged.
// Callers count downgrades themselves (g.downgraded).
func (g *Gateway) noteDowngraded(kinds []string) {
	g.dgMu.Lock()
	defer g.dgMu.Unlock()
	for _, k := range kinds {
		k = truncateKind(k)
		if _, seen := g.seenDowngrade[k]; seen {
			continue
		}
		if len(g.seenDowngrade) >= maxDowngradeKinds {
			if !g.dgCapped {
				g.dgCapped = true
				g.log.Info("further downgrade kinds not logged", "limit", maxDowngradeKinds)
			}
			continue
		}
		if g.seenDowngrade == nil {
			g.seenDowngrade = map[string]struct{}{}
		}
		g.seenDowngrade[k] = struct{}{}
		g.log.Info("feature downgraded (not supported upstream, or upstream output dropped)", "feature", k)
	}
}

// truncateKind cuts k to maxDowngradeKind bytes on a rune boundary, marking the cut.
func truncateKind(k string) string {
	if len(k) <= maxDowngradeKind {
		return k
	}
	n := maxDowngradeKind
	for n > 0 && !utf8.RuneStart(k[n]) {
		n--
	}
	return k[:n] + "…"
}

// Health exposes the health state (for status endpoints).
func (g *Gateway) Health() *router.Health { return g.health }

// Models returns route aliases followed by discovered "provider:model" ids.
func (g *Gateway) Models() []string {
	rt := g.rt.Load()
	out := rt.cfg.RouteNames()
	g.mu.Lock()
	defer g.mu.Unlock()
	var ids []string
	for p, ms := range g.models {
		if _, ok := rt.ups[p]; !ok {
			continue
		}
		for _, m := range ms {
			ids = append(ids, p+":"+m)
		}
	}
	sort.Strings(ids)
	return append(out, ids...)
}

// ModelInfo describes one model id the gateway exposes, for clients that want metadata (Codex).
type ModelInfo struct {
	ID                string
	ContextWindow     int // smallest known context among the route's targets; 0 = unknown
	SupportsReasoning bool
}

// ModelInfos returns metadata for every id in Models(). A route's context window is its
// smallest target context, so a client never plans for more than a fallback can take.
func (g *Gateway) ModelInfos() []ModelInfo {
	rt := g.rt.Load()
	var out []ModelInfo
	for _, id := range g.Models() {
		targets := rt.cfg.Routes[id]
		if targets == nil {
			if _, ts, ok := rt.resolve(id); ok {
				targets = ts
			}
		}
		mi := ModelInfo{ID: id}
		for _, t := range targets {
			if c := g.contextFor(t); c > 0 && (mi.ContextWindow == 0 || c < mi.ContextWindow) {
				mi.ContextWindow = c
			}
			if t.Reasoning != "" && t.Reasoning != "none" {
				mi.SupportsReasoning = true
			}
		}
		out = append(out, mi)
	}
	return out
}

// Discover refreshes model lists once; failures keep the previous list.
func (g *Gateway) Discover(ctx context.Context) {
	rt := g.rt.Load()
	var wg sync.WaitGroup
	for id, u := range rt.ups {
		p := u.Config()
		// Passthrough providers are never called on cbrt's own behalf: they only
		// ever see requests a client sent, with the client's credential.
		if !p.IsEnabled() || (rt.cfg.Offline && p.Location == "cloud") || p.Passthrough() {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			ids, err := u.Models(cctx)
			if err != nil {
				g.log.Warn("model discovery failed", "provider", id, "err", events.Redact(err.Error()))
				return
			}
			g.mu.Lock()
			g.models[id] = ids
			g.mu.Unlock()
		}()
	}
	wg.Wait()
}

// RunDiscovery refreshes model lists every interval until ctx ends.
func (g *Gateway) RunDiscovery(ctx context.Context, every time.Duration) {
	g.Discover(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.Discover(ctx)
		}
	}
}

func (g *Gateway) newRequestID() string {
	return fmt.Sprintf("req_%x_%d", g.now().UnixMilli(), g.reqSeq.Add(1))
}
