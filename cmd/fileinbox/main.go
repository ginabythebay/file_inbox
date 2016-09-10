package main

import (
	"fmt"
	"io/ioutil"
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
		fmt.Printf("Unable to determine home directory: %+v", err)
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
		fmt.Printf("Failed to read %q due to %+v", p, err)
		return err
	}

	err = yaml.Unmarshal(bytes, c)
	if err != nil {
		fmt.Printf("Failed to unmarshal %q: %+v", string(bytes), err)
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
		fmt.Printf("Failed to marshal %#v: %+v", c, err)
		return err
	}

	err = os.MkdirAll(path.Dir(p), 0500)
	if err != nil {
		fmt.Printf("Failed to create directory %q, %+v", path.Dir(p), err)
		return err
	}

	err = ioutil.WriteFile(p, bytes, 0600)
	if err != nil {
		fmt.Printf("Failed to write %q due to %+v", p, err)
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
	fmt.Printf("\n\n%d files moved in %s.", fr.okCount, duration)
	fmt.Printf("\n\n%d files organized in %s.", fr.orgCount, fr.orgDuration)
	if len(fr.missingDirs) != 0 {
		fmt.Println("\n\nThe following directories are missing:")
		for k := range fr.missingDirs {
			fmt.Printf("    %s", k)
		}
		fmt.Printf("\n\nYou can automatically create the above directories by running this command again with the --%s flag", forceFlag)
	}
	if fr.failureCount != 0 {
		fmt.Printf("\n\nThere were %d failures", fr.failureCount)
		return fmt.Errorf("There were %d failures", fr.failureCount)
	}
	fmt.Println()
	return nil
}

type accum map[string]map[string]bool

func newAccum() accum {
	return map[string]map[string]bool{}
}

func (a accum) add(dest string, year string) {
	yearSet, ok := a[dest]
	if ok {
		yearSet[year] = true
	} else {
		a[dest] = map[string]bool{year: true}
	}
}

type destNeeds struct {
	dest  string
	years []string
}

func (a accum) iter() []destNeeds {
	var result []destNeeds
	for dest, yearSet := range a {
		var years []string
		for year := range yearSet {
			years = append(years, year)
		}
		result = append(result, destNeeds{dest, years})
	}
	return result
}

func doFileInner(ctx *cli.Context) (fileResult, error) {
	fr := fileResult{}
	fr.missingDirs = map[string]bool{}

	skipconfig := ctx.Bool(skipConfigFlag)
	config := &Config{
		persist: !skipconfig,
	}
	if err := config.read(); err != nil {
		return fr, errors.Wrap(err, "doFileInner")
	}

	force := ctx.Bool(forceFlag)

	if ctx.String(rootFlag) == "" && config.Root == "" {
		return fr, errors.Errorf("You must use the --%s flag to specify a root directory.  This will be stored for later use.", rootFlag)
	}

	if ctx.String(rootFlag) != "" {
		config.Root = ctx.String(rootFlag)
		if err := config.write(); err != nil {
			return fr, errors.Wrap(err, "writing config")
		}
	}

	inbox := config.inbox()
	if !isDir(inbox) {
		return fr, errors.Errorf("%q does not appear to be a directory", inbox)
	}

	files, err := ioutil.ReadDir(inbox)
	if err != nil {
		return fr, errors.Wrapf(err, "Unable to dir %q", inbox)
	}

	// figure out what we are working on
	allParsed := []*parsedName{}
	acc := newAccum()
	for _, file := range files {
		b := file.Name()
		parsed, err := parseFileName(force, b)
		if err != nil {
			fmt.Printf("Unable to parse %q, skipping: %+v", path.Join(inbox, b), err)
			fr.failureCount++
			continue
		}
		allParsed = append(allParsed, parsed)
		acc.add(parsed.dest, parsed.year)
	}

	// make sure destination directories are ready
	for _, dn := range acc.iter() {
		dest := config.dest(dn.dest)
		if !isDir(dest) {
			if force {
				if err = os.MkdirAll(dest, 0700); err != nil {
					return fr, errors.Wrapf(err, "Failed creating dir for %s", dest)
				}
			} else {
				fr.missingDirs[dest] = true
				fr.failureCount++
				continue
			}
		}

		orgStart := time.Now()
		orgCount, err := organize(force, dest, dn.years)
		fr.orgDuration += time.Since(orgStart)
		fr.orgCount += orgCount
		if err != nil {
			return fr, errors.Wrapf(err, "Failed organizing %q", dest)
		}
	}

	// move the inbox files into place
	for _, parsed := range allParsed {
		oldPath := path.Join(inbox, parsed.baseName)
		newPath := path.Join(config.dest(parsed.dest), parsed.year, parsed.baseName)
		err = os.Rename(oldPath, newPath)
		if err != nil {
			fmt.Printf("Unable to rename from %q to %q: %+v", oldPath, newPath, err)
			fr.failureCount++
			continue
		}
		fr.okCount++
	}

	return fr, nil
}

func organize(force bool, destDir string, years []string) (cnt uint32, err error) {
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
		parsed, err := parseFileName(force, f)
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

	for _, y := range years {
		if err = ensureHave(destDir, y, &dirsHave); err != nil {
			return cnt, errors.Wrap(err, "organize")
		}
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
	duration := time.Since(start)
	summarizeErr := fr.summarize(duration)
	if err != nil {
		fmt.Printf("\n\nError: %+v", err)
	}
	return anyError(err, summarizeErr)
}

func anyError(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

type parsedName struct {
	baseName string // e.g. 20160825_pge_taxes2016.pdf
	year     string // e.g. 2016
	month    string // e.g. 08
	date     string // e.g. 25
	dest     string // e.g. pge
}

var fileRe = regexp.MustCompile(`^(\d\d\d\d)(\d\d)(\d\d)_([^_\.]+).*$`)

func parseFileName(force bool, baseName string) (*parsedName, error) {
	matches := fileRe.FindStringSubmatch(baseName)
	if matches == nil || len(matches) != 5 {
		return nil, fmt.Errorf("Unable to parse %q.  We expect an 8 digit value like 20160825_pge_taxes2016.pdf or 20160825_pge.pdf", baseName)
	}
	year, month, date, dest := matches[1], matches[2], matches[3], matches[4]

	yearVal, err := yearTest.verify(year)
	if err != nil {
		return nil, err
	}
	yearDiff := yearVal - time.Now().Year()
	if !force && yearDiff > 2 {
		return nil, fmt.Errorf("%s is %d years in the future, which is highly suspect.  To continue, set the --force flag", baseName, yearDiff)
	}
	if _, err := monthTest.verify(month); err != nil {
		return nil, err
	}
	if _, err := dateTest.verify(date); err != nil {
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

func (ut unitTest) verify(s string) (i int, err error) {
	i, err = strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	if i < ut.min || i > ut.max {
		return 0, fmt.Errorf("Unexpected %s %q.  We expect a value between %d and %d", ut.unit, s, ut.min, ut.max)
	}
	return i, nil
}
