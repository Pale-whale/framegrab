package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParseName(t *testing.T) {
	for _, c := range []struct {
		in    string
		title string
		year  int
		ids   IDs
	}{
		{"Inception (2010)", "Inception", 2010, IDs{}},
		{"Inception (2010) [imdbid-tt1375666]", "Inception", 2010, IDs{IMDb: "tt1375666"}},
		{"Inception (2010) {imdb-tt1375666} {tmdb-27205}", "Inception", 2010, IDs{IMDb: "tt1375666", TMDb: "27205"}},
		{"Inception (2010) [tmdbid-27205]", "Inception", 2010, IDs{TMDb: "27205"}},
		{"Inception (2010) - 1080p", "Inception", 2010, IDs{}},
		{"[REC] (2007)", "[REC]", 2007, IDs{}},
		{"28 Years Later - The Bone Temple (2026)", "28 Years Later - The Bone Temple", 2026, IDs{}},
		{"Inception.2010.1080p.BluRay.x264", "Inception", 2010, IDs{}},
		{"Blade.Runner.2049.2017.2160p", "Blade Runner 2049", 2017, IDs{}},
		{"3 Body Problem", "3 Body Problem", 0, IDs{}},
	} {
		title, year, ids := parseName(c.in)
		if title != c.title || year != c.year || ids != c.ids {
			t.Errorf("parseName(%q) = %q, %d, %+v; want %q, %d, %+v", c.in, title, year, ids, c.title, c.year, c.ids)
		}
	}
}

func TestParseEpisode(t *testing.T) {
	for _, c := range []struct {
		in      string
		season  int
		episode int
		ok      bool
	}{
		{"All.Her.Fault.S01E02.MULTi.HDR.2160p.WEB.mkv", 1, 2, true},
		{"Show - s3e14 - Title.mkv", 3, 14, true},
		{"Show S01E01-E02.mkv", 1, 1, true},
		{"Show 2x05 Title.mkv", 2, 5, true},
		{"Show (1080p).mkv", 0, 0, false},
		{"Show.2160p.x265.mkv", 0, 0, false},
	} {
		s, e, ok := parseEpisode(c.in)
		if s != c.season || e != c.episode || ok != c.ok {
			t.Errorf("parseEpisode(%q) = %d, %d, %v; want %d, %d, %v", c.in, s, e, ok, c.season, c.episode, c.ok)
		}
	}
	for in, want := range map[string]int{"Season 1": 1, "Season 01": 1, "Saison 12": 12, "Specials": 0, "S02": 2} {
		if n, ok := parseSeasonDir(in); !ok || n != want {
			t.Errorf("parseSeasonDir(%q) = %d, %v; want %d", in, n, ok, want)
		}
	}
	if _, ok := parseSeasonDir("Extras"); ok {
		t.Error("parseSeasonDir(Extras) should not be a season")
	}
}

func TestNormalize(t *testing.T) {
	if got := normalize("Amélie : Le Fabuleux Destin, Spider-Man"); got != "amelie le fabuleux destin spider man" {
		t.Errorf("normalize = %q", got)
	}
}

func write(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScan(t *testing.T) {
	dir := t.TempDir()
	movies, shows := filepath.Join(dir, "movies"), filepath.Join(dir, "tv")
	write(t, filepath.Join(movies, "Amélie (2001)", "Amelie.2001.1080p.mkv"), 10)
	write(t, filepath.Join(movies, "Amélie (2001)", "Amelie.2001.2160p.mkv"), 20)
	write(t, filepath.Join(movies, "Amélie (2001)", "Amelie-trailer.mkv"), 30)
	write(t, filepath.Join(movies, "Amélie (2001)", "Extras", "Making of.mkv"), 30)
	write(t, filepath.Join(movies, "Amélie (2001)", "poster.jpg"), 30)
	os.WriteFile(filepath.Join(movies, "Amélie (2001)", "movie.nfo"), []byte(
		`<?xml version="1.0"?><movie><title>Amélie</title>
<originaltitle>Le Fabuleux Destin d'Amélie Poulain</originaltitle><year>2001</year>
<uniqueid type="imdb">tt0211915</uniqueid><uniqueid type="tmdb">194</uniqueid></movie>`), 0o644)
	write(t, filepath.Join(movies, "Collection", "Alien (1979) {tmdb-348}", "Alien.mkv"), 10)
	write(t, filepath.Join(movies, "Heat (1995).mp4"), 10)
	write(t, filepath.Join(movies, ".hidden", "x.mkv"), 10)

	write(t, filepath.Join(shows, "Battlestar Galactica (2003)", "Season 1", "BSG.S01E02.mkv"), 10)
	write(t, filepath.Join(shows, "Battlestar Galactica (2003)", "Season 1", "BSG.S01E01.mkv"), 10)
	write(t, filepath.Join(shows, "Battlestar Galactica (2003)", "Season 1", "BSG.S01E01.720p.mkv"), 5)
	write(t, filepath.Join(shows, "Battlestar Galactica (2003)", "Specials", "BSG.S00E01.mkv"), 10)
	write(t, filepath.Join(shows, "Battlestar Galactica (2003)", "Season 2", "BSG.S02E01.nfo"), 10)
	write(t, filepath.Join(shows, "Empty Show", "Season 1", "notes.txt"), 10)

	lib := New([]string{movies}, []string{shows})
	if lib.Ready() {
		t.Fatal("ready before the first scan")
	}
	lib.Scan(context.Background())
	if !lib.Ready() {
		t.Fatal("not ready after a scan")
	}

	all := lib.Movies(Query{})
	var titles []string
	for _, m := range all {
		titles = append(titles, m.Title)
	}
	if len(all) != 3 || titles[0] != "Alien" || titles[1] != "Amélie" || titles[2] != "Heat" {
		t.Fatalf("movies = %q", titles)
	}
	am := all[1]
	if len(am.Files) != 2 || am.Files[0].Name != "Amelie.2001.2160p.mkv" {
		t.Errorf("Amélie files = %+v, want the two versions, largest first", am.Files)
	}
	if am.IDs.IMDb != "tt0211915" || am.IDs.TMDb != "194" {
		t.Errorf("Amélie ids = %+v, want the NFO's", am.IDs)
	}
	if all[0].IDs.TMDb != "348" || all[0].Folder != filepath.Join("Collection", "Alien (1979) {tmdb-348}") {
		t.Errorf("Alien = %+v", all[0])
	}
	if lib.Movie(am.ID) != am {
		t.Error("Movie(id) does not find Amélie")
	}
	for q, want := range map[Query]int{
		{Text: "amelie"}: 1, {Text: "FABULEUX destin"}: 1, {Text: "ali"}: 1, {Text: "lien"}: 0,
		{Year: 1995}: 1, {IMDb: "TT0211915"}: 1, {TMDb: "348"}: 1, {Text: "heat", Year: 1996}: 0,
	} {
		if got := len(lib.Movies(q)); got != want {
			t.Errorf("Movies(%+v) = %d results, want %d", q, got, want)
		}
	}

	ss := lib.Shows(Query{})
	if len(ss) != 1 || ss[0].Title != "Battlestar Galactica" || ss[0].Year != 2003 {
		t.Fatalf("shows = %+v", ss)
	}
	eps := ss[0].Episodes
	if len(eps) != 3 || eps[0].Season != 0 || eps[1].Episode != 1 || eps[2].Episode != 2 {
		t.Fatalf("episodes = %+v", eps)
	}
	if eps[1].File.Name != "BSG.S01E01.mkv" {
		t.Errorf("S01E01 = %s, want the larger version", eps[1].File.Name)
	}

	// Ids do not change from one scan to the next.
	lib.Scan(context.Background())
	if lib.Movie(am.ID) == nil || lib.Show(ss[0].ID) == nil {
		t.Error("ids changed across scans")
	}
}
