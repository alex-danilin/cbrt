package projects

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/alex-danilin/cbrt/internal/hostcheck"
)

//go:embed page.html
var pageHTML string

var pageTmpl = template.Must(template.New("page").Parse(pageHTML))

// card is one project on the page.
type card struct {
	Name, Path       string
	Goal, Now        string
	Milestones       []milestoneView
	MsShown          []milestoneView // up to msShown unfinished milestones
	MsHidden         []milestoneView // the others, folded
	Next             []taskView      // the next 3 tasks to do
	More             []taskView      // the other tasks to do
	Parked           []taskView
	Hash             string // PlanHash of the PLAN.md the card shows
	Commit           string // "3d ago: subject"
	Verdict, Checked string
	VerdictClass     string
	Checking         string // a check is running: "claude, started 2m ago"
	CheckError       string // the last check from this page failed
	Flags            []string
	Public           bool // a public repository: the plan stays local

	Done, Total, Percent int    // progress of the whole plan
	Ring                 string // stroke-dasharray of the progress ring
	Active               bool   // Now names a task (not "nothing")
	Spark                chart  // commits per week
	Commits              int    // commits in the charted weeks
}

// chart is a line chart drawn server side as SVG path data (the page runs
// no scripts).
type chart struct {
	Line, Area string  // path data; Area closes the line to the bottom
	EndX, EndY float64 // the newest point
	Labels     []axisLabel
	Has        bool // at least one point is above zero
}

type axisLabel struct {
	X    float64
	Text string
}

// Chart sizes (SVG viewBox units).
const (
	sparkW, sparkH = 240.0, 48.0
	heroW, heroH   = 600.0, 140.0
	ringC          = 113.1 // circumference of the progress ring (r = 18)
)

// lineChart draws counts across a w×h box with pad units of margin.
func lineChart(counts []int, w, h, pad float64) chart {
	var c chart
	if len(counts) < 2 {
		return c
	}
	top := 1
	for _, n := range counts {
		top = max(top, n)
		c.Has = c.Has || n > 0
	}
	step := (w - 2*pad) / float64(len(counts)-1)
	var line strings.Builder
	for i, n := range counts {
		x := pad + float64(i)*step
		y := h - pad - float64(n)/float64(top)*(h-2*pad)
		cmd := "L"
		if i == 0 {
			cmd = "M"
		}
		fmt.Fprintf(&line, "%s%.1f %.1f ", cmd, x, y)
		c.EndX, c.EndY = x, y
	}
	c.Line = strings.TrimSpace(line.String())
	c.Area = fmt.Sprintf("%s L%.1f %.1f L%.1f %.1f Z", c.Line, c.EndX, h, pad, h)
	return c
}

// weekLabels names every third week of a chart ending at now ("Sep 1").
func weekLabels(n int, w, pad float64, now time.Time) []axisLabel {
	var out []axisLabel
	step := (w - 2*pad) / float64(n-1)
	for i := 0; i < n; i += 3 {
		start := now.Add(-time.Duration(n-i) * 7 * 24 * time.Hour)
		out = append(out, axisLabel{X: pad + float64(i)*step, Text: start.Format("Jan 2")})
	}
	return append(out, axisLabel{X: w - pad, Text: "now"})
}

// summary is the overview above the cards.
type summary struct {
	Projects, Done, Total, Percent  int
	Active, Stale, Checked, OnTrack int
	Commits, LastWeek               int
	Trend                           string // last week against the week before: "+40%", "−20%", "new", ""
	Ring                            string // stroke-dasharray of the overall progress ring
	Down                            bool   // fewer commits last week than the week before
	Activity                        chart
}

func summarize(cs []card, ss []Status, now time.Time) summary {
	sm := summary{Projects: len(cs)}
	weeks := make([]int, ActivityWeeks)
	for i, c := range cs {
		sm.Done += c.Done
		sm.Total += c.Total
		if c.Active {
			sm.Active++
		}
		if ss[i].Stale(now) {
			sm.Stale++
		}
		if c.Verdict != "" {
			sm.Checked++
			if c.Verdict == OnTrack {
				sm.OnTrack++
			}
		}
		for j, n := range ss[i].Weekly {
			weeks[j] += n
			sm.Commits += n
		}
	}
	if sm.Total > 0 {
		sm.Percent = sm.Done * 100 / sm.Total
	}
	sm.Ring = fmt.Sprintf("%.1f %.1f", ringC*float64(sm.Percent)/100, ringC)
	last, prev := weeks[ActivityWeeks-1], weeks[ActivityWeeks-2]
	sm.LastWeek = last
	switch {
	case prev > 0:
		d := (last - prev) * 100 / prev
		sign := "+"
		if d < 0 {
			sign, d = "−", -d
			sm.Down = true
		}
		sm.Trend = sign + strconv.Itoa(d) + "%"
	case last > 0:
		sm.Trend = "new"
	}
	sm.Activity = lineChart(weeks, heroW, heroH, 12)
	sm.Activity.Labels = weekLabels(ActivityWeeks, heroW, 12, now)
	return sm
}

// taskView is a task with what its park toggle posts.
type taskView struct {
	Line      int
	Text      string // the task text as in PLAN.md (the toggle matches it)
	Display   string
	CanToggle bool // open, active or parked
}

// PublicMarker marks a public repository's project in list and on the page.
const PublicMarker = "public (plan local)"

type milestoneView struct {
	Name        string
	Done, Total int
	Percent     int
}

type pageData struct {
	Cards     []card
	Generated string
	Empty     bool
	Token     string
	Agents    []string
	Summary   summary
}

func (pageData) PublicMarker() string { return PublicMarker }

// toggleForm is what one park/unpark form posts.
type toggleForm struct {
	Token, Project, Hash string
	Task                 taskView
	Label, Title         string
}

// Toggle is the form of task t on card c; label is "park" or "unpark".
func (d pageData) Toggle(c card, t taskView, label string) toggleForm {
	title := "mark it [~] in PLAN.md: switched off, left out of progress"
	if label == "unpark" {
		title = "mark it [ ] in PLAN.md again"
	}
	return toggleForm{Token: d.Token, Project: c.Name, Hash: c.Hash, Task: t, Label: label, Title: title}
}

// nextShown is how many tasks to do a card lists before "more";
// msShown is how many unfinished milestones it shows unfolded.
const (
	nextShown = 3
	msShown   = 4
)

func cards(ss []Status, now time.Time) []card {
	out := make([]card, 0, len(ss))
	for i := range ss {
		s := &ss[i]
		c := card{Name: s.Name, Path: s.Path, Public: s.Visibility == Public, Hash: s.PlanHash}
		switch {
		case s.Missing:
			c.Flags = append(c.Flags, "folder missing")
		case s.NoPlan:
			c.Flags = append(c.Flags, "no PLAN.md")
		case s.PlanError != "":
			c.Flags = append(c.Flags, "PLAN.md unparsable: "+s.PlanError)
		}
		if s.Stale(now) {
			c.Flags = append(c.Flags, "stale: last commit "+Age(now, s.CommitTime))
		}
		if s.RemoteAdded {
			c.Flags = append(c.Flags, "remote added since connect: run connect again (PLAN.md may be committed)")
		}
		if s.Weekly != nil {
			c.Spark = lineChart(s.Weekly, sparkW, sparkH, 4)
			for _, n := range s.Weekly {
				c.Commits += n
			}
		}
		c.Ring = fmt.Sprintf("0 %.1f", ringC)
		if p := s.Plan; p != nil {
			c.Goal, c.Now = p.Goal, p.Now
			now := strings.ToLower(strings.TrimSpace(p.Now))
			c.Active = now != "" && !strings.HasPrefix(now, "nothing")
			pr := p.Progress()
			c.Done, c.Total, c.Percent = pr.Done, pr.Total, pr.Percent()
			c.Ring = fmt.Sprintf("%.1f %.1f", ringC*float64(c.Percent)/100, ringC)
			for _, m := range p.Milestones {
				pr := m.Progress()
				name := m.Name
				if name == "" {
					name = "(no milestone)"
				}
				v := milestoneView{Name: name, Done: pr.Done, Total: pr.Total, Percent: pr.Percent()}
				c.Milestones = append(c.Milestones, v)
				// the card shows the first unfinished milestones; finished
				// ones and the rest fold away
				if pr.Done < pr.Total && len(c.MsShown) < msShown {
					c.MsShown = append(c.MsShown, v)
				} else {
					c.MsHidden = append(c.MsHidden, v)
				}
			}
			for i, t := range p.Next(1 << 30) {
				v := taskView{Line: t.Line, Text: t.Text, Display: t.Text, CanToggle: t.State != Unknown}
				switch t.State {
				case Active:
					v.Display = "▶ " + t.Text
				case Unknown:
					v.Display = "[" + t.Marker + "] " + t.Text
				}
				if i < nextShown {
					c.Next = append(c.Next, v)
				} else {
					c.More = append(c.More, v)
				}
			}
			for _, m := range p.Milestones {
				for _, t := range m.Tasks {
					if t.State == Parked {
						c.Parked = append(c.Parked, taskView{Line: t.Line, Text: t.Text, Display: t.Text, CanToggle: true})
					}
				}
			}
		}
		if !s.CommitTime.IsZero() {
			c.Commit = Age(now, s.CommitTime) + ": " + s.CommitSubject
		}
		if s.Verdict != "" {
			c.Verdict = s.Verdict
			c.Checked = Age(now, s.VerdictTime)
			c.VerdictClass = strings.ReplaceAll(s.Verdict, " ", "-")
		}
		out = append(out, c)
	}
	return out
}

// ServerOptions configure the page server.
type ServerOptions struct {
	// Load reads the project list; it is called on every request, so
	// projects connected while the page runs show up on reload.
	Load  func() ([]Entry, error)
	Now   func() time.Time
	Log   *slog.Logger
	Token string // the form token; "" = a random one per server
	// Run and CheckTimeout run the direction checks the page starts;
	// nil and 0 = ExecRun and DefaultAgentTimeout.
	Run          RunFunc
	CheckTimeout time.Duration
}

// Server serves the overview page at "/" and its two form endpoints,
// POST /api/park and POST /api/check. It answers only requests whose Host
// header names a loopback host (DNS rebinding). A POST must also come from
// the page itself: its Origin (or, without one, its Referer) must be this
// server's own http://host:port, and it must carry the per-server random
// token the page embeds in its forms.
type Server struct {
	o  ServerOptions
	mu sync.Mutex
	// checks by project path: a running or failed check started here
	checks map[string]*checkRun
	wg     sync.WaitGroup // running checks (tests wait for them)
}

type checkRun struct {
	agent   string
	started time.Time
	running bool
	err     string
}

// NewServer returns the page server.
func NewServer(o ServerOptions) *Server {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Token == "" {
		b := make([]byte, 32)
		rand.Read(b) // never fails (crypto/rand)
		o.Token = hex.EncodeToString(b)
	}
	return &Server{o: o, checks: map[string]*checkRun{}}
}

// Handler serves the page with the default options (a random token, real
// agent CLIs for checks).
func Handler(load func() ([]Entry, error), now func() time.Time, log *slog.Logger) http.Handler {
	return NewServer(ServerOptions{Load: load, Now: now, Log: log})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !hostcheck.LoopbackHeader(r.Host) {
		http.Error(w, "host not allowed", http.StatusMisdirectedRequest)
		return
	}
	switch r.URL.Path {
	case "/":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "the page only answers GET; forms post to /api/park and /api/check", http.StatusMethodNotAllowed)
			return
		}
		s.page(w, r)
	case "/api/park", "/api/check":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if !sameOrigin(r) {
			http.Error(w, "forbidden: the request does not come from this page (Origin/Referer)", http.StatusForbidden)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("token")), []byte(s.o.Token)) != 1 {
			http.Error(w, "forbidden: bad or missing form token; reload the page", http.StatusForbidden)
			return
		}
		e, ok := s.entry(w, r.PostForm.Get("project"))
		if !ok {
			return
		}
		if r.URL.Path == "/api/park" {
			s.park(w, r, e)
		} else {
			s.check(w, r, e)
		}
	default:
		http.NotFound(w, r)
	}
}

// sameOrigin reports a request whose Origin, or else Referer, is this
// server's own origin (http://<Host header>). A request with neither, or
// with Origin "null", is refused.
func sameOrigin(r *http.Request) bool {
	want := "http://" + r.Host
	if o := r.Header.Get("Origin"); o != "" {
		return strings.EqualFold(o, want)
	}
	ref := r.Header.Get("Referer")
	if ref == "" {
		return false
	}
	u, err := url.Parse(ref)
	return err == nil && u.Scheme == "http" && strings.EqualFold(u.Host, r.Host)
}

// entry finds the project a form names.
func (s *Server) entry(w http.ResponseWriter, name string) (Entry, bool) {
	es, err := s.o.Load()
	if err != nil {
		s.o.Log.Error("project list", "err", err)
		http.Error(w, "cannot read the project list: "+err.Error(), http.StatusInternalServerError)
		return Entry{}, false
	}
	for _, e := range es {
		if e.Name == name {
			return e, true
		}
	}
	http.Error(w, fmt.Sprintf("no connected project %q", Clean(name)), http.StatusNotFound)
	return Entry{}, false
}

func (s *Server) park(w http.ResponseWriter, r *http.Request, e Entry) {
	line, err := strconv.Atoi(r.PostForm.Get("line"))
	if err != nil || line < 1 {
		http.Error(w, "bad line number", http.StatusBadRequest)
		return
	}
	text := r.PostForm.Get("text")
	marker, err := Toggle(e.Path, line, text, r.PostForm.Get("hash"))
	var te *ToggleError
	switch {
	case errors.As(err, &te):
		http.Error(w, te.Msg, te.Code)
		return
	case err != nil:
		s.o.Log.Error("park", "project", e.Name, "err", err)
		http.Error(w, "could not change PLAN.md: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.o.Log.Info("park", "project", e.Name, "line", line, "marker", marker)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) check(w http.ResponseWriter, r *http.Request, e Entry) {
	agent := r.PostForm.Get("agent")
	if err := validAgent(agent); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	if c := s.checks[e.Path]; c != nil && c.running {
		s.mu.Unlock()
		http.Error(w, fmt.Sprintf("a check of %s is already running (%s)", Clean(e.Name), c.agent), http.StatusConflict)
		return
	}
	run := &checkRun{agent: agent, started: s.o.Now(), running: true}
	s.checks[e.Path] = run
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		res, err := CheckEntry(context.Background(), e, CheckOptions{Agent: agent, Timeout: s.o.CheckTimeout, Run: s.o.Run, Now: s.o.Now})
		s.mu.Lock()
		run.running = false
		if err != nil {
			run.err = err.Error()
		}
		s.mu.Unlock()
		if err != nil {
			s.o.Log.Error("check", "project", e.Name, "agent", agent, "err", err)
		} else {
			s.o.Log.Info("check", "project", e.Name, "agent", agent, "verdict", res.Verdict)
		}
	}()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	es, err := s.o.Load()
	if err != nil {
		s.o.Log.Error("project list", "err", err)
		http.Error(w, "cannot read the project list: "+err.Error(), http.StatusInternalServerError)
		return
	}
	t := s.o.Now()
	ss := Collect(r.Context(), es)
	cs := cards(ss, t)
	s.mu.Lock()
	for i := range cs {
		if c := s.checks[cs[i].Path]; c != nil {
			if c.running {
				cs[i].Checking = c.agent + ", started " + Age(t, c.started)
			} else {
				cs[i].CheckError = c.err
			}
		}
	}
	s.mu.Unlock()
	data := pageData{Cards: cs, Generated: t.Format("2006-01-02 15:04"), Empty: len(es) == 0, Token: s.o.Token, Agents: Agents,
		Summary: summarize(cs, ss, t)}
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, data); err != nil {
		s.o.Log.Error("render", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	// same-origin, not no-referrer: with no-referrer browsers send
	// "Origin: null" on the page's own form posts.
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
	w.Write(buf.Bytes())
}

// WriteList prints one line per project: name, Now, progress and the age
// of the last commit, plus any flags.
func WriteList(w io.Writer, ss []Status, now time.Time) error {
	if len(ss) == 0 {
		_, err := fmt.Fprintln(w, "no connected projects (cbrt projects connect <dir>)")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tNOW\tPROGRESS\tLAST COMMIT\tFLAGS")
	for i, c := range cards(ss, now) { // cards keeps the order of ss
		s := &ss[i]
		progress, nowText, commit := "-", "-", "-"
		if s.Plan != nil {
			p := s.Plan.Progress()
			progress = fmt.Sprintf("%d/%d (%d%%)", p.Done, p.Total, p.Percent())
			if s.Plan.Now != "" {
				nowText = clip(s.Plan.Now, 40)
			}
		}
		if !s.CommitTime.IsZero() {
			commit = Age(now, s.CommitTime)
		}
		// Text from the repository is cleaned of control characters (tabs
		// would also break the columns).
		flags := c.Flags
		if c.Public {
			flags = append([]string{PublicMarker}, flags...)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", Clean(c.Name), Clean(nowText), progress, commit, Clean(strings.Join(flags, "; ")))
	}
	return tw.Flush()
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// OpenBrowser asks the platform to open url in the default browser. It
// does not wait; a failure to start the opener is returned and is not
// worth more than a message.
func OpenBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// rundll32 takes the URL as one argument: no cmd.exe parsing of & or spaces.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
