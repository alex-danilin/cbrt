package main

import (
	"context"
	"os"
	"time"

	"github.com/alex-danilin/cbrt/internal/analyze"
	"github.com/alex-danilin/cbrt/internal/analyze/collect"
	"github.com/alex-danilin/cbrt/internal/analyze/report"
	"github.com/alex-danilin/cbrt/internal/projects"
)

// recentSessions lists the sessions of the last projects.SuggestWindow from
// every tool's transcripts, for the projects page's suggestions: tool,
// working folder and last event only. Subagents and the sessions the
// analyzer excludes (exclusions.json, the home folder) are left out. The
// analyzer's parse cache is used only when its folder exists, so the page
// never creates it.
func recentSessions(ctx context.Context) ([]projects.RecentSession, error) {
	home := analyze.Home()
	cfg, err := analyze.LoadConfig(home)
	if err != nil {
		return nil, err
	}
	_, statErr := os.Stat(home)
	now := time.Now()
	w := analyze.Window{Since: now.Add(-projects.SuggestWindow), Until: now}
	ds, err := collect.Collect(cfg, home, w, collect.Options{NoCache: statErr != nil})
	if err != nil {
		return nil, err
	}
	ex, err := report.LoadExclusions(home, *cfg.ExcludeHomeProject)
	if err != nil {
		return nil, err
	}
	var out []projects.RecentSession
	for _, s := range ds.Sessions {
		if s.Parent != nil || s.Project == "" || len(s.Events) == 0 || ex.Excluded(s) {
			continue
		}
		out = append(out, projects.RecentSession{Tool: s.Key.AdapterID, Dir: s.Project, Last: s.Events[len(s.Events)-1].TS})
	}
	return out, ctx.Err()
}
