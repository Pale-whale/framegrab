// Package library indexes films and series laid out the way Jellyfin and
// Plex expect: "Title (Year)/Title (Year).mkv" for films and
// "Show (Year)/Season 01/Show S01E02.mkv" for series.
package library

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// File is one video file. Path is absolute and never leaves the server.
type File struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Path string `json:"-"`
}

// Movie is a film folder (or a film file at the root of a library). Files are
// its versions, the largest first: that one is used unless asked otherwise.
type Movie struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Year   int    `json:"year,omitempty"`
	IDs    IDs    `json:"ids"`
	Folder string `json:"folder"`
	Files  []File `json:"files"`
	search string
}

// Episode is one episode file of a show.
type Episode struct {
	ID      string `json:"id"`
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
	File    File   `json:"file"`
}

// Show is a series folder and its episodes, sorted by season then episode.
type Show struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Year     int       `json:"year,omitempty"`
	IDs      IDs       `json:"ids"`
	Folder   string    `json:"folder"`
	Episodes []Episode `json:"episodes"`
	search   string
}

// Library holds the last complete scan of its roots.
type Library struct {
	movieRoots, showRoots []string

	mu       sync.RWMutex
	movies   []*Movie
	shows    []*Show
	byID     map[string]any
	scanned  time.Time
	scanning sync.Mutex
}

func New(movieRoots, showRoots []string) *Library {
	return &Library{movieRoots: movieRoots, showRoots: showRoots}
}

// id is stable as long as the file or folder keeps its path.
func id(kind, root, rel string) string {
	h := sha256.Sum256([]byte(kind + "\x00" + root + "\x00" + rel))
	return hex.EncodeToString(h[:6])
}

// Scan walks every root and replaces the index in one go, so a request never
// sees half a scan. A root that cannot be read is logged and skipped.
func (l *Library) Scan(ctx context.Context) {
	l.scanning.Lock()
	defer l.scanning.Unlock()
	start := time.Now()
	var movies []*Movie
	var shows []*Show
	for _, root := range l.movieRoots {
		movies = append(movies, scanMovies(ctx, root)...)
	}
	for _, root := range l.showRoots {
		shows = append(shows, scanShows(ctx, root)...)
	}
	if ctx.Err() != nil {
		return
	}
	slices.SortFunc(movies, func(a, b *Movie) int {
		return cmp.Or(cmp.Compare(normalize(a.Title), normalize(b.Title)), cmp.Compare(a.Year, b.Year))
	})
	slices.SortFunc(shows, func(a, b *Show) int {
		return cmp.Or(cmp.Compare(normalize(a.Title), normalize(b.Title)), cmp.Compare(a.Year, b.Year))
	})
	byID := make(map[string]any)
	episodes := 0
	for _, m := range movies {
		byID[m.ID] = m
	}
	for _, s := range shows {
		byID[s.ID] = s
		episodes += len(s.Episodes)
	}
	l.mu.Lock()
	l.movies, l.shows, l.byID, l.scanned = movies, shows, byID, time.Now()
	l.mu.Unlock()
	slog.Info("library scanned", "movies", len(movies), "shows", len(shows),
		"episodes", episodes, "ms", time.Since(start).Milliseconds())
}

// Ready is true once a first scan has finished.
func (l *Library) Ready() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return !l.scanned.IsZero()
}

func scanMovies(ctx context.Context, root string) []*Movie {
	byDir := map[string]*Movie{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			slog.Warn("cannot read", "path", p, "err", err)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "@") {
			if d.IsDir() && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != root && isExtrasDir(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isVideo(name) || isExtrasFile(name) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		f := File{Name: name, Size: info.Size(), Path: p}
		dir := filepath.Dir(p)
		// A film file at the root of the library is a film of its own.
		key := dir
		if dir == root {
			key = p
		}
		m := byDir[key]
		if m == nil {
			rel, _ := filepath.Rel(root, key)
			m = &Movie{ID: id("movie", root, rel), Folder: rel}
			if dir == root {
				m.Title, m.Year, m.IDs = parseName(strings.TrimSuffix(name, filepath.Ext(name)))
			} else {
				m.Title, m.Year, m.IDs = parseName(filepath.Base(dir))
			}
			byDir[key] = m
		}
		m.Files = append(m.Files, f)
		return nil
	})
	if err != nil && ctx.Err() == nil {
		slog.Warn("cannot scan", "root", root, "err", err)
	}
	movies := make([]*Movie, 0, len(byDir))
	for key, m := range byDir {
		slices.SortFunc(m.Files, func(a, b File) int {
			return cmp.Or(cmp.Compare(b.Size, a.Size), cmp.Compare(a.Name, b.Name))
		})
		var nfos []string
		if key == filepath.Join(root, m.Folder) && !isVideo(key) {
			nfos = append(nfos, filepath.Join(key, "movie.nfo"))
		}
		f := m.Files[0].Path
		nfos = append(nfos, strings.TrimSuffix(f, filepath.Ext(f))+".nfo")
		aliases := []string{m.Title}
		if n, ok := readNFO(nfos...); ok {
			m.IDs.fill(n.ids())
			if m.Year == 0 {
				m.Year = n.Year
			}
			aliases = append(aliases, n.Title, n.OriginalTitle)
		}
		m.search = normalize(strings.Join(aliases, " "))
		movies = append(movies, m)
	}
	return movies
}

func scanShows(ctx context.Context, root string) []*Show {
	entries, err := os.ReadDir(root)
	if err != nil {
		slog.Warn("cannot scan", "root", root, "err", err)
		return nil
	}
	var shows []*Show
	for _, e := range entries {
		if ctx.Err() != nil {
			return nil
		}
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "@") {
			continue
		}
		if s := scanShow(ctx, root, e.Name()); len(s.Episodes) > 0 {
			shows = append(shows, s)
		}
	}
	return shows
}

func scanShow(ctx context.Context, root, folder string) *Show {
	dir := filepath.Join(root, folder)
	s := &Show{ID: id("show", root, folder), Folder: folder}
	s.Title, s.Year, s.IDs = parseName(folder)
	aliases := []string{s.Title}
	if n, ok := readNFO(filepath.Join(dir, "tvshow.nfo")); ok {
		s.IDs.fill(n.ids())
		if s.Year == 0 {
			s.Year = n.Year
		}
		aliases = append(aliases, n.Title, n.OriginalTitle)
	}
	s.search = normalize(strings.Join(aliases, " "))

	type key struct{ season, episode int }
	byNumber := map[key]Episode{}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if p != dir && (strings.HasPrefix(name, ".") || isExtrasDir(name)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isVideo(name) || isExtrasFile(name) {
			return nil
		}
		season, episode, ok := parseEpisode(name)
		if !ok {
			return nil
		}
		// The season folder wins over the file name: it is what Jellyfin uses.
		if n, ok := parseSeasonDir(filepath.Base(filepath.Dir(p))); ok {
			season = n
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		ep := Episode{ID: id("episode", root, rel), Season: season, Episode: episode,
			File: File{Name: name, Size: info.Size(), Path: p}}
		// Two files for one episode are two versions: keep the largest.
		k := key{season, episode}
		if old, dup := byNumber[k]; !dup || ep.File.Size > old.File.Size {
			byNumber[k] = ep
		}
		return nil
	})
	for _, ep := range byNumber {
		s.Episodes = append(s.Episodes, ep)
	}
	slices.SortFunc(s.Episodes, func(a, b Episode) int {
		return cmp.Or(cmp.Compare(a.Season, b.Season), cmp.Compare(a.Episode, b.Episode))
	})
	return s
}

// Query filters a search. Empty fields match everything.
type Query struct {
	Text             string
	Year             int
	IMDb, TMDb, TVDb string
}

func (q Query) match(search string, year int, ids IDs) bool {
	if q.Year != 0 && q.Year != year {
		return false
	}
	if q.IMDb != "" && !strings.EqualFold(q.IMDb, ids.IMDb) ||
		q.TMDb != "" && q.TMDb != ids.TMDb ||
		q.TVDb != "" && q.TVDb != ids.TVDb {
		return false
	}
	words := strings.Fields(normalize(q.Text))
	have := strings.Fields(search)
	for _, w := range words {
		if !slices.ContainsFunc(have, func(h string) bool { return strings.HasPrefix(h, w) }) {
			return false
		}
	}
	return true
}

func (l *Library) Movies(q Query) []*Movie {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []*Movie
	for _, m := range l.movies {
		if q.match(m.search, m.Year, m.IDs) {
			out = append(out, m)
		}
	}
	return out
}

func (l *Library) Shows(q Query) []*Show {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []*Show
	for _, s := range l.shows {
		if q.match(s.search, s.Year, s.IDs) {
			out = append(out, s)
		}
	}
	return out
}

func (l *Library) Movie(id string) *Movie {
	l.mu.RLock()
	defer l.mu.RUnlock()
	m, _ := l.byID[id].(*Movie)
	return m
}

func (l *Library) Show(id string) *Show {
	l.mu.RLock()
	defer l.mu.RUnlock()
	s, _ := l.byID[id].(*Show)
	return s
}
