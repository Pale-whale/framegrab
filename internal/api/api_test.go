package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pale-whale/framegrab/internal/library"
	"github.com/Pale-whale/framegrab/internal/media"
)

const (
	fps    = 25
	nFrame = 30
	step   = 8 // frame n is grey n*step
)

// video writes a clip whose frame n is a flat grey of level n*step, encoded
// with keyframes 12 frames apart, so a frame that is off by one shows.
func video(t *testing.T, path string, extra ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	for n := range nFrame {
		raw.Write(bytes.Repeat([]byte{byte(n * step)}, 64*48))
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "rawvideo", "-pix_fmt", "gray",
		"-s", "64x48", "-r", fmt.Sprint(fps), "-i", "pipe:0", "-c:v", "mpeg4", "-q:v", "1", "-g", "12",
		"-pix_fmt", "yuv420p"}
	args = append(append(args, extra...), "-y", path)
	cmd := exec.Command("ffmpeg", args...)
	cmd.Stdin = &raw
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
}

func setup(t *testing.T) (*httptest.Server, *library.Library) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	dir := t.TempDir()
	video(t, filepath.Join(dir, "movies", "Grey (2020)", "Grey.2020.mkv"))
	video(t, filepath.Join(dir, "movies", "Wide (2021)", "Wide.2021.mkv"), "-vf", "setsar=2")
	video(t, filepath.Join(dir, "movies", "Bright (2022)", "Bright.2022.mkv"),
		"-vf", "setparams=color_trc=smpte2084:color_primaries=bt2020:colorspace=bt2020nc")
	video(t, filepath.Join(dir, "tv", "Show", "Season 1", "Show.S01E01.mkv"))
	video(t, filepath.Join(dir, "tv", "Show", "Season 1", "Show.S01E02.mkv"))
	lib := library.New([]string{filepath.Join(dir, "movies")}, []string{filepath.Join(dir, "tv")})
	lib.Scan(context.Background())
	tool := media.New("ffmpeg", "ffprobe", 2, 30*time.Second)
	srv := httptest.NewServer(New(lib, tool, 32, func() {}).Handler())
	t.Cleanup(srv.Close)
	return srv, lib
}

func get(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

// level is the grey level of a frame, which says which frame it is.
func level(t *testing.T, img image.Image) int {
	t.Helper()
	r, _, _, _ := img.At(img.Bounds().Dx()/2, img.Bounds().Dy()/2).RGBA()
	return int(math.Round(float64(r>>8) / step))
}

func decode(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return img
}

func movieID(t *testing.T, lib *library.Library, title string) string {
	t.Helper()
	m := lib.Movies(library.Query{Text: title})
	if len(m) != 1 {
		t.Fatalf("%d movies match %q", len(m), title)
	}
	return m[0].ID
}

func TestExactFrames(t *testing.T) {
	srv, lib := setup(t)
	id := movieID(t, lib, "grey")
	for q, want := range map[string]int{
		"frame=0": 0, "frame=1": 1, "frame=11": 11, "frame=12": 12, "frame=13": 13, "frame=29": 29,
		"t=0.68": 17, "t=0.7": 18, "t=00:00:00.400": 10, "t=50%": 15,
	} {
		resp, body := get(t, srv.URL+"/v1/movies/"+id+"/frames?format=png&"+q)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", q, resp.StatusCode, body)
		}
		if got := level(t, decode(t, body)); got != want {
			t.Errorf("%s: got frame %d, want %d", q, got, want)
		}
		if h := resp.Header.Get("X-Framegrab-Frame"); h != fmt.Sprint(want) {
			t.Errorf("%s: X-Framegrab-Frame = %s, want %d", q, h, want)
		}
	}
}

func TestOutputs(t *testing.T) {
	srv, lib := setup(t)
	base := srv.URL + "/v1/movies/" + movieID(t, lib, "grey") + "/frames"

	resp, body := get(t, base+"?frame=3,20&output=json")
	var js struct {
		Item   item    `json:"item"`
		Seed   *uint64 `json:"seed"`
		Frames []Frame `json:"frames"`
	}
	if err := json.Unmarshal(body, &js); err != nil || resp.StatusCode != 200 {
		t.Fatalf("json: %d %v %s", resp.StatusCode, err, body)
	}
	if js.Item.Title != "Grey" || js.Seed != nil || len(js.Frames) != 2 || js.Frames[1].Frame != 20 {
		t.Fatalf("json = %+v", js)
	}
	if got := level(t, decode(t, js.Frames[1].Data)); got != 20 {
		t.Errorf("json frame 20 shows frame %d", got)
	}

	resp, body = get(t, base+"?frame=3&frame=20")
	if resp.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("two frames: %s, want a zip", resp.Header.Get("Content-Type"))
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if len(names) != 3 || names[0] != "001_00-00-00.100.jpg" || names[2] != "frames.json" {
		t.Errorf("zip = %q", names)
	}

	resp, body = get(t, base+"?frame=1,2,3,4,5&output=grid&cols=3&width=32&format=png")
	img, err := png.Decode(bytes.NewReader(body))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("grid: %d %v", resp.StatusCode, err)
	}
	// 3 columns and 2 rows of 32x24 cells, 4 pixels apart.
	if b := img.Bounds(); b.Dx() != 3*32+4*4 || b.Dy() != 2*24+3*4 {
		t.Errorf("grid is %v", b)
	}

	resp, body = get(t, base+"?frame=7&format=webp")
	if resp.Header.Get("Content-Type") != "image/webp" || !bytes.HasPrefix(body, []byte("RIFF")) {
		t.Errorf("webp: %s %q", resp.Header.Get("Content-Type"), body[:min(len(body), 12)])
	}

	_, body = get(t, base+"?frame=7&width=32")
	if img, err := jpeg.Decode(bytes.NewReader(body)); err != nil || img.Bounds().Dx() != 32 || img.Bounds().Dy() != 24 {
		t.Errorf("width=32: %v %v", err, img.Bounds())
	}
}

func TestAspectAndHDR(t *testing.T) {
	srv, lib := setup(t)
	_, body := get(t, srv.URL+"/v1/movies/"+movieID(t, lib, "wide")+"/frames?frame=5&format=png")
	if b := decode(t, body).Bounds(); b.Dx() != 128 || b.Dy() != 48 {
		t.Errorf("anamorphic frame is %v, want 128x48", b)
	}
	id := movieID(t, lib, "bright")
	resp, body := get(t, srv.URL+"/v1/movies/"+id)
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte(`"hdr": true`)) {
		t.Fatalf("HDR movie: %d %s", resp.StatusCode, body)
	}
	resp, body = get(t, srv.URL+"/v1/movies/"+id+"/frames?frame=5")
	if resp.StatusCode != 200 {
		t.Fatalf("HDR frame: %d %s", resp.StatusCode, body)
	}
}

func TestRandom(t *testing.T) {
	srv, lib := setup(t)
	base := srv.URL + "/v1/movies/" + movieID(t, lib, "grey") + "/frames?output=json&random=4"
	frames := func(q string) ([]Frame, uint64) {
		resp, body := get(t, base+q)
		var js struct {
			Seed   uint64  `json:"seed"`
			Frames []Frame `json:"frames"`
		}
		if err := json.Unmarshal(body, &js); err != nil || resp.StatusCode != 200 {
			t.Fatalf("random: %d %v %s", resp.StatusCode, err, body)
		}
		if resp.Header.Get("X-Framegrab-Seed") != fmt.Sprint(js.Seed) {
			t.Errorf("seed header %s, body %d", resp.Header.Get("X-Framegrab-Seed"), js.Seed)
		}
		return js.Frames, js.Seed
	}
	a, seed := frames("")
	b, _ := frames(fmt.Sprintf("&seed=%d", seed))
	for i := range a {
		if a[i].Time != b[i].Time {
			t.Errorf("same seed, frame %d at %v then %v", i, a[i].Time, b[i].Time)
		}
		if !a[i].Random {
			t.Errorf("frame %d not marked random", i)
		}
		// Past the first 5% and before the last 10%, and in its own quarter.
		lo, hi := 1.2*(0.05+0.85*float64(i)/4), 1.2*(0.05+0.85*float64(i+1)/4)
		if a[i].Time < lo-0.001 || a[i].Time > hi+0.001 {
			t.Errorf("frame %d at %v, outside %v-%v", i, a[i].Time, lo, hi)
		}
	}
	// Frames 0 and 1 are near black (grey 0 and 8, under blackLuma): a random
	// frame is drawn again when it lands on them.
	// Fixed seeds keep the test from failing on an unlucky draw.
	for seed := range 5 {
		f, _ := frames(fmt.Sprintf("&skip_start=0&skip_end=0&seed=%d", seed))
		for _, fr := range f {
			if fr.Frame < 2 {
				t.Errorf("random frame %d is black", fr.Frame)
			}
		}
	}
}

func TestShows(t *testing.T) {
	srv, lib := setup(t)
	sh := lib.Shows(library.Query{Text: "show"})
	if len(sh) != 1 {
		t.Fatalf("%d shows", len(sh))
	}
	base := srv.URL + "/v1/shows/" + sh[0].ID
	resp, body := get(t, base+"/frames?season=1&episode=2&frame=9&format=png")
	if resp.StatusCode != 200 || resp.Header.Get("X-Framegrab-Episode") != "S01E02" || level(t, decode(t, body)) != 9 {
		t.Fatalf("S01E02 frame 9: %d %s", resp.StatusCode, resp.Header.Get("X-Framegrab-Episode"))
	}
	resp, body = get(t, base+"/frames?random=6&output=json&season=1")
	var js struct{ Frames []Frame }
	if err := json.Unmarshal(body, &js); err != nil || resp.StatusCode != 200 || len(js.Frames) != 6 {
		t.Fatalf("random episodes: %d %s", resp.StatusCode, body)
	}
	for _, f := range js.Frames {
		if f.Episode == nil || f.Episode.Season != 1 {
			t.Errorf("random frame without its episode: %+v", f)
		}
	}
	resp, body = get(t, srv.URL+"/v1/shows")
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte(`"episodes": 2`)) {
		t.Errorf("list shows: %s", body)
	}
}

func TestErrors(t *testing.T) {
	srv, lib := setup(t)
	movie := srv.URL + "/v1/movies/" + movieID(t, lib, "grey") + "/frames"
	show := srv.URL + "/v1/shows/" + lib.Shows(library.Query{})[0].ID + "/frames"
	for url, want := range map[string]int{
		movie:                                  400,
		movie + "?t=2":                         400, // the clip lasts 1.2 s
		movie + "?frame=30":                    400,
		movie + "?t=1:75":                      400,
		movie + "?t=100%":                      400,
		movie + "?frame=1,2&output=image":      400,
		movie + "?random=33":                   400,
		movie + "?frame=1&format=gif":          400,
		movie + "?frame=1&file=1":              400,
		movie + "?frame=1&season=1":            400,
		movie + "?skip_start=60%&random=1":     400,
		show + "?t=1":                          400, // which episode?
		show + "?season=9&random=1":            404,
		srv.URL + "/v1/movies/nope/frames?t=1": 404,
		srv.URL + "/v1/shows/nope":             404,
	} {
		resp, body := get(t, url)
		if resp.StatusCode != want {
			t.Errorf("%s: %d, want %d: %s", url, resp.StatusCode, want, body)
		}
	}
}
