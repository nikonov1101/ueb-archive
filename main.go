package main

import (
	"bufio"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"gopkg.in/ini.v1"
)

var (
	archiveRoot string

	// TODO: what if we have sub-directories in this folder?
	// options:
	// 1. traverse by our own
	// 2. list all directories explicitly (easier)
	bookmarksFolder string

	workers       int
	ffProfileName string
)

func init() {
	flag.StringVar(&archiveRoot, "archive", "/tmp/archive/", "where to store saved web pages")
	flag.StringVar(&bookmarksFolder, "folder", "archive", "firefox folder name to archive")
	flag.IntVar(&workers, "workers", 4, "number of paralel downloads")
	flag.StringVar(&ffProfileName, "profile-name", "Profile0", "firefox profile name, check ~/.mozilla/firefox/profiles.ini")
	flag.Parse()

	// now it's a convenient version of printf
	// without a worry about \n at the end.
	log.SetPrefix("")
	log.SetFlags(0)
}

func main() {
	dbPath := defaultProfileDB()
	log.Printf("will read bookmarks from %q", dbPath)

	connstr := fmt.Sprintf("file:%s?immutable=1", dbPath)
	log.Printf("conn string: %s", connstr)
	db, err := sql.Open("sqlite3", connstr)
	if err != nil {
		panik(err, "open database")
	}
	defer db.Close()

	started := time.Now()
	downloads := make(chan *bookmark)
	wg := &sync.WaitGroup{}

	log.Printf("starting %d workers", workers)
	wg.Add(workers)
	for i := range workers {
		i := i
		go func() {
			worker(i, downloads)
			wg.Done()
		}()
	}

	bookmarksList, err := getBookmarksToSync(db)
	if err != nil {
		panik(err, "get bookmarks")
	}

	for i := range bookmarksList {
		downloads <- &bookmarksList[i]
	}

	close(downloads)
	wg.Wait()

	makeIndexPage(bookmarksList)
	log.Printf("done %d urls in %s", len(bookmarksList),
		time.Since(started).Truncate(time.Second))
}

func defaultProfileDB() string {
	homedir, err := os.UserHomeDir()
	if err != nil {
		panik(err, "get user home dir")
	}

	ffDir := path.Join(homedir, ".mozilla/firefox")
	ffProfilePath := path.Join(ffDir, "profiles.ini")

	log.Printf("reading ff profiles from %s", ffProfilePath)
	profiles, err := ini.Load(ffProfilePath)
	if err != nil {
		panik(err, "read profiles.ini from "+ffProfilePath)
	}

	profile, err := profiles.GetSection(ffProfileName)
	if err != nil {
		panik(err, "get profile from ini")
	}
	profileName, err := profile.GetKey("Name")
	if err != nil {
		panik(err, "get .Name section from profile")
	}
	profilePath, err := profile.GetKey("Path")
	if err != nil {
		panik(err, "get .Path section from a profile")
	}

	log.Printf("profile: name: %q; path: %q", profileName, profilePath)
	return path.Join(ffDir, profilePath.String(), "places.sqlite")
}

type bookmark struct {
	title string
	url   string
	hash  int64

	archiveMeta *archiveMeta
}

func (b bookmark) url50() string {
	if len(b.url) > 50 {
		return b.url[:50] + "..."
	}
	return b.url
}

type archiveMeta struct {
	saved    []string
	execTime time.Duration

	wgetFinished   string
	wgetDownloaded string
}

func (a archiveMeta) index() string {
	if len(a.saved) == 0 {
		panic("empty archive referened")
	}
	return a.saved[0]
}

// getBookmarksToSync read bookmarks from a given folder in a firefox database.
func getBookmarksToSync(db *sql.DB) ([]bookmark, error) {
	// exchange folder name to its id, type=2 is folder
	row := db.QueryRow(`select id from moz_bookmarks where title=? and type=2`, bookmarksFolder)
	var folderID int64
	if err := row.Scan(&folderID); err != nil {
		panik(err, "query moz_bookmarks table")
	}
	log.Printf("get bookmarks: got folder id = %v", folderID)

	// get ids of all bookmarks in such folder, type=1 is bookmark,
	// it is named fk as of foreign key because the fk points to the `moz_places` table
	rows, err := db.Query(`select fk from moz_bookmarks where parent=? and type=1`, folderID)
	if err != nil {
		panik(err, "query bookmarks from a folder")
	}

	var fkeys []int64
	for rows.Next() {
		var fk int64
		if err := rows.Scan(&fk); err != nil {
			panik(err, "query fk row")
		}
		fkeys = append(fkeys, fk)
	}

	log.Printf("get bookmarks: got %d fkeys", len(fkeys))

	// finaly, we know all the keys we need, let's query the actual bookmarks data:
	bookmarks := make([]bookmark, len(fkeys))
	for i, placeid := range fkeys {
		tmp := &bookmarks[i]
		row = db.QueryRow(`select title, url_hash, url from moz_places where id=?`, placeid)
		if err := row.Scan(&tmp.title, &tmp.hash, &tmp.url); err != nil {
			panik(err, "query moz_places for bookmark details")
		}
	}

	return bookmarks, nil
}

func downloadOne(bmark *bookmark) {
	started := time.Now()
	// the classic "linux download web-page" stackoverflow answer, works well for decades
	logfile := path.Join(archiveRoot, fmt.Sprintf("wget-%d.log", bmark.hash))
	cmd := exec.Command(
		"wget",
		"--verbose",
		"--page-requisites",
		"--convert-links",
		"--adjust-extension",
		"--no-parent",
		"-o", logfile,
		bmark.url,
	)
	// pretend to be a simble terminal,
	// without that, wget weirdly use some sort of
	// fancy unicode single brackets, which i unable
	// to just cut with string slicing. so kindly
	// requesting wget to produce normal ascii stuff.
	cmd.Env = append(cmd.Env, "TERM=xterm")
	cmd.Dir = archiveRoot

	if err := cmd.Run(); err != nil {
		// from "man 1 wget":
		// > 8   Server issued an error response.
		//
		// any 404 returned by any sequential requests (for images, .css, .js, etc)
		// will lead to this error code, even if we have succesfully downloaded
		// everyting else, so just ignore this particular code
		if cmd.ProcessState.ExitCode() != 8 {
			log.Printf("WARN: wget failed with status=%d, url=%q",
				cmd.ProcessState.ExitCode(), bmark.url50())
			return
		}
	}

	meta := parseWgetLog(logfile)
	meta.execTime = time.Since(started).Truncate(time.Millisecond)
	bmark.archiveMeta = &meta
}

func worker(n int, downloads <-chan *bookmark) {
	for bmark := range downloads {
		downloadOne(bmark)
	}

	log.Printf("worker_%d: exiting", n)
}

func makeIndexPage(list []bookmark) error {
	// TODO(nikonov): worker probably should return some metadata about the download:
	// ok/fail, time taken, files downloaded, its size, etc.
	// we could show that on the index page as well.
	index := `<!DOCTYPE html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title></title></head><body><h1>μeb-archive</h1>`
	index += "<ol>"
	for _, bmark := range list {
		target := "#"
		suffix := "MISSING"
		if bmark.archiveMeta != nil {
			target = bmark.archiveMeta.index()
			suffix = "OK"
		}

		title := bmark.title
		if len(title) == 0 {
			// TODO(nikonov): extract from a <title> tag?
			title = target
		}

		// TODO(nikonov):target=blank,noreferrer, etc
		index += fmt.Sprintf(`<li><a href="%s">%s | %s</a></li>`, target, title, suffix)
	}
	index += "</ol></body></html>"

	if err := os.WriteFile(path.Join(archiveRoot, "index.html"), []byte(index), 0o600); err != nil {
		panik(err, "write index file")
	}

	return nil
}

func parseWgetLog(logfile string) archiveMeta {
	out, err := os.OpenFile(logfile, os.O_RDONLY, 0o600)
	if err != nil {
		panik(err, "read wget log at "+logfile)
	}
	defer out.Close()

	archive := archiveMeta{
		// no idea, should we measure the average?
		saved: make([]string, 0, 10),
	}

	lscan := bufio.NewScanner(out)
	for lscan.Scan() {
		line := lscan.Text()
		// WARN: that's not quite portable
		if strings.HasPrefix(line, "Saving to: ") {
			fileName := line[12 : len(line)-1]
			archive.saved = append(archive.saved, fileName)
		}
		if strings.HasPrefix(line, "FINISHED") {
			line = strings.TrimPrefix(line, "FINISHED")
			line = strings.ReplaceAll(line, "--", "")
			archive.wgetFinished = strings.TrimSpace(line)
		}
		if strings.HasPrefix(line, "Downloaded:") && len(archive.wgetFinished) > 0 {
			line = strings.TrimPrefix(line, "Downloaded:")
			archive.wgetDownloaded = strings.TrimSpace(line)
		}
	}

	return archive
}

func panik(err error, msg ...string) {
	if len(msg) > 0 {
		panic(msg[0] + ": " + err.Error())
	}
	panic(err)
}
