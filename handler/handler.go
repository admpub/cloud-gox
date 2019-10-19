package handler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jpillora/velox"

	"github.com/admpub/cloud-gox/release"
	"github.com/admpub/cloud-gox/static"
)

const maxQueue = 20

func randomID() string {
	b := make([]byte, 6)
	_, err := rand.Read(b)
	if err != nil {
		return "000000000000"
	}
	return hex.EncodeToString(b)
}

//goxHandler is an HTTP server accepting requests
//for cross-compilation
type goxHandler struct {
	auth        string
	q           chan *Compilation
	logger      *Logger
	files, sync http.Handler
	releasers   map[string]release.ReleaseHost
	config      serverConfig
	state       serverState
}

type serverConfig struct {
	Version, Bin, OS, Arch string
	NumCPU                 int
	Platforms              Platforms
	BinVersion             string
}

type serverState struct {
	sync.Mutex
	velox.State
	Ready        bool
	NumQueued    int
	NumDone      int
	NumTotal     int
	Compilations []*Compilation
	LogOffset    int64
	LogCount     int64
	Log          map[string]*message //Log is a map for syncing purposes
}

//New creates a new Handler
func New() (http.Handler, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, fmt.Errorf("git is not installed")
	}
	//check for go tool
	goBin, err := exec.LookPath("go")
	if err != nil {
		return nil, fmt.Errorf("go is not installed")
	}
	platforms, err := GetDefaultPlatforms(goBin)
	if err != nil {
		return nil, fmt.Errorf("failed to list platforms (go 1.7 or higher required)")
	}
	binVersion, err := GoBinVersion(goBin)
	if err != nil {
		return nil, err
	}
	userMessage := ""
	if u, err := user.Current(); err == nil {
		userMessage = fmt.Sprintf(" (process user: %s)", u.Username)
	}
	//prepare temp dir
	if err := os.RemoveAll(tempBuild); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("Failed to clear temporary directory: %s", err)
	}
	if err := os.Mkdir(tempBuild, 0755); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("Failed to create temporary directory: %s", err)
	}
	//
	s := &goxHandler{
		q:      make(chan *Compilation, maxQueue),
		logger: NewLogger(),
		releasers: map[string]release.ReleaseHost{
			"github": release.Github,
			// "bintray": release.Bintray,
			// "s3": TODO,
		},
		files: static.FileSystemHandler(),
		config: serverConfig{
			Version:    strings.TrimPrefix(runtime.Version(), "go"),
			Bin:        goBin,
			OS:         runtime.GOOS,
			Arch:       runtime.GOARCH,
			NumCPU:     runtime.NumCPU(),
			Platforms:  platforms,
			BinVersion: binVersion,
		},
		state: serverState{
			Log:       map[string]*message{},
			LogOffset: 1,
		},
	}

	s.sync = velox.SyncHandler(&s.state)

	//start logger first! copy log messages into state
	go s.dequeueLogs()
	s.Printf("cloud-gox started%s\n", userMessage)

	githubPan := os.Getenv("GH_PAN")
	if githubPan != "" {
		s.Printf("Using Github PAN to clone private repositories")
		gitCommand := exec.Command("bash", "-c", "git config --global url.\"https://"+githubPan+":x-oauth-basic@github.com/\".insteadOf \"https://github.com/\"")
		output, err := gitCommand.CombinedOutput()
		if err != nil {
			s.Printf("Git config failed: %s", err)
		}
		s.Printf("Git config succeeded: %s", output)
	}

	auth := os.Getenv("HTTP_USER") + ":" + os.Getenv("HTTP_PASS")
	if auth != ":" {
		s.auth = auth
		s.Printf("http auth is enabled\n")
	}

	for id, r := range s.releasers {
		if err := r.Auth(); err == nil {
			s.Printf("%s authenticated\n", id)
		} else {
			s.Printf("%s\n", err)
		}
	}

	//async startup sequence
	go func() {
		//ready!
		s.state.Ready = true
		s.state.Push()
		//service compilation queue
		go s.dequeue()
	}()

	return s, nil
}

func (s *goxHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	base := filepath.Base(r.URL.Path)
	if base == "hook" {
		s.hookReq(w, r)
		return
	}

	if s.auth != "" {
		if u, p, _ := r.BasicAuth(); s.auth != u+":"+p {
			w.Header().Set("WWW-Authenticate", "Basic")
			w.WriteHeader(401)
			w.Write([]byte("Unauthorized"))
			return
		}
	}

	if base == "sync" {
		s.sync.ServeHTTP(w, r)
	} else if base == "velox.js" {
		velox.JS.ServeHTTP(w, r)
	} else if base == "config" {
		s.configReq(w, r)
	} else if base == "compile" {
		s.enqueueReq(w, r)
	} else if strings.HasPrefix(r.URL.Path, "/download") {
		s.downloadReq(w, r)
	} else {
		s.files.ServeHTTP(w, r)
	}
}

func (s *goxHandler) configReq(w http.ResponseWriter, r *http.Request) {
	b, _ := json.MarshalIndent(&s.config, "", "  ")
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (s *goxHandler) downloadReq(w http.ResponseWriter, r *http.Request) {
	file := filepath.Join(tempBuild, strings.TrimPrefix(r.URL.Path, "/download/"))
	if !strings.HasSuffix(file, ".gz") {
		file += ".gz"
	}
	f, err := os.Open(file)
	if err != nil {
		http.Error(w, "Download failed: "+err.Error(), 500)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "Stat failed: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Content-Disposition", "attachment; filename="+filepath.Base(strings.TrimSuffix(file, ".gz")))
	io.Copy(w, f)
}

func (s *goxHandler) enqueueReq(w http.ResponseWriter, r *http.Request) {

	b, err := ioutil.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(400)
		w.Write([]byte("missing body"))
		return
	}

	c := &Compilation{}
	err = json.Unmarshal(b, c)
	if err != nil {
		w.WriteHeader(400)
		w.Write([]byte("invalid json: " + err.Error()))
		return
	}

	//disabled
	c.Releaser = ""

	err = s.enqueue(c)
	if err != nil {
		w.WriteHeader(400)
		w.Write([]byte(err.Error()))
	}
}

func (s *goxHandler) enqueue(c *Compilation) error {
	if c.Package == "" {
		return errors.New("Missing package")
	}
	if c.Version == "" {
		return errors.New("Missing version")
	}
	if c.VersionVar == "" {
		c.VersionVar = "main.VERSION"
	}
	if c.CommitVar == "" {
		c.CommitVar = "main.COMMIT"
	}
	if c.LabelVar == "" {
		c.LabelVar = "main.LABEL"
	}
	if c.Env == nil {
		c.Env = map[string]string{}
	}
	if c.Platforms != nil {
		c.OSArch = []string{}
		for os, arches := range c.Platforms {
			for arch, ok := range arches {
				if ok {
					c.OSArch = append(c.OSArch, os+"/"+arch)
				}
			}
		}
	}
	if len(c.OSArch) == 0 {
		return errors.New("Requires at least one OS/Arch")
	}
	if len(s.q) == maxQueue {
		return errors.New("Queue is full")
	}

	if c.Variables == nil {
		c.Variables = map[string]string{}
	}
	c.Variables[c.VersionVar] = c.Version

	s.state.Lock()
	s.state.NumTotal++
	c.ID = randomID()
	s.Printf("enqueue compilation (%s #%d)\n", c.ID, s.state.NumTotal)
	s.state.Unlock()

	c.Completed = false
	c.Queued = true
	c.Error = ""
	//default pkg root
	if len(c.Targets) == 0 {
		c.Targets = []string{"."}
	}

	s.q <- c
	s.state.Lock()
	s.state.NumQueued = len(s.q) //count after enqueue
	s.state.Unlock()
	s.state.Push()
	return nil
}

func (s *goxHandler) dequeue() {
	//run each compilation in series
	for c := range s.q {
		s.state.Lock()
		s.state.Compilations = append([]*Compilation{c}, s.state.Compilations...)
		c.Queued = false
		s.state.Ready = false
		s.state.NumQueued = len(s.q) //count after dequeue
		s.state.Unlock()
		s.state.Push()
		//run compile!
		if err := s.compile(c); err != nil {
			s.Printf("compile error '%s': %s\n", c.Package, err)
			c.Error = err.Error()
		}
		c.CompletedAt = time.Now()
		c.Completed = true
		s.state.Lock()
		s.state.Ready = true
		s.state.NumDone++
		s.state.Unlock()
		s.state.Push()
	}
}

func (s *goxHandler) dequeueLogs() {
	for l := range s.logger.messages {
		log.Print(l.Message)
		s.state.Lock()
		//handle insertions
		key := strconv.FormatInt(l.ID, 10)
		s.state.Log[key] = l
		//handle deletions when full
		if s.state.LogCount == maxLogSize {
			key = strconv.FormatInt(s.state.LogOffset, 10)
			delete(s.state.Log, key)
			s.state.LogOffset++
		} else {
			s.state.LogCount++
		}
		s.state.Unlock()
		s.state.Push()
	}
}

//Printf a server message to the log
func (s *goxHandler) Printf(f string, args ...interface{}) {
	fmt.Fprintf(s.logger, f, args...)
}
