package library

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// IDs are the external ids a folder name or an NFO file carries.
type IDs struct {
	IMDb string `json:"imdb,omitempty"`
	TMDb string `json:"tmdb,omitempty"`
	TVDb string `json:"tvdb,omitempty"`
}

func (a *IDs) fill(b IDs) {
	if a.IMDb == "" {
		a.IMDb = b.IMDb
	}
	if a.TMDb == "" {
		a.TMDb = b.TMDb
	}
	if a.TVDb == "" {
		a.TVDb = b.TVDb
	}
}

var (
	// Jellyfin: "Title (2010) [imdbid-tt1375666]", Plex: "Title (2010) {imdb-tt1375666}".
	idTag = regexp.MustCompile(`(?i)[\[{](imdb|tmdb|tvdb)(?:id)?-([^\]}]+)[\]}]`)
	// "Title (2010)", the year at the end once the id tags are gone.
	titleYear = regexp.MustCompile(`^(.*?)\s*\((\d{4})\)\s*$`)
	// A scene name, "Inception.2010.1080p.BluRay.x264": the title stops at the
	// last year, so "Blade.Runner.2049.2017" keeps its 2049.
	sceneYear = regexp.MustCompile(`^(.+)[ ._-]+\(?((?:19|20)\d{2})\)?(?:[ ._-]|$)`)
	// Plex's "Title (2010) - 1080p" version suffix.
	versionSuffix = regexp.MustCompile(`\s+-\s+[^-]+$`)

	// S01E02, s1e2, S01.E02, then the 1x02 form.
	episodeSE = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])s(\d{1,3})[ ._-]?e(\d{1,4})`)
	episodeX  = regexp.MustCompile(`(?:^|[^0-9])(\d{1,2})x(\d{2,3})(?:[^0-9]|$)`)
	seasonDir = regexp.MustCompile(`(?i)^(?:season|saison|series|staffel|s)[ ._-]*(\d{1,3})$`)
)

// parseName reads a folder or file name (without extension) into a title,
// a year (0 when there is none) and the ids it carries.
func parseName(name string) (title string, year int, ids IDs) {
	for _, m := range idTag.FindAllStringSubmatch(name, -1) {
		switch strings.ToLower(m[1]) {
		case "imdb":
			ids.IMDb = m[2]
		case "tmdb":
			ids.TMDb = m[2]
		case "tvdb":
			ids.TVDb = m[2]
		}
	}
	name = strings.TrimSpace(idTag.ReplaceAllString(name, ""))
	if m := titleYear.FindStringSubmatch(name); m != nil {
		year, _ = strconv.Atoi(m[2])
		return strings.TrimSpace(m[1]), year, ids
	}
	if m := titleYear.FindStringSubmatch(versionSuffix.ReplaceAllString(name, "")); m != nil {
		year, _ = strconv.Atoi(m[2])
		return strings.TrimSpace(m[1]), year, ids
	}
	if m := sceneYear.FindStringSubmatch(name); m != nil {
		year, _ = strconv.Atoi(m[2])
		return strings.TrimSpace(strings.NewReplacer(".", " ", "_", " ").Replace(m[1])), year, ids
	}
	return name, 0, ids
}

// parseEpisode finds the season and episode numbers in a file name. ok is
// false when the name carries no episode number.
func parseEpisode(name string) (season, episode int, ok bool) {
	m := episodeSE.FindStringSubmatch(name)
	if m == nil {
		m = episodeX.FindStringSubmatch(name)
	}
	if m == nil {
		return 0, 0, false
	}
	season, _ = strconv.Atoi(m[1])
	episode, _ = strconv.Atoi(m[2])
	return season, episode, true
}

// parseSeasonDir reads "Season 01", "Saison 1" or "Specials" (season 0).
func parseSeasonDir(name string) (int, bool) {
	if strings.EqualFold(name, "specials") {
		return 0, true
	}
	m := seasonDir.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	return n, true
}

var videoExt = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true,
	".ts": true, ".m2ts": true, ".webm": true, ".wmv": true, ".mpg": true,
	".mpeg": true, ".flv": true, ".ogv": true,
}

func isVideo(name string) bool {
	return videoExt[strings.ToLower(filepath.Ext(name))]
}

// Folders Jellyfin and Plex read as extras rather than as the film or episode.
var extrasDirs = map[string]bool{
	"extras": true, "featurettes": true, "behind the scenes": true,
	"deleted scenes": true, "interviews": true, "scenes": true, "shorts": true,
	"trailers": true, "other": true, "others": true, "sample": true,
	"samples": true, "clips": true, "backdrops": true, "theme-music": true,
}

var extrasSuffix = []string{
	"-trailer", "-sample", "-featurette", "-behindthescenes", "-deleted",
	"-deletedscene", "-interview", "-scene", "-short", "-other", "-extra", "-clip",
}

func isExtrasDir(name string) bool {
	return extrasDirs[strings.ToLower(name)]
}

func isExtrasFile(name string) bool {
	base := strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
	if base == "sample" || strings.HasPrefix(base, "sample.") || strings.Contains(base, ".sample.") {
		return true
	}
	for _, s := range extrasSuffix {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	return false
}

// nfo is the part of Kodi-style metadata (movie.nfo, tvshow.nfo) we read.
type nfo struct {
	Title         string `xml:"title"`
	OriginalTitle string `xml:"originaltitle"`
	Year          int    `xml:"year"`
	IMDbID        string `xml:"imdbid"`
	TMDbID        string `xml:"tmdbid"`
	TVDbID        string `xml:"tvdbid"`
	UniqueIDs     []struct {
		Type  string `xml:"type,attr"`
		Value string `xml:",chardata"`
	} `xml:"uniqueid"`
}

// readNFO returns what the first readable file of paths says. A missing or
// broken NFO is not an error: the folder name is enough.
func readNFO(paths ...string) (n nfo, ok bool) {
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		err = xml.NewDecoder(f).Decode(&n)
		f.Close()
		if err == nil {
			return n, true
		}
	}
	return nfo{}, false
}

func (n nfo) ids() IDs {
	ids := IDs{IMDb: n.IMDbID, TMDb: n.TMDbID, TVDb: n.TVDbID}
	for _, u := range n.UniqueIDs {
		v := strings.TrimSpace(u.Value)
		switch strings.ToLower(u.Type) {
		case "imdb":
			ids.IMDb = v
		case "tmdb":
			ids.TMDb = v
		case "tvdb":
			ids.TVDb = v
		}
	}
	return ids
}

var fold = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

// normalize lowers and strips accents and punctuation, so "Amélie" matches
// "amelie" and "Spider-Man" matches "spider man".
func normalize(s string) string {
	s, _, _ = transform.String(fold, strings.ToLower(s))
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}
