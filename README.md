# framegrab

An HTTP API that extracts frames from the films and series of a media library
laid out the way Jellyfin and Plex expect. Ask for one frame or many, at an
exact time or frame number, or at random, and get them back as an image, a
ZIP, JSON or a contact sheet.

framegrab does not ship or fetch any media: it reads the library you mount.

```
GET /v1/movies?q=inception                       -> find the film, get its id
GET /v1/movies/{id}/frames?t=01:23:45.5          -> that exact frame, as a JPEG
GET /v1/movies/{id}/frames?random=9&output=grid  -> 9 random frames on one sheet
GET /v1/shows/{id}/frames?season=1&episode=2&frame=1200
```

## The library

Films, one folder each (versions of a film share its folder), or one file at
the root of the library; folders can be nested in collections:

```
movies/
  Inception (2010) [imdbid-tt1375666]/Inception.2010.2160p.mkv
  Alien (1979) {tmdb-348}/Alien.mkv
  Heat (1995).mp4
```

Series, one folder each, episodes found by `S01E02` (or `1x02`) in the file
name; a season folder (`Season 1`, `Saison 01`, `Specials`) wins over the
season in the file name:

```
tv/
  Battlestar Galactica (2003)/Season 1/BSG.S01E02.mkv
```

- **Titles and years** come from the folder name. Ids come from Jellyfin
  (`[imdbid-…]`, `[tmdbid-…]`, `[tvdbid-…]`) and Plex (`{imdb-…}`, `{tmdb-…}`,
  `{tvdb-…}`) tags, then from `movie.nfo`, `<file>.nfo` or `tvshow.nfo`. An NFO
  title and original title are searchable too.
- **Skipped:** extras folders (`Extras`, `Featurettes`, `Trailers`, `Behind The
  Scenes`…), extras files (`-trailer`, `-sample`…), hidden folders, and files
  without a video extension.
- **Several files for one film** are its versions, the largest first: the first
  one is used unless `file` says otherwise. Two files for one episode: the
  largest is kept.
- **Ids are stable** while a file or folder keeps its path: they are a hash of
  it.

The library is scanned at start, then every `FRAMEGRAB_SCAN_INTERVAL` and on
`POST /v1/rescan`. `/readyz` answers 503 until the first scan is done.

## API

All answers that are not images are JSON. Errors are `{"error": "…"}` with
400 (bad parameter), 404 (unknown id, no matching episode), 503 (first scan
still running) or 500.

| Endpoint | |
|---|---|
| `GET /v1/movies` | Search films. `q` (every word must start a word of the title, accents and case ignored), `year`, `imdb`, `tmdb`, `limit` (50), `offset`. |
| `GET /v1/movies/{id}` | A film, its files and what ffprobe says of each: duration, frame rate, size, HDR. |
| `GET /v1/movies/{id}/frames` | Frames of a film. |
| `GET /v1/shows` | Search series: same parameters, plus `tvdb`. |
| `GET /v1/shows/{id}` | A series and its episodes. |
| `GET /v1/shows/{id}/frames` | Frames of a series: `season` and `episode` choose the episode. |
| `POST /v1/rescan` | Scan the library again, in the background. |
| `GET /healthz`, `GET /readyz` | Liveness, and readiness after the first scan. |

### Frames

| Parameter | |
|---|---|
| `t` | A time: seconds (`5025.5`), `MM:SS`, `HH:MM:SS.mmm`, or a share of the duration (`50%`). Repeat it or separate with commas: `t=10,20&t=1:00`. |
| `frame` | A frame number, from 0. Repeatable like `t`. |
| `random` | How many random frames. |
| `seed` | Replays a random draw. Every answer with random frames gives its seed (`X-Framegrab-Seed`, or `seed` in JSON). |
| `skip_start`, `skip_end` | Random frames stay out of the first 5% and the last 10% (opening and credits). `0` to `49%`, as `5%` or `0.05`. |
| `season`, `episode` | Series only. `t` and `frame` need both, so they point at one episode. Random frames each come from an episode drawn among those that match (the whole series, a season, or one episode). |
| `file` | Films only: which version, from 0, in the order `GET /v1/movies/{id}` lists them. |
| `output` | `image` (one frame), `zip`, `json` or `grid`. Default: `image` for one frame, `zip` for more. |
| `format` | `jpeg` (default), `png` or `webp`. For `grid`, the sheet's format. |
| `quality` | 1 to 100, default 85 (JPEG and WebP). |
| `width` | Scale down to this width, keeping the aspect ratio. Default: full size, except `grid`, where each frame is 480 wide. |
| `cols` | `grid` only: columns, default the square root of the count. |

`t`, `frame` and `random` mix in one request, up to `FRAMEGRAB_MAX_FRAMES`
frames.

- **Exact:** the frame returned is the one shown at that time, decoded from the
  previous keyframe, not the nearest keyframe. `frame=n` is the n-th frame
  counted at the stream's average frame rate, which is exact for constant frame
  rate files (nearly every film and episode) and approximate otherwise.
- **Random:** the window is cut in as many equal slices as frames asked, and
  each frame is drawn in its own slice, so frames spread over the whole film.
  A frame that comes out near black (a fade) is drawn again, up to 4 times.
- **Images as shown:** anamorphic video is stretched to its display aspect
  ratio, and HDR10 and HLG are tonemapped to SDR. Dolby Vision profile 5 has no
  HDR10 layer to tonemap: its colours come out wrong, and `GET` on the film
  flags it with `dolby_vision_only`.
- **Answers:** a single image carries `X-Framegrab-Time`, `X-Framegrab-Frame`
  and, for a series, `X-Framegrab-Episode`. A ZIP holds `001_01-23-45.500.jpg`
  … (`001_S01E02_…` for a series) and `frames.json`. JSON gives, per frame,
  `time`, `timecode`, `frame`, `random`, `episode`, `file`, `width`, `height`
  and `data` (the image, base64).

```sh
# One frame at 1h23m45.5s, 1280 wide.
curl -o frame.jpg "http://framegrab/v1/movies/$ID/frames?t=1:23:45.5&width=1280"
# Five random frames of season 2, as a ZIP.
curl -o frames.zip "http://framegrab/v1/shows/$ID/frames?season=2&random=5"
# A 4x3 contact sheet of twelve random frames, in WebP.
curl -o sheet.webp "http://framegrab/v1/movies/$ID/frames?random=12&output=grid&cols=4&format=webp"
```

In a shell, `%` is safe as typed: `t=50%` works.

## Configuration

| Variable | Default | |
|---|---|---|
| `FRAMEGRAB_MOVIES` | | Film library folders, comma separated. |
| `FRAMEGRAB_SHOWS` | | Series library folders, comma separated. At least one of the two. |
| `FRAMEGRAB_LISTEN` | `:8080` | |
| `FRAMEGRAB_SCAN_INTERVAL` | `1h` | `0` scans only at start and on `POST /v1/rescan`. |
| `FRAMEGRAB_CONCURRENCY` | `2` | ffmpeg processes at once, across all requests. |
| `FRAMEGRAB_MAX_FRAMES` | `32` | Frames per request. |
| `FRAMEGRAB_FRAME_TIMEOUT` | `60s` | Per frame. |
| `FRAMEGRAB_FFMPEG`, `FRAMEGRAB_FFPROBE` | `ffmpeg`, `ffprobe` | |

A 4K HEVC frame takes about 0.7 s and a few hundred MiB in ffmpeg. There is no
authentication: keep it on a private network, or put it behind a proxy that
authenticates.

## Running it

```sh
docker run -p 8080:8080 -v /srv/media:/library:ro \
  -e FRAMEGRAB_MOVIES=/library/movies -e FRAMEGRAB_SHOWS=/library/tv \
  ghcr.io/pale-whale/framegrab:latest
```

With Helm, from the chart published with each release:

```sh
helm install framegrab oci://ghcr.io/pale-whale/charts/framegrab --version <version> \
  -f values.yaml
```

```yaml
libraries:
  movies: [/library/movies]
  shows: [/library/tv]
volumes:
  - name: library
    persistentVolumeClaim:
      claimName: media
      readOnly: true
volumeMounts:
  - name: library
    mountPath: /library
    readOnly: true
```

The pod runs as uid 1000, which must be able to read the library
(`podSecurityContext`). The chart sets no `fsGroup` on purpose: on a large
volume, Kubernetes would change the group of every file at each start.

## Development

```sh
go test ./...          # needs ffmpeg with zscale and libwebp; skips without ffmpeg
docker build -t framegrab .
```

The tests encode small clips where each frame is a different grey, so a frame
one off from the one asked for fails them.

Releases are cut by release-please from conventional commits: merging its pull
request publishes `ghcr.io/pale-whale/framegrab:vX.Y.Z` and the chart
`oci://ghcr.io/pale-whale/charts/framegrab`, whose version and image tag move
together.
