// youtube-dl-web is a simple web interface for youtube-dl
package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/artyom/autoflags"
	"github.com/artyom/basicauth"
)

func main() {
	args := runArgs{
		Results: os.ExpandEnv("${HOME}/youtube/ready"),
		WorkDir: os.ExpandEnv("${HOME}/youtube/.temp"),
		Addr:    "localhost:8080",
	}
	autoflags.Parse(&args)
	if err := run(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type runArgs struct {
	Results string `flag:"results,path to store downloaded files (automatically cleaned)"`
	WorkDir string `flag:"workdir,path to store temporary files (automatically cleaned)"`
	Addr    string `flag:"addr,address to listen"`
	Users   string `flag:"users,path to csv file with user,password pairs (leave empty to disable authentication)"`
}

func run(args runArgs) error {
	if args.Results == "" || args.WorkDir == "" {
		return errors.New("neither results nor workdir path can be empty")
	}
	if args.Results == args.WorkDir {
		return errors.New("results and workdir cannot be the same")
	}
	if td := os.TempDir(); args.Results == td || args.WorkDir == td {
		return errors.New("refusing to use system temporary dir, program expects exclusive directory access")
	}
	h := &handler{
		queue: make(chan string, 10),
		ready: args.Results,
		wdir:  args.WorkDir,
	}
	var hnd http.Handler = h
	if args.Users != "" {
		realm, err := realmFromFile(args.Users)
		if err != nil {
			return err
		}
		hnd = realm.WrapHandler(h)
	}
	go h.loop()
	srv := &http.Server{
		Addr:              args.Addr,
		Handler:           hnd,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return srv.ListenAndServe()
}

type handler struct {
	active int32 // 1 while download is running, 0 otherwise
	queue  chan string

	ready string // download results directory
	wdir  string // working directory, must be on the same fs as ready dir
}

func (h *handler) loop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			h.cleanup()
		case name := <-h.queue:
			if err := h.download(name); err != nil {
				log.Printf("%q download: %v", name, err)
			}
		}
	}
}

func (h *handler) cleanup() {
	filepath.Walk(h.wdir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		_ = os.Remove(path)
		return nil
	})
	filepath.Walk(h.ready, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if info.ModTime().Add(24 * time.Hour).Before(time.Now()) {
			_ = os.Remove(path)
		}
		return nil
	})
}

func (h *handler) download(name string) error {
	atomic.StoreInt32(&h.active, 1)
	defer atomic.StoreInt32(&h.active, 0)
	if isVideo(h.ready, name) {
		return nil
	}
	if err := os.MkdirAll(h.wdir, 0777); err != nil {
		return err
	}
	if err := os.MkdirAll(h.ready, 0777); err != nil {
		return err
	}
	time.Sleep(time.Duration(rand.Intn(4000)) * time.Millisecond)
	const outFile = "out.mp4"
	cmd := exec.Command("youtube-dl", "--no-mtime", "-q", "-o", outFile,
		"https://www.youtube.com/watch?v="+name)
	cmd.Dir = h.wdir
	if _, err := cmd.Output(); err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			_ = ioutil.WriteFile(filepath.Join(h.ready, name), e.Stderr, 0666)
		}
		return err
	}
	return os.Rename(filepath.Join(h.wdir, outFile), filepath.Join(h.ready, name))
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		h.handleForm(w, r)
		return
	}
	if !jobPath.MatchString(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	name := strings.Trim(r.URL.Path, "/")
	qLen, running := len(h.queue), h.running()
	if exists(h.ready, name) {
		if isVideo(h.ready, name) {
			w.Header().Set("Content-Disposition",
				fmt.Sprintf("attachment; filename=%q", name+".mp4"))
		}
		http.ServeFile(w, r, filepath.Join(h.ready, name))
		return
	}
	if !running && qLen == 0 {
		http.NotFound(w, r)
		return
	}
	if running {
		qLen++
	}
	jobPageTemplate.Execute(w, qLen)
}

func (h *handler) handleForm(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if r.ParseForm() != nil || r.Form.Get("url") == "" {
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}
		u, err := url.Parse(r.Form.Get("url"))
		if err != nil || !u.IsAbs() {
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}
		var name string
		switch s := u.String(); {
		case strings.HasPrefix(s, "https://www.youtube.com/watch?"):
			name = u.Query().Get("v")
		case strings.HasPrefix(s, "https://youtu.be/"):
			name = strings.Trim(u.Path, "/")
		}
		if name == "" || !jobName.MatchString(name) {
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}
		select {
		case h.queue <- name:
		default:
			http.Error(w, "Queue is full, please try later", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		acceptedTemplate.Execute(w, "/"+name)
		return
	}
	io.WriteString(w, formPage)
}

func (h *handler) running() bool { return atomic.LoadInt32(&h.active) != 0 }

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func isVideo(dir, name string) bool {
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 512)
	switch n, err := io.ReadFull(f, buf); err {
	case nil, io.ErrUnexpectedEOF:
		return strings.HasPrefix(http.DetectContentType(buf[:n]), "video/")
	}
	return false
}

func realmFromFile(name string) (*basicauth.Realm, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rd := csv.NewReader(f)
	rd.ReuseRecord = true
	rd.FieldsPerRecord = 2
	realm := basicauth.NewRealm("Restricted")
	for {
		rec, err := rd.Read()
		if err == io.EOF {
			return realm, nil
		}
		if err != nil {
			return nil, err
		}
		if err := realm.AddUser(rec[0], rec[1]); err != nil {
			return nil, err
		}
	}
}

var jobPath = regexp.MustCompile(`^/[a-zA-Z0-9-]+$`)
var jobName = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)

var jobPageTemplate = template.Must(template.New("job").Parse(`<!doctype html>
<head><meta http-equiv="refresh" content="10">
<style>body{font-size:x-large;margin:2em auto;max-width:50%}</style>
</head><body>
<p>{{.}} job(s) in queue, please wait. This page will refresh automatically.
`))

var acceptedTemplate = template.Must(template.New("accepted").Parse(`<!doctype html>
<head><meta http-equiv="refresh" content="5;url={{.}}">
<style>body{font-size:x-large;margin:2em auto;max-width:50%}</style>
</head><body>
<p>Job queued, results will be available at <a href="{{.}}">{{.}}</a>
`))

const formPage = `<!doctype html>
<head><title>Download job submit form</title><meta charset=utf-8>
<style>body{font-size:x-large;margin:2em auto;max-width:50%}</style>
<body>
<form method=post autocomplete=off>
<label for=url>YouTube url<br>(<samp>https://youtu.be/XXXXX</samp> or
<samp>https://www.youtube.com/watch?v=XXXXX</samp>)<br></label>
<input id=url type=url name=url placeholder="https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	title="YouTube video page URL"
	pattern="https://(youtu.be|www.youtube.com)/.+"
	size=60 autofocus required>
<input type=submit></form>
`
