package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"os/user"
	"path"
	"regexp"
	"runtime"
	"strconv"
	"syscall"
	"time"

	yaml "gopkg.in/yaml.v2"

	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

const (
	rootFlag       string = "root"
	skipConfigFlag        = "skipconfig"
	forceFlag             = "force"
)

// Config represents some configuration we can store/read
type Config struct {
	persist bool
	Root    string
}

func (c *Config) path() (string, error) {
	usr, err := user.Current()
	if err != nil {
		log.Printf("Unable to determine home directory: %v", err)
		return "", err
	}

	return path.Join(usr.HomeDir, ".config", "fileinbox", "fileinbox.yaml"), nil
}

func (c *Config) read() error {
	if !c.persist {
		return nil
	}
	p, err := c.path()
	if err != nil {
		return err
	}

	bytes, err := ioutil.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		log.Printf("Failed to read %q due to %v", p, err)
		return err
	}

	err = yaml.Unmarshal(bytes, c)
	if err != nil {
		log.Printf("Failed to unmarshal %q: %v", string(bytes), err)
	}
	return err
}

func (c *Config) write() error {
	if !c.persist {
		return nil
	}
	p, err := c.path()
	if err != nil {
		return err
	}

	bytes, err := yaml.Marshal(c)
	if err != nil {
		log.Printf("Failed to marshal %#v: %v", c, err)
		return err
	}

	err = os.MkdirAll(path.Dir(p), 0500)
	if err != nil {
		log.Printf("Failed to create directory %q, %v", path.Dir(p), err)
		return err
	}

	err = ioutil.WriteFile(p, bytes, 0600)
	if err != nil {
		log.Printf("Failed to write %q due to %v", p, err)
		return err
	}

	return nil
}

func (c *Config) inbox() string {
	return path.Join(c.Root, "inbox")
}

func (c *Config) dest(name string) string {
	return path.Join(c.Root, "filed", name)
}

func newCli() *cli.App {
	app := cli.NewApp()
	app.Name = "fileinbox"
	app.Usage = "Move files into the correct place, using their names."
	app.Action = doFile
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  rootFlag,
			Usage: "Specifies the root directory.  Will be saved into ~/.config/fileinbox/fileinbox.yaml"},
		cli.BoolFlag{
			Name:   skipConfigFlag,
			Usage:  "If set, we don't read or write configuration.  Meant for testing.",
			Hidden: true,
		},
		cli.BoolFlag{
			Name:  forceFlag,
			Usage: "If set, we will create destination directories as needed.",
		},
	}
	return app
}

func main() {
	sigChan := make(chan os.Signal)
	go func() {
		stacktrace := make([]byte, 8192)
		for _ = range sigChan {
			length := runtime.Stack(stacktrace, true)
			fmt.Println(string(stacktrace[:length]))
		}
	}()
	signal.Notify(sigChan, syscall.SIGQUIT)

	newCli().Run(os.Args)
}

func isDir(name string) bool {
	fi, err := os.Stat(name)
	if err != nil {
		return false
	}
	return fi.IsDir()
}

type fileResult struct {
	okCount      uint32
	orgCount     uint32
	orgDuration  time.Duration
	failureCount uint32
	missingDirs  map[string]bool
}

func (fr fileResult) summarize(duration time.Duration) error {
	log.Printf("\n\n%d files moved in %s.", fr.okCount, duration)
	log.Printf("\n\n%d files organized in %s.", fr.orgCount, fr.orgDuration)
	if len(fr.missingDirs) != 0 {
		log.Println("\n\nThe following directories are missing:")
		for k := range fr.missingDirs {
			log.Printf("    %s", k)
		}
		log.Printf("\n\nYou can automatically create the above directories by running this command again with the --%s flag", forceFlag)
	}
	if fr.failureCount != 0 {
		log.Printf("\n\nThere were %d failures", fr.failureCount)
		return fmt.Errorf("There were %d failures", fr.failureCount)
	}
	return nil
}

func doFileInner(ctx *cli.Context) (fileResult, error) {
	fr := fileResult{}
	fr.missingDirs = map[string]bool{}

	skipconfig := ctx.Bool(skipConfigFlag)
	config := &Config{
		persist: !skipconfig,
	}
	if err := config.read(); err != nil {
		return fr, err
	}

	force := ctx.Bool(forceFlag)

	if ctx.String(rootFlag) == "" && config.Root == "" {
		log.Printf("You must use the --%s flag to specify a root directory.  This will be stored for later use.", rootFlag)
		return fr, nil
	}

	if ctx.String(rootFlag) != "" {
		config.Root = ctx.String(rootFlag)
		if err := config.write(); err != nil {
			return fr, err
		}
	}

	inbox := config.inbox()
	if !isDir(inbox) {
		log.Printf("%q does not appear to be a directory", inbox)
		return fr, os.ErrInvalid
	}

	files, err := ioutil.ReadDir(inbox)
	if err != nil {
		log.Printf("Unable to dir %q: %v", inbox, err)
		return fr, err
	}

	for _, file := range files {
		b := file.Name()
		parsed, err := parseFileName(b)
		if err != nil {
			log.Printf("Unable to parse %q, skipping: %+v", path.Join(inbox, b), err)
			fr.failureCount++
			continue
		}
		dest := config.dest(parsed.dest)
		if !isDir(dest) {
			if force {
				if err = os.MkdirAll(dest, 0700); err != nil {
					log.Printf("Failed creating dir for %s, %+v", dest, err)
					return fr, err
				}
			} else {
				fr.missingDirs[dest] = true
				fr.failureCount++
				continue
			}
		}

		orgStart := time.Now()
		orgCount, err := organize(dest, parsed.year)
		fr.orgDuration += time.Since(orgStart)
		fr.orgCount += orgCount
		if err != nil {
			log.Printf("Failed organizing %q, %+v", dest, err)
			return fr, err
		}

		oldPath := path.Join(inbox, b)
		newPath := path.Join(dest, parsed.year, b)
		err = os.Rename(oldPath, newPath)
		if err != nil {
			log.Printf("Unable to rename from %q to %q: %v", oldPath, newPath, err)
			fr.failureCount++
			continue
		}
		fr.okCount++
	}

	return fr, nil
}

func organize(destDir string, year string) (cnt uint32, err error) {
	dirsHave := map[string]bool{}
	filesHave := []string{}
	children, err := ioutil.ReadDir(destDir)
	if err != nil {
		return 0, errors.Wrap(err, "ReadDir")
	}
	for _, c := range children {
		name := c.Name()
		if c.IsDir() {
			dirsHave[name] = true
		} else {
			filesHave = append(filesHave, name)
		}
	}

	for _, f := range filesHave {
		parsed, err := parseFileName(f)
		if err != nil {
			return 0, errors.Wrap(err, "organize")
		}
		if err = ensureHave(destDir, parsed.year, &dirsHave); err != nil {
			return 0, errors.Wrap(err, "organize")
		}
		oldPath := path.Join(destDir, f)
		newPath := path.Join(destDir, parsed.year, f)
		err = os.Rename(oldPath, newPath)
		if err != nil {
			return 0, errors.Wrapf(err, "organizing %q", oldPath)
		}
		cnt++
	}

	if err = ensureHave(destDir, year, &dirsHave); err != nil {
		return cnt, errors.Wrap(err, "organize")
	}
	return cnt, nil
}

func ensureHave(destDir string, year string, dirsHave *map[string]bool) error {
	if (*dirsHave)[year] {
		return nil
	}
	if err := os.Mkdir(path.Join(destDir, year), 0700); err != nil {
		return errors.Wrap(err, "ensureHave")
	}
	(*dirsHave)[year] = true
	return nil
}

func doFile(ctx *cli.Context) error {
	start := time.Now()
	fr, err := doFileInner(ctx)
	if err != nil {
		return err
	}
	duration := time.Since(start)
	return fr.summarize(duration)
}

type parsedName struct {
	baseName string // e.g. 20160825_pge_taxes2016.pdf
	year     string // e.g. 2016
	month    string // e.g. 08
	date     string // e.g. 25
	dest     string // e.g. pge
}

var fileRe = regexp.MustCompile(`^(\d\d\d\d)(\d\d)(\d\d)_([^_\.]+).*$`)

func parseFileName(baseName string) (*parsedName, error) {
	matches := fileRe.FindStringSubmatch(baseName)
	if matches == nil || len(matches) != 5 {
		return nil, fmt.Errorf("Unable to parse %q.  We expect an 8 digit value like 20160825_pge_taxes2016.pdf or 20160825_pge.pdf", baseName)
	}
	year, month, date, dest := matches[1], matches[2], matches[3], matches[4]

	if err := yearTest.verify(year); err != nil {
		return nil, err
	}
	if err := monthTest.verify(month); err != nil {
		return nil, err
	}
	if err := dateTest.verify(date); err != nil {
		return nil, err
	}

	return &parsedName{baseName, year, month, date, dest}, nil
}

var (
	yearTest  = unitTest{1, 9999, "year"}
	monthTest = unitTest{1, 12, "month"}
	dateTest  = unitTest{1, 31, "date"}
)

type unitTest struct {
	min  int
	max  int
	unit string
}

func (ut unitTest) verify(s string) error {
	i, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	if i < ut.min || i > ut.max {
		return fmt.Errorf("Unexpected %s %q.  We expect a value between %d and %d", ut.unit, s, ut.min, ut.max)
	}
	return nil
}
