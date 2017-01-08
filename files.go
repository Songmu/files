package main

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/monochromegane/go-gitignore"
	"github.com/monochromegane/go-home"
)

var (
	ignore        = flag.String("i", env(`FILES_IGNORE_PATTERN`, `^(\.git|\.hg|\.svn|_darcs|\.bzr)$`), "Ignore directory")
	progress      = flag.Bool("p", false, "Progress message")
	absolute      = flag.Bool("a", false, "Display absolute path")
	match         = flag.String("m", "", "Display matched files")
	maxfiles      = flag.Int64("M", -1, "Max files")
	careGitignore = flag.Bool("g", false, "care gitignore")
)

var (
	ignorere *regexp.Regexp
	matchre  *regexp.Regexp
	maxcount = int64(^uint64(0) >> 1)
	maxError = errors.New("Overflow max count")
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func globalGitIgnoreName() string {
	gitCmd, err := exec.LookPath("git")
	if err != nil {
		return ""
	}
	file, err := exec.Command(gitCmd, "config", "--get", "core.excludesfile").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(filepath.Base(string(file)))
}

type fileInfo struct {
	os.FileInfo
}

func (f fileInfo) isSymlink() bool {
	return f.FileInfo.Mode()&os.ModeSymlink == os.ModeSymlink
}

type ignoreMatchers []gitignore.IgnoreMatcher

func (im ignoreMatchers) Match(path string, isDir bool) bool {
	for _, ig := range im {
		if ig == nil {
			return false
		}
		if ig.Match(path, isDir) {
			return true
		}
	}
	return false
}

type walkFunc func(path string, info fileInfo, ignores ignoreMatchers) (ignoreMatchers, error)

func filesAsync(base string) chan string {
	wg := new(sync.WaitGroup)

	q := make(chan string, 20)
	n := int64(0)

	var ignMatchers ignoreMatchers
	if *careGitignore {
		if homeDir := home.Dir(); homeDir != "" {
			globalIgnore := globalGitIgnoreName()
			if globalIgnore != "" {
				if matcher, err := gitignore.NewGitIgnore(filepath.Join(homeDir, globalIgnore), base); err == nil {
					ignMatchers = append(ignMatchers, matcher)
				}
			}
		}
	}
	gitignoreFile := ".gitignore"

	fi, err := os.Lstat(base)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if !fi.IsDir() {
		fmt.Fprintf(os.Stderr, "%q is not a directory")
		os.Exit(1)
	}

	var ferr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		sem := make(chan struct{}, 16)
		ferr = walk(base, fileInfo{fi}, ignMatchers, func(path string, fi fileInfo, matchers ignoreMatchers) (ignoreMatchers, error) {


			if *careGitignore && fi.IsDir() && !fi.isSymlink() {
				if matcher, err := gitignore.NewGitIgnore(filepath.Join(path, gitignoreFile)); err == nil {
					matchers = append(matchers, matcher)
				}
			}

			if ignorere.MatchString(fi.Name()) || *careGitignore && matchers.Match(path, fi.IsDir()) {
				var err error
				if fi.IsDir() {
					err = filepath.SkipDir
				}
				return matchers, err
			}
			if !fi.IsDir() {
				n++
				// This is pseudo handling because this is not atomic
				if n > maxcount {
					return matchers, maxError
				}
				if *progress {
					if n%10 == 0 {
						fmt.Fprintf(os.Stderr, "\r%d            \r", n)
					}
				}
				q <- filepath.ToSlash(path)
			}
			return matchers, nil
		}, sem)
	}()

	go func() {
		wg.Wait()
		close(q)
		if ferr != nil {
			fmt.Fprintln(os.Stderr, ferr)
		}
	}()
	return q
}

func walk(path string, info fileInfo, parentIgnores ignoreMatchers, walkFn walkFunc, sem chan struct{}) error {
	ignores, walkError := walkFn(path, info, parentIgnores)
	if walkError != nil {
		if info.IsDir() && walkError == filepath.SkipDir {
			return nil
		}
		return walkError
	}
	if info.isSymlink() || !info.IsDir() {
		return nil
	}

	files, err := ioutil.ReadDir(path)
	if err != nil {
		return err
	}
	var ferr error
	wg := &sync.WaitGroup{}
	for _, file := range files {
		f := fileInfo{file}
		select {
		case sem <- struct{}{}:
			wg.Add(1)
			go func(path string, file fileInfo, ignores ignoreMatchers) {
				defer wg.Done()
				defer func() { <-sem }()
				err := walk(path, file, ignores, walkFn, sem)
				if err != nil {
					ferr = err
				}
			}(filepath.Join(path, file.Name()), f, ignores)
		default:
			err := walk(filepath.Join(path, file.Name()), f, ignores, walkFn, sem)
			if err != nil {
				ferr = err
			}
		}
	}
	wg.Wait()
	return ferr
}

func main() {
	flag.Parse()

	var err error

	if *match != "" {
		matchre, err = regexp.Compile(*match)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	ignorere, err = regexp.Compile(*ignore)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	base := "."
	if flag.NArg() > 0 {
		base = filepath.FromSlash(flag.Arg(0))
		if runtime.GOOS == "windows" && base != "" && base[0] == '~' {
			base = filepath.Join(os.Getenv("USERPROFILE"), base[1:])
		}
	}

	if *maxfiles > 0 {
		maxcount = *maxfiles
	}

	left := base
	if *absolute {
		if left, err = filepath.Abs(base); err != nil {
			left = filepath.Dir(left)
		}
	} else if !filepath.IsAbs(base) {
		if cwd, err := os.Getwd(); err == nil {
			if left, err = filepath.Rel(cwd, base); err == nil {
				base = left
			}
		}
	}

	q := filesAsync(base)

	printLine := func() func(string) {
		if *absolute && !filepath.IsAbs(base) {
			return func(s string) {
				fmt.Println(filepath.Join(left, s))
			}
		} else {
			return func(s string) {
				fmt.Println(s)
			}
		}
	}()
	for s := range q {
		printLine(s)
	}
}
