// Package web serves the status pages, htmx fragments, SSE stream and health probes.
package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/jalet/pulumi-operator-ui/internal/events"
	"github.com/jalet/pulumi-operator-ui/internal/store"
)

const (
	_csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
		"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
	// The default page size, in state changes; every size in pageSizes is under
	// store.RunsPageMax.
	runsPageSize = 50
	readyTimeout = 2 * time.Second
)

// Reader is the subset of the store the pages read.
type Reader interface {
	ListStacks(ctx context.Context) ([]store.StackSummary, error)
	GetStack(ctx context.Context, namespace, name string) (store.StackSummary, error)
	ListTimeline(ctx context.Context, namespace, name string, before *store.Cursor,
		limit int) ([]store.Run, []store.Run, *store.Cursor, error)
	TimelineNewerAnchor(ctx context.Context, namespace, name string, after store.Cursor,
		limit int) (store.Cursor, bool, error)
	GetRun(ctx context.Context, id int64) (store.Run, error)
	StackStats(ctx context.Context, namespace, name string, f store.RunFilter,
		since time.Time) (store.StackStats, error)
	Ping(ctx context.Context) error
}

// Deps are the collaborators New wires together.
type Deps struct {
	Store       Reader
	Broker      *events.Broker
	RequireAuth func(http.Handler) http.Handler
	AuthRoutes  func(*http.ServeMux)
	Log         zerolog.Logger
	Now         func() time.Time
	S3Interval  time.Duration  // 0 = S3 history off
	Location    *time.Location // time zone for day headers; nil = UTC
	ThemeCSS    []byte         // color overrides from --theme-file; empty = built-in colors

	// heartbeat overrides the SSE heartbeat interval; zero means the default. Tests only.
	heartbeat time.Duration
}

type server struct {
	now        func() time.Time
	store      Reader
	broker     *events.Broker
	heartbeat  time.Duration
	s3Interval time.Duration
	loc        *time.Location
	log        zerolog.Logger
	pages      map[string]*template.Template // executed directly, for fragments
	bases      map[string]*template.Template // never executed; cloned per full page
}

type stackPage struct {
	Stack  store.StackSummary
	Days   []railDay
	Pager  pager
	Live   bool // latest page only: other pages do not auto-refresh
	Expand bool // ?previews=all: every preview is its own node
	Stats  store.StackStats
	S3On   bool // S3 history is enabled; a stored s3_error is stale otherwise
}

// statsWindow is how far back the stack header counts runs.
const statsWindow = 7 * 24 * time.Hour

// New returns the application handler.
func New(d Deps) http.Handler {
	if d.Store == nil || d.Broker == nil || d.RequireAuth == nil || d.AuthRoutes == nil ||
		d.Now == nil {
		panic("invariant violated: web.Deps is incomplete")
	}
	themeURL, themeVersion := "", ""
	if len(d.ThemeCSS) > 0 {
		sum := sha256.Sum256(d.ThemeCSS)
		themeVersion = hex.EncodeToString(sum[:])[:12]
		themeURL = "/static/theme.css?v=" + themeVersion
	}
	s := &server{now: d.Now, store: d.Store, broker: d.Broker, log: d.Log,
		pages: parsePages(d.Now, themeURL), bases: parsePages(d.Now, themeURL),
		heartbeat: d.heartbeat, s3Interval: d.S3Interval, loc: d.Location}
	if s.loc == nil {
		s.loc = time.UTC
	}
	if s.heartbeat == 0 {
		s.heartbeat = sseHeartbeatIntervalDefault
	}
	registerMetrics()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("GET /static/", staticHandler(assetVersions()))
	if themeURL != "" {
		css := d.ThemeCSS
		mux.HandleFunc("GET /static/theme.css", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			// Only this pod's version is immutable: during a rolling update a page from the
			// other pod may ask for its version here, which must not be cached as ours.
			if r.URL.Query().Get("v") == themeVersion {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			_, _ = w.Write(css)
		})
	}
	mux.Handle("GET /favicon.ico", faviconHandler())
	d.AuthRoutes(mux)

	protected := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, d.RequireAuth(h)) }
	protected("GET /{$}", s.index)
	protected("GET /fragments/stacks", s.stackRows)
	protected("GET /fragments/stacks/counters", s.stackCounters)
	protected("GET /fragments/stacks/{ns}/{name}", s.stackRow)
	protected("GET /fragments/stacks/{ns}/{name}/runs", s.stackRuns)
	protected("GET /stacks/{ns}/{name}", s.stackPage)
	protected("GET /runs/{id}", s.runPage)
	protected("GET /fragments/runs/{id}/row", s.runRow)
	protected("GET /fragments/runs/{id}/header", s.runHeader)
	protected("GET /fragments/runs/{id}/changes", s.runChanges)
	protected("GET /events", s.events)
	// A cheap authenticated probe: the layout fetches it on "session-expired", and an
	// expired session answers 401 with HX-Redirect to the login.
	protected("GET /fragments/session", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", _csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store") // static assets override this
		next.ServeHTTP(w, r)
	})
}

func staticHandler(versions map[string]string) http.Handler {
	sub, err := fs.Sub(_static, "static")
	if err != nil {
		panic("invariant violated: embedded static: " + err.Error())
	}
	files := http.StripPrefix("/static/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") { // no directory listings
			http.NotFound(w, r)
			return
		}
		// Only this build's version is immutable: during a rolling update a page from the
		// other pod may ask for its version here, which must not be cached as ours.
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		if v, ok := versions[name]; ok && r.URL.Query().Get("v") == v {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// faviconHandler answers the browser's unversioned /favicon.ico request, which comes before
// any sign-in, so it sits outside the auth wall. The URL is not versioned, so it is cached
// for a day rather than forever.
func faviconHandler() http.Handler {
	icon, err := _static.ReadFile("static/favicon.ico")
	if err != nil {
		panic("invariant violated: embedded favicon: " + err.Error())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(icon) // a client that went away is not an error worth logging
	})
}

func (s *server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		s.log.Warn().Err(err).Msg("readyz")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	stacks, err := s.store.ListStacks(r.Context())
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	s.render(w, r, "stacks", "layout", buildListPage(stacks, r.URL.Query().Get("ns")), http.StatusOK)
}

func (s *server) stackRows(w http.ResponseWriter, r *http.Request) {
	stacks, err := s.store.ListStacks(r.Context())
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	s.render(w, r, "stacks", "stack-overview", buildListPage(stacks, r.URL.Query().Get("ns")),
		http.StatusOK)
}

func (s *server) stackCounters(w http.ResponseWriter, r *http.Request) {
	stacks, err := s.store.ListStacks(r.Context())
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	s.render(w, r, "stacks", "stack-counters", buildListPage(stacks, r.URL.Query().Get("ns")),
		http.StatusOK)
}

// stackRow renders one list row. A deleted stack renders nothing, so the outerHTML swap
// removes it from the list.
func (s *server) stackRow(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.GetStack(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if st.DeletedAt != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.render(w, r, "stacks", "stack-row", st, http.StatusOK)
}

func (s *server) stackPage(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	q, ok := parseStackQuery(r.URL.Query())
	if !ok {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "The page cursor or size is invalid.")
		return
	}
	if q.After != nil {
		anchor, found, err := s.store.TimelineNewerAnchor(r.Context(), ns, name, *q.After, q.Limit)
		if err != nil {
			s.storeError(w, r, err)
			return
		}
		if !found {
			// Everything newer fits on the latest page, which is the live one.
			latest := stackQuery{Limit: q.Limit, Expand: q.Expand}
			http.Redirect(w, r, withQuery("/stacks/"+ns+"/"+name, latest.values()), http.StatusSeeOther)
			return
		}
		q.Before, q.After = &anchor, nil
	}
	page, err := s.loadStackPage(r.Context(), ns, name, q)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	s.render(w, r, "stack", "layout", page, http.StatusOK)
}

// stackRuns renders the latest page's rail for live updates; cursors are ignored.
func (s *server) stackRuns(w http.ResponseWriter, r *http.Request) {
	q, ok := parseStackQuery(r.URL.Query())
	if !ok {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "The page cursor or size is invalid.")
		return
	}
	page, err := s.loadStackPage(r.Context(), r.PathValue("ns"), r.PathValue("name"),
		stackQuery{Limit: q.Limit, Expand: q.Expand})
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	s.render(w, r, "stack", "rail", page, http.StatusOK)
}

func (s *server) loadStackPage(ctx context.Context, ns, name string,
	q stackQuery) (stackPage, error) {
	st, err := s.store.GetStack(ctx, ns, name)
	if err != nil {
		return stackPage{}, err
	}
	changes, previews, next, err := s.store.ListTimeline(ctx, ns, name, q.Before, q.Limit)
	if err != nil {
		return stackPage{}, err
	}
	stats, err := s.store.StackStats(ctx, ns, name, store.RunFilter{Types: store.StateChangeTypes},
		s.now().Add(-statsWindow))
	if err != nil {
		return stackPage{}, err
	}
	return stackPage{Stack: st, Days: buildRail(changes, previews, s.now(), s.loc, q.Expand),
		Pager: buildPager(ns, name, q, changes, next), Live: q.Before == nil,
		Expand: q.Expand, Stats: stats,
		S3On: s.s3Interval > 0}, nil
}

func (s *server) runPage(w http.ResponseWriter, r *http.Request) {
	s.withRun(w, r, func(run store.Run) {
		s.render(w, r, "run", "layout", s.runView(r, run), http.StatusOK)
	})
}

func (s *server) runRow(w http.ResponseWriter, r *http.Request) {
	s.withRun(w, r, func(run store.Run) {
		node := &railNode{Run: run, Drift: run.Type == store.RunTypePreview && drift(run)}
		s.render(w, r, "stack", "rail-node", node, http.StatusOK)
	})
}

func (s *server) runHeader(w http.ResponseWriter, r *http.Request) {
	s.withRun(w, r, func(run store.Run) {
		s.render(w, r, "run", "run-header", s.runView(r, run), http.StatusOK)
	})
}

// runChanges re-renders the changes panel only when its content changed: 204 tells htmx
// to keep what is on screen, including any diff the user opened.
func (s *server) runChanges(w http.ResponseWriter, r *http.Request) {
	s.withRun(w, r, func(run store.Run) {
		v := s.runView(r, run)
		if r.URL.Query().Get("v") == v.Version {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.render(w, r, "run", "run-changes", v, http.StatusOK)
	})
}

// runView builds the run page data. Whether history can still arrive depends on the
// Stack's backend; a failed Stack lookup only drops the waiting note.
func (s *server) runView(r *http.Request, run store.Run) runView {
	s3Stack := false
	if s.s3Interval > 0 {
		st, err := s.store.GetStack(r.Context(), run.Namespace, run.StackName)
		s3Stack = err == nil && strings.HasPrefix(st.BackendURL, "s3://")
	}
	return newRunView(run, s.s3Interval, s3Stack)
}

func (s *server) withRun(w http.ResponseWriter, r *http.Request, fn func(store.Run)) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such run.")
		return
	}
	run, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	fn(run)
}

func (s *server) storeError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "Not found", "It may have been deleted or pruned.")
		return
	}
	s.log.Error().Err(err).Msg("store read")
	s.renderError(w, r, http.StatusInternalServerError, "Something went wrong",
		"The database could not be read. Try again shortly.")
}
