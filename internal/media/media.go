// Package media probes video files and extracts frames from them with
// ffprobe and ffmpeg.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Info is what extraction needs to know about a file's video stream.
type Info struct {
	Duration float64 `json:"duration"`
	FPS      float64 `json:"fps"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	// DisplayWidth applies the sample aspect ratio: an anamorphic DVD is
	// stored 720 wide and shown 1024.
	DisplayWidth int    `json:"display_width"`
	Codec        string `json:"codec"`
	HDR          bool   `json:"hdr"`
	// DolbyVisionOnly marks a Dolby Vision profile 5 stream, which has no
	// HDR10 base layer: without a Dolby Vision decoder its colours are wrong.
	DolbyVisionOnly bool `json:"dolby_vision_only,omitempty"`
}

// Tool runs ffprobe and ffmpeg. Its zero value is not usable: use New.
type Tool struct {
	ffmpeg, ffprobe string
	slots           chan struct{}
	timeout         time.Duration

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	mod  time.Time
	size int64
	info Info
}

// New allows concurrency ffmpeg processes at once, across every request;
// each frame waits for a slot. A frame that takes longer than timeout fails.
func New(ffmpeg, ffprobe string, concurrency int, timeout time.Duration) *Tool {
	return &Tool{ffmpeg: ffmpeg, ffprobe: ffprobe, slots: make(chan struct{}, concurrency),
		timeout: timeout, cache: map[string]cached{}}
}

type probeOut struct {
	Streams []struct {
		CodecName    string `json:"codec_name"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		SAR          string `json:"sample_aspect_ratio"`
		AvgRate      string `json:"avg_frame_rate"`
		RRate        string `json:"r_frame_rate"`
		Transfer     string `json:"color_transfer"`
		Duration     string `json:"duration"`
		SideDataList []struct {
			Type      string `json:"side_data_type"`
			DVProfile int    `json:"dv_profile"`
		} `json:"side_data_list"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// Probe reads the first real video stream: cover art embedded in a file is
// also a video stream, and `V` skips it. The result is cached until the file
// changes.
func (t *Tool) Probe(ctx context.Context, path string) (Info, error) {
	st, err := os.Stat(path)
	if err != nil {
		return Info{}, err
	}
	t.mu.Lock()
	c, ok := t.cache[path]
	t.mu.Unlock()
	if ok && c.mod.Equal(st.ModTime()) && c.size == st.Size() {
		return c.info, nil
	}

	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.ffprobe, "-v", "error", "-select_streams", "V:0",
		"-show_entries", "stream=codec_name,width,height,sample_aspect_ratio,avg_frame_rate,"+
			"r_frame_rate,color_transfer,duration:stream_side_data:format=duration",
		"-of", "json", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return Info{}, fmt.Errorf("ffprobe: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var p probeOut
	if err := json.Unmarshal(out, &p); err != nil {
		return Info{}, fmt.Errorf("ffprobe output: %w", err)
	}
	if len(p.Streams) == 0 {
		return Info{}, errors.New("no video stream")
	}
	s := p.Streams[0]
	info := Info{Width: s.Width, Height: s.Height, Codec: s.CodecName,
		HDR: s.Transfer == "smpte2084" || s.Transfer == "arib-std-b67"}
	info.Duration, _ = strconv.ParseFloat(p.Format.Duration, 64)
	if info.Duration <= 0 {
		info.Duration, _ = strconv.ParseFloat(s.Duration, 64)
	}
	if info.FPS = ratio(s.AvgRate); info.FPS <= 0 || info.FPS > 1000 {
		info.FPS = ratio(s.RRate)
	}
	info.DisplayWidth = info.Width
	if sar := ratio(strings.Replace(s.SAR, ":", "/", 1)); sar > 0 {
		info.DisplayWidth = int(math.Round(float64(info.Width) * sar))
	}
	for _, sd := range s.SideDataList {
		if sd.DVProfile == 5 {
			info.DolbyVisionOnly = true
		}
	}
	if info.Width <= 0 || info.Height <= 0 || info.Duration <= 0 {
		return Info{}, fmt.Errorf("unusable video stream: %dx%d, %.1f s", info.Width, info.Height, info.Duration)
	}
	t.mu.Lock()
	t.cache[path] = cached{st.ModTime(), st.Size(), info}
	t.mu.Unlock()
	return info, nil
}

func ratio(s string) float64 {
	num, den, ok := strings.Cut(s, "/")
	n, err1 := strconv.ParseFloat(num, 64)
	if !ok {
		if err1 != nil {
			return 0
		}
		return n
	}
	d, err2 := strconv.ParseFloat(den, 64)
	if err1 != nil || err2 != nil || d == 0 {
		return 0
	}
	return n / d
}

// Size is the frame size extraction produces: the display aspect ratio, at
// most width pixels wide (0 keeps the full size).
func (i Info) Size(width int) (w, h int) {
	w, h = i.DisplayWidth, i.Height
	if width > 0 && width < w {
		h = int(math.Round(float64(h) * float64(width) / float64(w)))
		w = width
	}
	return w, max(h, 1)
}

// HDR10 and HLG to SDR, so a frame of a 4K HDR film does not come out grey
// and washed out.
const tonemap = "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709," +
	"tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,format=yuv420p,"

// Frame decodes the frame shown at second at (from the start of the file),
// width pixels wide at most. With -ss before -i and decoding on, ffmpeg seeks
// to the keyframe before at and decodes up to the exact frame, so this is the
// frame at that time, not the nearest keyframe.
func (t *Tool) Frame(ctx context.Context, path string, info Info, at float64, width int) (*image.RGBA, error) {
	select {
	case t.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-t.slots }()

	w, h := info.Size(width)
	filter := fmt.Sprintf("scale=%d:%d:flags=bicubic,setsar=1,format=rgba", w, h)
	if info.HDR {
		filter = tonemap + filter
	}
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	// -noautorotate keeps the size we computed: films carry no rotation.
	cmd := exec.CommandContext(ctx, t.ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-noautorotate", "-ss", strconv.FormatFloat(at, 'f', 6, 64), "-i", path,
		"-map", "0:V:0", "-an", "-sn", "-dn", "-frames:v", "1", "-vf", filter,
		"-pix_fmt", "rgba", "-f", "rawvideo", "pipe:1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// A killed ffmpeg says "signal: killed"; say why it was killed.
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("ffmpeg took longer than %s", t.timeout)
		} else if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no frame at %.3f s", at)
	}
	if len(out) != w*h*4 {
		return nil, fmt.Errorf("ffmpeg returned %d bytes for a %dx%d frame", len(out), w, h)
	}
	return &image.RGBA{Pix: out, Stride: w * 4, Rect: image.Rect(0, 0, w, h)}, nil
}

// Formats are the image formats Encode writes, with their content types.
var Formats = map[string]string{"jpeg": "image/jpeg", "png": "image/png", "webp": "image/webp"}

// Ext is the file extension of a format.
func Ext(format string) string {
	if format == "jpeg" {
		return "jpg"
	}
	return format
}

// Encode writes img as jpeg, png or webp. Go's standard library has no WebP
// encoder, so WebP goes back through ffmpeg, which takes a slot like Frame.
func (t *Tool) Encode(ctx context.Context, w io.Writer, img *image.RGBA, format string, quality int) error {
	switch format {
	case "jpeg":
		return jpeg.Encode(w, img, &jpeg.Options{Quality: quality})
	case "png":
		return (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(w, img)
	case "webp":
		select {
		case t.slots <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		defer func() { <-t.slots }()
		ctx, cancel := context.WithTimeout(ctx, t.timeout)
		defer cancel()
		b := img.Bounds()
		cmd := exec.CommandContext(ctx, t.ffmpeg, "-hide_banner", "-loglevel", "error",
			"-f", "rawvideo", "-pix_fmt", "rgba", "-s", fmt.Sprintf("%dx%d", b.Dx(), b.Dy()),
			"-i", "pipe:0", "-frames:v", "1", "-c:v", "libwebp", "-quality", strconv.Itoa(quality),
			"-f", "webp", "pipe:1")
		cmd.Stdin = bytes.NewReader(img.Pix)
		cmd.Stdout = w
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ffmpeg webp: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
	return fmt.Errorf("unknown format %q", format)
}

// Luma is the mean brightness of img from 0 to 1, on a sample of its pixels.
func Luma(img *image.RGBA) float64 {
	var sum, n float64
	b := img.Bounds()
	step := max(1, b.Dx()/160)
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			i := img.PixOffset(x, y)
			p := img.Pix[i : i+3]
			sum += 0.2126*float64(p[0]) + 0.7152*float64(p[1]) + 0.0722*float64(p[2])
			n++
		}
	}
	return sum / n / 255
}
