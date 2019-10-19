package handler

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/webx-top/com"

	"github.com/admpub/cloud-gox/release"
)

//temporary storeage for the resulting binaries
var tempBuild = path.Join(os.TempDir(), "cloudgox")

//server's compile method
func (s *goxHandler) compile(c *Compilation) error {
	s.Printf("compiling %s...\n", c.Package)
	c.StartedAt = time.Now()
	//optional releaser
	releaser := s.releasers[c.Releaser]
	var rel release.Release
	once := sync.Once{}
	setupRelease := func() {
		desc := "*This release was automatically cross-compiled and uploaded by " +
			"[cloud-gox](https://github.com/admpub/cloud-gox) at " +
			time.Now().UTC().Format(time.RFC3339) + "* using Go " +
			"*" + s.config.BinVersion + "*"
		if r, err := releaser.Setup(c.Package, c.Version, desc); err == nil {
			rel = r
			s.Printf("%s successfully setup release %s (%s)\n", c.Releaser, c.Package, c.Version)
		} else {
			s.Printf("%s failed to setup release %s (%s)\n", c.Releaser, c.Package, err)
		}
	}
	//initial environment
	goEnv := environ{}
	//setup temp dir
	buildDir := filepath.Join(tempBuild, c.ID)
	if err := os.Mkdir(buildDir, 0755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("Failed to create build directory %s", err)
	}
	//set this builds' package GOPATH
	goPath := filepath.Join(buildDir, "gopath")
	if err := os.Mkdir(goPath, 0755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("Failed to create build directory %s", err)
	}
	goEnv["GOPATH"] = goPath
	s.Printf("GOPATH: %s", goPath)
	//set this builds' package directory
	pkgDir := filepath.Join(goPath, "src", c.Package)
	s.Printf("Package: %s", pkgDir)
	//get target package
	if c.GoGet {
		if err := s.exec(goPath, "go", goEnv, "get", "-v", c.Package); err != nil {
			return fmt.Errorf("failed to get dependencies %s (%s)", c.Package, err)
		}
	} else {
		localRepo := filepath.Join(os.Getenv("GOPATH"), "src", c.Package)
		if _, err := os.Stat(localRepo); err != nil {
			return fmt.Errorf("failed to find package %s", c.Package)
		}
		if err := com.CopyDir(localRepo, pkgDir); err != nil {
			return fmt.Errorf("failed to copy package %s", c.Package)
		}
	}
	if _, err := os.Stat(pkgDir); err != nil {
		return fmt.Errorf("failed to find package %s", c.Package)
	}
	//decide whether to enable or disable go modules
	if _, err := os.Stat(filepath.Join(pkgDir, "go.mod")); err == nil {
		goEnv["GO111MODULE"] = "on"
	} else {
		goEnv["GO111MODULE"] = "off"
	}
	//specified commit?
	if c.Commitish != "" {
		s.Printf("loading specific commit %s\n", c.Commitish)
		//go to specific commit
		if err := s.exec(pkgDir, "git", nil, "status"); err != nil {
			return fmt.Errorf("failed to load commit: %s: %s is not a git repo", c.Commitish, c.Package)
		}
		if err := s.exec(pkgDir, "git", nil, "checkout", c.Commitish); err != nil {
			return fmt.Errorf("failed to load commit %s: %s", c.Package, err)
		}
		c.Variables[c.CommitVar] = c.Commitish
	} else {
		//commitish not set, attempt to find it
		s.Printf("retrieving current commit hash\n")
		cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
		cmd.Dir = pkgDir
		if out, err := cmd.Output(); err == nil {
			currCommitish := strings.TrimSuffix(string(out), "\n")
			c.Variables[c.CommitVar] = currCommitish
		}
	}
	if len(c.Label) > 0 {
		c.Variables[c.LabelVar] = c.Label
	}
	//calculate ldflags
	ldflags := []string{}
	if c.Shrink {
		s.Printf("ld-flag: -s -w (shrink)")
		ldflags = append(ldflags, "-s", "-w")
	}
	c.Variables["main.CLOUD_GOX"] = "1"
	c.Variables["main.BUILD_TIME"] = strconv.FormatInt(time.Now().Unix(), 10)
	for k, v := range c.Variables {
		s.Printf("ld-flag-X: %s=%s", k, v)
		ldflags = append(ldflags, "-X "+k+"="+v)
	}
	//compile all combinations of each target and each osarch
	for _, t := range c.Targets {
		target := filepath.Join(c.Package, t)
		targetDir := filepath.Join(pkgDir, t)
		targetName := filepath.Base(target)
		//go-get target deps
		if c.GoGet && targetDir != pkgDir {
			if err := s.exec(targetDir, "go", goEnv, "get", "-v", "-d", "."); err != nil {
				s.Printf("failed to get dependencies  of subdirectory %s", t)
				continue
			}
		}
		if c.GoGenerate {
			if err := s.exec(targetDir, "go", goEnv, "generate"); err != nil {
				s.Printf("failed to generate %s\n", c.Package)
				continue
			}
		}
		//compile target for all os/arch combos
		for _, osarchstr := range c.OSArch {
			osarch := strings.SplitN(osarchstr, "/", 2)
			osname := osarch[0]
			arch := osarch[1]
			targetFilename := fmt.Sprintf("%s_%s_%s", targetName, osname, arch)
			if osname == "windows" {
				targetFilename += ".exe"
			}
			targetOut := filepath.Join(buildDir, targetFilename)
			if _, err := os.Stat(targetDir); err != nil {
				s.Printf("failed to find target %s\n", target)
				continue
			}
			args := []string{
				"build",
				"-a",
				"-v",
				"-ldflags", strings.Join(ldflags, " "),
				"-o", targetOut,
			}
			if len(c.Tags) > 0 {
				args = append(args, "-tags", c.Tags)
			}
			args = append(args, ".")
			c.Env["GOOS"] = osname
			c.Env["GOARCH"] = arch
			if !c.CGO {
				s.Printf("cgo disabled")
				c.Env["CGO_ENABLED"] = "0"
			}
			for k, v := range c.Env {
				s.Printf("env: %s=%s", k, v)
				goEnv[k] = v
			}
			//run go build with cross compile configuration
			if err := s.exec(targetDir, "go", goEnv, args...); err != nil {
				s.Printf("failed to build %s\n", targetFilename)
				continue
			}
			//gzip file
			b, err := ioutil.ReadFile(targetOut)
			if err != nil {
				return err
			}
			gzb := bytes.Buffer{}
			gz := gzip.NewWriter(&gzb)
			gz.Write(b)
			gz.Close()
			b = gzb.Bytes()
			targetFilename += ".gz"

			//optional releaser
			if releaser != nil {
				once.Do(setupRelease)
			}
			if rel != nil {
				if err := rel.Upload(targetFilename, b); err == nil {
					s.Printf("%s included asset in release %s\n", c.Releaser, targetFilename)
				} else {
					s.Printf("%s failed to release asset %s: %s\n", c.Releaser, targetFilename, err)
				}
			}
			//swap non-gzipd with gzipd
			if err := os.Remove(targetOut); err != nil {
				s.Printf("asset local remove failed %s\n", err)
				continue
			}
			targetOut += ".gz"
			if err := ioutil.WriteFile(targetOut, b, 0755); err != nil {
				s.Printf("asset local write failed %s\n", err)
				continue
			}
			//ready for download!
			s.Printf("compiled %s\n", targetFilename)
			c.Files = append(c.Files, targetFilename)
			s.state.Push()
		}
	}

	if c.Commitish != "" {
		s.Printf("revert repo back to latest commit\n")
		if err := s.exec(pkgDir, "git", nil, "checkout", "-"); err != nil {
			s.Printf("failed to revert commit %s: %s", c.Package, err)
		}
	}

	if len(c.Files) == 0 {
		return errors.New("No files compiled")
	}
	s.Printf("compiled %s (%s)\n", c.Package, c.Version)
	return nil
}
