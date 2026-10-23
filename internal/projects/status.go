package projects

import (
	"bufio"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// StaleAfter is how long without a commit makes a project stale.
const StaleAfter = 14 * 24 * time.Hour

// gitParallel bounds the git processes Collect runs at once.
const gitParallel = 8

// ChecksDir holds the AI check reports in a repository.
const ChecksDir = ".cbrt/checks"

// Status is what the page and `list` show for one project.
type Status struct {
	Entry
	Missing   bool   // the folder does not exist
	NoPlan    bool   // no PLAN.md
	PlanError string // PLAN.md could not be read or parsed
	Plan      *Plan

	CommitTime    time.Time // zero: no commit (or not a repository)
	CommitSubject string

	Verdict     string // first line of the newest check: on track | drifting | off track
	VerdictTime time.Time

	PlanTime time.Time // PLAN.md modification time
	PlanHash string    // PlanHash of the PLAN.md read: the park toggle's guard

	// RemoteAdded: connect found no remote (so the plan is committed like
	// any file), and the repository has one now: connect must re-check it.
	RemoteAdded bool

	// Weekly is the number of commits in each of the last ActivityWeeks
	// weeks, oldest first (nil: no repository).
	Weekly []int
}

// ActivityWeeks is how many weeks of commits the page charts.
const ActivityWeeks = 12

// Stale reports no commit for StaleAfter (only when there is a commit).
func (s *Status) Stale(now time.Time) bool {
	return !s.CommitTime.IsZero() && now.Sub(s.CommitTime) > StaleAfter
}

// LastActivity is the newest of the last commit and the PLAN.md change.
func (s *Status) LastActivity() time.Time {
	if s.PlanTime.After(s.CommitTime) {
		return s.PlanTime
	}
	return s.CommitTime
}

// Collect reads the status of every entry: PLAN.md, the newest check file
// and one git call each, git calls in parallel with a bound. The result is
// sorted by last activity, newest first, then by name.
func Collect(ctx context.Context, es []Entry) []Status {
	out := make([]Status, len(es))
	sem := make(chan struct{}, gitParallel)
	var wg sync.WaitGroup
	for i, e := range es {
		out[i] = readStatus(e)
		if out[i].Missing {
			continue
		}
		wg.Add(1)
		go func(s *Status) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.CommitTime, s.CommitSubject = lastCommit(ctx, s.Path)
			if !s.CommitTime.IsZero() {
				s.Weekly = weeklyCommits(ctx, s.Path, time.Now())
			}
			if s.Visibility == Private && strings.HasPrefix(s.VisibilityHow, noRemoteHow) {
				if r, err := gitCtx(ctx, s.Path, "remote"); err == nil && r != "" {
					s.RemoteAdded = true
				}
			}
		}(&out[i])
	}
	wg.Wait()
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].LastActivity(), out[j].LastActivity()
		if !a.Equal(b) {
			return a.After(b)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func readStatus(e Entry) Status {
	s := Status{Entry: e}
	st, err := os.Stat(e.Path)
	if err != nil || !st.IsDir() {
		s.Missing = true
		return s
	}
	plan := filepath.Join(e.Path, "PLAN.md")
	pst, err := os.Stat(plan)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.NoPlan = true
	case err != nil:
		s.PlanError = reason(err)
	case pst.Size() > MaxPlanBytes:
		s.PlanError = ErrTooLarge.Error()
	default:
		s.PlanTime = pst.ModTime()
		b, err := os.ReadFile(plan)
		if err != nil {
			s.PlanError = reason(err)
			break
		}
		s.PlanHash = PlanHash(b)
		if p, err := Parse(b); err != nil {
			s.PlanError = err.Error()
		} else {
			s.Plan = p
		}
	}
	s.Verdict, s.VerdictTime = latestCheck(filepath.Join(e.Path, filepath.FromSlash(ChecksDir)))
	return s
}

func reason(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// lastCommit is the time and subject of HEAD; zero when there is none.
func lastCommit(ctx context.Context, dir string) (time.Time, string) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := gitCtx(cctx, dir, "log", "-1", "--format=%ct%x09%s")
	if err != nil || out == "" {
		return time.Time{}, ""
	}
	ts, subject, _ := strings.Cut(out, "\t")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return time.Time{}, ""
	}
	return time.Unix(sec, 0), subject
}

// weeklyCommits counts the commits reachable from HEAD in each of the
// ActivityWeeks weeks before now, oldest first.
func weeklyCommits(ctx context.Context, dir string, now time.Time) []int {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	week := 7 * 24 * time.Hour
	since := now.Add(-ActivityWeeks * week)
	out, err := gitCtx(cctx, dir, "log", "--since="+strconv.FormatInt(since.Unix(), 10), "--format=%ct")
	if err != nil {
		return nil
	}
	return bucketWeeks(strings.Fields(out), now)
}

// bucketWeeks sorts unix timestamps into ActivityWeeks weekly counts ending
// at now, oldest first; timestamps outside the range are dropped.
func bucketWeeks(stamps []string, now time.Time) []int {
	week := 7 * 24 * time.Hour
	counts := make([]int, ActivityWeeks)
	for _, f := range stamps {
		sec, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			continue
		}
		age := now.Sub(time.Unix(sec, 0))
		if age < 0 {
			age = 0
		}
		if i := ActivityWeeks - 1 - int(age/week); i >= 0 && i < ActivityWeeks {
			counts[i]++
		}
	}
	return counts
}

// latestCheck reads the verdict line of the newest check report: names
// start with a UTC timestamp, so the greatest name is the newest.
func latestCheck(dir string) (string, time.Time) {
	newest := newestCheck(dir)
	if newest == "" {
		return "", time.Time{}
	}
	p := filepath.Join(dir, newest)
	f, err := os.Open(p)
	if err != nil {
		return "", time.Time{}
	}
	defer f.Close()
	var t time.Time
	if st, err := f.Stat(); err == nil {
		t = st.ModTime()
	}
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return "", t
	}
	line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), bom))
	v, ok := strings.CutPrefix(line, "Verdict:")
	if !ok {
		return "unreadable", t
	}
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case OnTrack, Drifting, OffTrack, VerdictUnknown:
		return v, t
	}
	return "unreadable", t
}

// newestCheck is the name of the newest report in the checks folder dir:
// names start with a UTC timestamp, so the greatest name is the newest.
func newestCheck(dir string) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var newest string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") && e.Name() > newest {
			newest = e.Name()
		}
	}
	return newest
}

// Age is a short human duration: "just now", "5m", "3h", "2d", "6w".
func Age(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h ago"
	case d < 14*24*time.Hour:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d ago"
	default:
		return strconv.Itoa(int(d/(7*24*time.Hour))) + "w ago"
	}
}
