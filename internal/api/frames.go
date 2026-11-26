package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"

	"github.com/Pale-whale/framegrab/internal/library"
	"github.com/Pale-whale/framegrab/internal/media"
)

// A random frame darker than this is drawn again, at most retries times: a
// fade to black says nothing about the film.
const (
	blackLuma = 0.05
	retries   = 4
)

// target is one frame to extract.
type target struct {
	file    library.File
	info    media.Info
	seconds float64
	random  bool
	rng     *rand.Rand // random frames: where a retry draws again
	lo, hi  float64
	episode *library.Episode
}

// Frame is a frame's metadata, in the JSON and zip answers.
type Frame struct {
	Time     float64     `json:"time"`
	Timecode string      `json:"timecode"`
	Frame    int         `json:"frame"`
	Random   bool        `json:"random"`
	Episode  *episodeRef `json:"episode,omitempty"`
	File     string      `json:"file"`
	Format   string      `json:"format"`
	Width    int         `json:"width"`
	Height   int         `json:"height"`
	Data     []byte      `json:"data,omitempty"`
	img      *image.RGBA // kept for the grid only
}

type episodeRef struct {
	ID      string `json:"id"`
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
}

type item struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Title string `json:"title"`
	Year  int    `json:"year,omitempty"`
}

func (s *Server) movieFrames(w http.ResponseWriter, r *http.Request) {
	m := s.lib.Movie(r.PathValue("id"))
	if m == nil {
		writeError(w, http.StatusNotFound, "no movie with this id")
		return
	}
	p, err := parseParams(values(r.URL.RawQuery), s.maxFrames)
	if err != nil {
		fail(w, r, badRequest(err))
		return
	}
	if p.season >= 0 || p.episode >= 0 {
		fail(w, r, badRequest(errors.New("season and episode are for shows")))
		return
	}
	if p.file >= len(m.Files) {
		fail(w, r, badRequest(fmt.Errorf("file=%d: this movie has %d file(s)", p.file, len(m.Files))))
		return
	}
	f := m.Files[p.file]
	info, err := s.tool.Probe(r.Context(), f.Path)
	if err != nil {
		fail(w, r, err)
		return
	}
	var targets []target
	for _, a := range p.at {
		sec, err := a.resolve(info)
		if err != nil {
			fail(w, r, badRequest(err))
			return
		}
		targets = append(targets, target{file: f, info: info, seconds: sec})
	}
	// Random frames are spread: one in each of random equal slices of the
	// window, at a random place in its slice.
	lo, hi := p.window(info)
	slot := (hi - lo) / float64(max(p.random, 1))
	for i := range p.random {
		rng := rand.New(rand.NewPCG(p.seed, uint64(i)))
		t := target{file: f, info: info, random: true, rng: rng,
			lo: lo + float64(i)*slot, hi: lo + float64(i+1)*slot}
		t.seconds = t.draw()
		targets = append(targets, t)
	}
	s.answer(w, r, p, item{"movie", m.ID, m.Title, m.Year}, targets)
}

func (s *Server) showFrames(w http.ResponseWriter, r *http.Request) {
	sh := s.lib.Show(r.PathValue("id"))
	if sh == nil {
		writeError(w, http.StatusNotFound, "no show with this id")
		return
	}
	p, err := parseParams(values(r.URL.RawQuery), s.maxFrames)
	if err != nil {
		fail(w, r, badRequest(err))
		return
	}
	if p.file != 0 {
		fail(w, r, badRequest(errors.New("file is for movies: an episode keeps its largest file")))
		return
	}
	var eps []*library.Episode
	for i := range sh.Episodes {
		e := &sh.Episodes[i]
		if (p.season < 0 || e.Season == p.season) && (p.episode < 0 || e.Episode == p.episode) {
			eps = append(eps, e)
		}
	}
	if len(eps) == 0 {
		writeError(w, http.StatusNotFound, "no episode matches season and episode")
		return
	}
	var targets []target
	if len(p.at) > 0 {
		if len(eps) != 1 {
			fail(w, r, badRequest(fmt.Errorf(
				"t and frame need one episode: give season and episode (%d match)", len(eps))))
			return
		}
		info, err := s.tool.Probe(r.Context(), eps[0].File.Path)
		if err != nil {
			fail(w, r, err)
			return
		}
		for _, a := range p.at {
			sec, err := a.resolve(info)
			if err != nil {
				fail(w, r, badRequest(err))
				return
			}
			targets = append(targets, target{file: eps[0].File, info: info, seconds: sec, episode: eps[0]})
		}
	}
	// Each random frame comes from an episode drawn among those that match.
	for i := range p.random {
		rng := rand.New(rand.NewPCG(p.seed, uint64(i)))
		e := eps[rng.IntN(len(eps))]
		info, err := s.tool.Probe(r.Context(), e.File.Path)
		if err != nil {
			fail(w, r, fmt.Errorf("S%02dE%02d: %w", e.Season, e.Episode, err))
			return
		}
		lo, hi := p.window(info)
		t := target{file: e.File, info: info, random: true, rng: rng, lo: lo, hi: hi, episode: e}
		t.seconds = t.draw()
		targets = append(targets, t)
	}
	s.answer(w, r, p, item{"show", sh.ID, sh.Title, sh.Year}, targets)
}

func (t *target) draw() float64 { return t.lo + t.rng.Float64()*(t.hi-t.lo) }

// extract decodes and encodes one frame, drawing a random one again while it
// comes out black.
func (s *Server) extract(ctx context.Context, p params, t target) (Frame, error) {
	var img *image.RGBA
	for try := 0; ; try++ {
		var err error
		img, err = s.tool.Frame(ctx, t.file.Path, t.info, t.seconds, p.width)
		if err != nil {
			return Frame{}, err
		}
		if !t.random || try == retries || media.Luma(img) >= blackLuma {
			break
		}
		t.seconds = t.draw()
	}
	f := Frame{Time: math.Round(t.seconds*1000) / 1000, Timecode: timecode(t.seconds),
		Frame: frameAt(t.seconds, t.info), Random: t.random, File: t.file.Name, Format: p.format,
		Width: img.Rect.Dx(), Height: img.Rect.Dy()}
	if e := t.episode; e != nil {
		f.Episode = &episodeRef{e.ID, e.Season, e.Episode}
	}
	if p.output == "grid" {
		f.img = img
		return f, nil
	}
	var buf bytes.Buffer
	if err := s.tool.Encode(ctx, &buf, img, p.format, p.quality); err != nil {
		return Frame{}, err
	}
	f.Data = buf.Bytes()
	return f, nil
}

func (s *Server) answer(w http.ResponseWriter, r *http.Request, p params, it item, targets []target) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	frames := make([]Frame, len(targets))
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Go(func() {
			if frames[i], errs[i] = s.extract(ctx, p, t); errs[i] != nil {
				cancel()
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			fail(w, r, err)
			return
		}
	}
	if err := r.Context().Err(); err != nil {
		return
	}
	if p.random > 0 {
		w.Header().Set("X-Framegrab-Seed", strconv.FormatUint(p.seed, 10))
	}
	var err error
	switch p.output {
	case "image":
		err = writeImage(w, frames[0])
	case "json":
		writeJSON(w, http.StatusOK, struct {
			Item   item    `json:"item"`
			Seed   *uint64 `json:"seed,omitempty"`
			Frames []Frame `json:"frames"`
		}{it, seedOf(p), frames})
	case "zip":
		err = writeZip(w, it, p, frames)
	case "grid":
		err = s.writeGrid(r.Context(), w, p, frames)
	}
	if err != nil {
		fail(w, r, err)
	}
}

func seedOf(p params) *uint64 {
	if p.random == 0 {
		return nil
	}
	return &p.seed
}

// name is a frame's file name: its place in the answer, then its episode and time.
func name(i int, f Frame) string {
	ep := ""
	if f.Episode != nil {
		ep = fmt.Sprintf("S%02dE%02d_", f.Episode.Season, f.Episode.Episode)
	}
	tc := []byte(f.Timecode)
	for i, c := range tc {
		if c == ':' {
			tc[i] = '-'
		}
	}
	return fmt.Sprintf("%03d_%s%s.%s", i+1, ep, tc, media.Ext(f.Format))
}

func writeImage(w http.ResponseWriter, f Frame) error {
	h := w.Header()
	h.Set("Content-Type", media.Formats[f.Format])
	h.Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", name(0, f)))
	h.Set("X-Framegrab-Time", strconv.FormatFloat(f.Time, 'f', 3, 64))
	h.Set("X-Framegrab-Frame", strconv.Itoa(f.Frame))
	if f.Episode != nil {
		h.Set("X-Framegrab-Episode", fmt.Sprintf("S%02dE%02d", f.Episode.Season, f.Episode.Episode))
	}
	_, err := w.Write(f.Data)
	return err
}

// writeZip stores the images as they are (they are compressed already) and
// adds frames.json, the same metadata as the JSON answer without the images.
func writeZip(w http.ResponseWriter, it item, p params, frames []Frame) error {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", it.ID+".zip"))
	zw := zip.NewWriter(w)
	meta := make([]Frame, len(frames))
	for i, f := range frames {
		zf, err := zw.CreateHeader(&zip.FileHeader{Name: name(i, f), Method: zip.Store})
		if err != nil {
			return err
		}
		if _, err := zf.Write(f.Data); err != nil {
			return err
		}
		meta[i], meta[i].Data = f, nil
	}
	zf, err := zw.Create("frames.json")
	if err != nil {
		return err
	}
	enc := json.NewEncoder(zf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(struct {
		Item   item    `json:"item"`
		Seed   *uint64 `json:"seed,omitempty"`
		Frames []Frame `json:"frames"`
	}{it, seedOf(p), meta}); err != nil {
		return err
	}
	return zw.Close()
}

// writeGrid lays the frames out in time order of the request, cols wide (a
// square by default), each centred in a cell as large as the largest frame.
func (s *Server) writeGrid(ctx context.Context, w http.ResponseWriter, p params, frames []Frame) error {
	const gap = 4
	cols := p.cols
	if cols == 0 {
		cols = int(math.Ceil(math.Sqrt(float64(len(frames)))))
	}
	cols = min(cols, len(frames))
	rows := (len(frames) + cols - 1) / cols
	cw, ch := 0, 0
	for _, f := range frames {
		cw, ch = max(cw, f.Width), max(ch, f.Height)
	}
	grid := image.NewRGBA(image.Rect(0, 0, cols*cw+(cols+1)*gap, rows*ch+(rows+1)*gap))
	draw.Draw(grid, grid.Rect, image.Black, image.Point{}, draw.Src)
	for i, f := range frames {
		x := gap + i%cols*(cw+gap) + (cw-f.Width)/2
		y := gap + i/cols*(ch+gap) + (ch-f.Height)/2
		draw.Draw(grid, image.Rect(x, y, x+f.Width, y+f.Height), f.img, image.Point{}, draw.Src)
	}
	var buf bytes.Buffer
	if err := s.tool.Encode(ctx, &buf, grid, p.format, p.quality); err != nil {
		return err
	}
	w.Header().Set("Content-Type", media.Formats[p.format])
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", "grid."+media.Ext(p.format)))
	_, err := w.Write(buf.Bytes())
	return err
}
