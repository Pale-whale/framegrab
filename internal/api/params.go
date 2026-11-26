package api

import (
	"fmt"
	"math"
	"math/rand/v2"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/Pale-whale/framegrab/internal/media"
)

// at is one requested frame: a time in seconds, a share of the duration
// ("50%"), or a frame number.
type at struct {
	seconds  float64
	fraction float64
	frame    int
	kind     byte // 's', '%' or 'f'
	raw      string
}

type params struct {
	at                 []at
	random             int
	seed               uint64
	seeded             bool
	skipStart, skipEnd float64
	format             string
	quality            int
	width              int
	output             string
	cols               int
	file               int
	season, episode    int // -1 when not given
}

// values parses a query string leniently: "t=50%", typed by hand, keeps its
// "%" instead of being dropped as a broken escape.
func values(raw string) url.Values {
	q, _ := url.ParseQuery(badEscape.ReplaceAllString(raw, "%25"))
	return q
}

var badEscape = regexp.MustCompile(`%([^0-9A-Fa-f]|[0-9A-Fa-f][^0-9A-Fa-f]|[0-9A-Fa-f]?$)`)

// list splits repeated and comma-separated values: t=10&t=20 or t=10,20.
func list(q url.Values, key string) []string {
	var out []string
	for _, v := range q[key] {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// parseTime reads 5025.5, 83:45, 1:23:45.5 or 50%.
func parseTime(s string) (at, error) {
	if p, ok := strings.CutSuffix(s, "%"); ok {
		f, err := strconv.ParseFloat(p, 64)
		if err != nil || f < 0 || f >= 100 {
			return at{}, fmt.Errorf("t=%s: a share must be from 0%% to under 100%%", s)
		}
		return at{fraction: f / 100, kind: '%', raw: s}, nil
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return at{}, fmt.Errorf("t=%s: expected seconds, MM:SS or HH:MM:SS", s)
	}
	var total float64
	for i, p := range parts {
		f, err := strconv.ParseFloat(p, 64)
		if err != nil || f < 0 || math.IsInf(f, 0) || i > 0 && f >= 60 {
			return at{}, fmt.Errorf("t=%s: expected seconds, MM:SS or HH:MM:SS", s)
		}
		total = total*60 + f
	}
	return at{seconds: total, kind: 's', raw: s}, nil
}

// parseShare reads a share of the duration as 5% or 0.05.
func parseShare(q url.Values, key string, def float64) (float64, error) {
	s := q.Get(key)
	if s == "" {
		return def, nil
	}
	div := 1.0
	if p, ok := strings.CutSuffix(s, "%"); ok {
		s, div = p, 100
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f/div < 0 || f/div >= 0.5 {
		return 0, fmt.Errorf("%s=%s: expected a share under 50%%, like 5%% or 0.05", key, q.Get(key))
	}
	return f / div, nil
}

func parseInt(q url.Values, key string, def, lo, hi int) (int, error) {
	s := q.Get(key)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s=%s: expected a whole number from %d to %d", key, s, lo, hi)
	}
	return n, nil
}

func parseParams(q url.Values, maxFrames int) (p params, err error) {
	for _, s := range list(q, "t") {
		a, err := parseTime(s)
		if err != nil {
			return p, err
		}
		p.at = append(p.at, a)
	}
	for _, s := range list(q, "frame") {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return p, fmt.Errorf("frame=%s: expected a frame number from 0", s)
		}
		p.at = append(p.at, at{frame: n, kind: 'f', raw: s})
	}
	if p.random, err = parseInt(q, "random", 0, 0, maxFrames); err != nil {
		return p, err
	}
	if len(p.at) == 0 && p.random == 0 {
		return p, fmt.Errorf("ask for frames with t, frame or random")
	}
	if n := len(p.at) + p.random; n > maxFrames {
		return p, fmt.Errorf("%d frames asked, at most %d per request", n, maxFrames)
	}
	if s := q.Get("seed"); s != "" {
		if p.seed, err = strconv.ParseUint(s, 10, 64); err != nil {
			return p, fmt.Errorf("seed=%s: expected a positive whole number", s)
		}
		p.seeded = true
	} else {
		p.seed = rand.Uint64() >> 11 // fits a JSON number exactly
	}
	if p.skipStart, err = parseShare(q, "skip_start", 0.05); err != nil {
		return p, err
	}
	if p.skipEnd, err = parseShare(q, "skip_end", 0.10); err != nil {
		return p, err
	}
	p.format = strings.ToLower(q.Get("format"))
	if p.format == "" || p.format == "jpg" {
		p.format = "jpeg"
	}
	if _, ok := media.Formats[p.format]; !ok {
		return p, fmt.Errorf("format=%s: expected jpeg, png or webp", q.Get("format"))
	}
	if p.quality, err = parseInt(q, "quality", 85, 1, 100); err != nil {
		return p, err
	}
	if p.width, err = parseInt(q, "width", 0, 0, 7680); err != nil {
		return p, err
	}
	if p.cols, err = parseInt(q, "cols", 0, 0, 16); err != nil {
		return p, err
	}
	if p.file, err = parseInt(q, "file", 0, 0, 1000); err != nil {
		return p, err
	}
	if p.season, err = parseInt(q, "season", -1, 0, 10000); err != nil {
		return p, err
	}
	if p.episode, err = parseInt(q, "episode", -1, 0, 100000); err != nil {
		return p, err
	}
	p.output = strings.ToLower(q.Get("output"))
	n := len(p.at) + p.random
	switch p.output {
	case "":
		p.output = "image"
		if n > 1 {
			p.output = "zip"
		}
	case "image":
		if n > 1 {
			return p, fmt.Errorf("output=image returns one frame, %d asked: use zip, json or grid", n)
		}
	case "zip", "json", "grid":
	default:
		return p, fmt.Errorf("output=%s: expected image, zip, json or grid", q.Get("output"))
	}
	if p.output == "grid" && p.width == 0 {
		p.width = 480
	}
	return p, nil
}

// resolve turns a requested time into seconds from the start of the file.
// A frame number lands half a frame before that frame: ffmpeg returns the
// first frame at or after the time, so rounding cannot slip to the next one.
func (a at) resolve(info media.Info) (float64, error) {
	var s float64
	switch a.kind {
	case 's':
		s = a.seconds
	case '%':
		s = a.fraction * info.Duration
	case 'f':
		if info.FPS <= 0 {
			return 0, fmt.Errorf("frame=%s: the frame rate of this file is unknown, use t", a.raw)
		}
		if a.frame > 0 {
			s = (float64(a.frame) - 0.5) / info.FPS
		}
	}
	if a.kind == 'f' && info.FPS > 0 && a.frame >= int(math.Round(info.Duration*info.FPS)) {
		return 0, fmt.Errorf("frame=%s is past the end: the file has about %d frames",
			a.raw, int(math.Round(info.Duration*info.FPS)))
	}
	if s >= info.Duration {
		return 0, fmt.Errorf("%s is past the end: the file lasts %s", a.raw, timecode(info.Duration))
	}
	return s, nil
}

// frameAt is the number of the frame ffmpeg returns for a time.
func frameAt(s float64, info media.Info) int {
	return int(math.Ceil(s*info.FPS - 1e-6))
}

func timecode(s float64) string {
	ms := int64(math.Round(s * 1000))
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// window is where random frames are drawn: past the opening and before the
// credits, and never in the last two frames, which may not decode.
func (p params) window(info media.Info) (lo, hi float64) {
	lo, hi = info.Duration*p.skipStart, info.Duration*(1-p.skipEnd)
	if info.FPS > 0 {
		hi = min(hi, info.Duration-2/info.FPS)
	}
	if hi <= lo {
		return 0, info.Duration * 0.9
	}
	return lo, hi
}
