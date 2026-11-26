// Package api serves the library and its frames over HTTP.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Pale-whale/framegrab/internal/library"
	"github.com/Pale-whale/framegrab/internal/media"
)

type Server struct {
	lib       *library.Library
	tool      *media.Tool
	maxFrames int
	rescan    func()
}

// New serves lib. rescan starts a scan in the background.
func New(lib *library.Library, tool *media.Tool, maxFrames int, rescan func()) *Server {
	return &Server{lib: lib, tool: tool, maxFrames: maxFrames, rescan: rescan}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.lib.Ready() {
			http.Error(w, "first scan running\n", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /v1/movies", s.ready(s.listMovies))
	mux.HandleFunc("GET /v1/movies/{id}", s.ready(s.getMovie))
	mux.HandleFunc("GET /v1/movies/{id}/frames", s.ready(s.movieFrames))
	mux.HandleFunc("GET /v1/shows", s.ready(s.listShows))
	mux.HandleFunc("GET /v1/shows/{id}", s.ready(s.getShow))
	mux.HandleFunc("GET /v1/shows/{id}/frames", s.ready(s.showFrames))
	mux.HandleFunc("POST /v1/rescan", func(w http.ResponseWriter, r *http.Request) {
		s.rescan()
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "scan started"})
	})
	return logRequests(mux)
}

func (s *Server) ready(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.lib.Ready() {
			writeError(w, http.StatusServiceUnavailable, "the first library scan is still running")
			return
		}
		h(w, r)
	}
}

// httpError carries the status a failed request answers with.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func badRequest(err error) error { return &httpError{http.StatusBadRequest, err.Error()} }

func fail(w http.ResponseWriter, r *http.Request, err error) {
	var he *httpError
	switch {
	case errors.As(err, &he):
		writeError(w, he.status, he.msg)
	case errors.Is(err, context.Canceled):
		// The client left; nobody reads the answer.
	default:
		slog.Error("request failed", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func query(r *http.Request) (library.Query, error) {
	q := r.URL.Query()
	lq := library.Query{Text: q.Get("q"), IMDb: q.Get("imdb"), TMDb: q.Get("tmdb"), TVDb: q.Get("tvdb")}
	if y := q.Get("year"); y != "" {
		n, err := strconv.Atoi(y)
		if err != nil {
			return lq, badRequest(errors.New("year: expected a year like 2010"))
		}
		lq.Year = n
	}
	return lq, nil
}

type page[T any] struct {
	Total int `json:"total"`
	Items []T `json:"items"`
}

func paginate[T any](r *http.Request, items []T) (page[T], error) {
	q := r.URL.Query()
	limit, err := parseInt(q, "limit", 50, 1, 1000)
	if err != nil {
		return page[T]{}, badRequest(err)
	}
	offset, err := parseInt(q, "offset", 0, 0, 1<<30)
	if err != nil {
		return page[T]{}, badRequest(err)
	}
	p := page[T]{Total: len(items), Items: []T{}}
	if offset < len(items) {
		p.Items = items[offset:min(len(items), offset+limit)]
	}
	return p, nil
}

func (s *Server) listMovies(w http.ResponseWriter, r *http.Request) {
	q, err := query(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	p, err := paginate(r, s.lib.Movies(q))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

type fileInfo struct {
	library.File
	Info  *media.Info `json:"info,omitempty"`
	Error string      `json:"error,omitempty"`
}

func (s *Server) getMovie(w http.ResponseWriter, r *http.Request) {
	m := s.lib.Movie(r.PathValue("id"))
	if m == nil {
		writeError(w, http.StatusNotFound, "no movie with this id")
		return
	}
	files := make([]fileInfo, len(m.Files))
	for i, f := range m.Files {
		files[i].File = f
		if info, err := s.tool.Probe(r.Context(), f.Path); err != nil {
			files[i].Error = err.Error()
		} else {
			files[i].Info = &info
		}
	}
	writeJSON(w, http.StatusOK, struct {
		*library.Movie
		Files []fileInfo `json:"files"`
	}{m, files})
}

type showSummary struct {
	ID       string      `json:"id"`
	Title    string      `json:"title"`
	Year     int         `json:"year,omitempty"`
	IDs      library.IDs `json:"ids"`
	Folder   string      `json:"folder"`
	Seasons  []int       `json:"seasons"`
	Episodes int         `json:"episodes"`
}

func summarize(sh *library.Show) showSummary {
	sum := showSummary{ID: sh.ID, Title: sh.Title, Year: sh.Year, IDs: sh.IDs,
		Folder: sh.Folder, Seasons: []int{}, Episodes: len(sh.Episodes)}
	for _, e := range sh.Episodes {
		if n := len(sum.Seasons); n == 0 || sum.Seasons[n-1] != e.Season {
			sum.Seasons = append(sum.Seasons, e.Season)
		}
	}
	return sum
}

func (s *Server) listShows(w http.ResponseWriter, r *http.Request) {
	q, err := query(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	shows := s.lib.Shows(q)
	sums := make([]showSummary, len(shows))
	for i, sh := range shows {
		sums[i] = summarize(sh)
	}
	p, err := paginate(r, sums)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) getShow(w http.ResponseWriter, r *http.Request) {
	sh := s.lib.Show(r.PathValue("id"))
	if sh == nil {
		writeError(w, http.StatusNotFound, "no show with this id")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		showSummary
		Episodes []library.Episode `json:"episodes"`
	}{summarize(sh), sh.Episodes})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(sw, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			return
		}
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "query", r.URL.RawQuery,
			"status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}
