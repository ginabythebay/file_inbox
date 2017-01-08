package main

import (
	"fmt"
	"io"
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
	rootFlag         string = "root"
	skipConfigFlag          = "skipconfig"
	forceFlag               = "force"
)

// Config represents some configuration we can store/read
type Config struct {
	persist bool
	Root    string
	ExtraInboxes []string
	CC      struct {
		Root  string
		Dests []string
	}
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

func (c *Config) ccDest(dest string) string {
	if c.CC.Root == "" {
		return ""
	}
	for _, d := range c.CC.Dests {
		if d == dest {
			return path.Join(c.CC.Root, d)
		}
	}
	return ""
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
		for range sigChan {
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
	fmt.Printf("\n\n%d directories organized in %s.", fr.orgCount, fr.orgDuration)
	if len(fr.missingDirs) != 0 {
		fmt.Println("\n\nThe following directories are missing:")
		for k := range fr.missingDirs {
			fmt.Printf("    %s\n", k)
		}
		fmt.Printf("\n\nYou can automatically create the above directories by running this command again with the --%s flag", forceFlag)
	}
	if fr.failureCount != 0 {
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

	allInboxes := []string{config.inbox()}
	allInboxes = append(allInboxes, config.ExtraInboxes...)
	for _, inbox := range allInboxes {
		if err := processInbox(inbox, config, force, &fr); err != nil {
			return fr, errors.Wrapf(err, "processing %s", inbox)
		}
	}

	return fr, nil
}

func processInbox(inbox string, config *Config, force bool, fr *fileResult) error {
	if !isDir(inbox) {
		return errors.Errorf("%q does not appear to be a directory", inbox)
	}

	files, err := ioutil.ReadDir(inbox)
	if err != nil {
		return errors.Wrapf(err, "Unable to dir %q", inbox)
	}

	// figure out what we are working on
	allParsed := []*parsedName{}
	acc := newAccum()
	for _, file := range files {
		b := file.Name()
		var parsed *parsedName
		parsed, err = parseFileName(force, b)
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
					return errors.Wrapf(err, "Failed creating dir for %s", dest)
				}
			} else {
				fr.missingDirs[dest] = true
				fr.failureCount++
				continue
			}
		}

		orgStart := time.Now()
		var orgCount uint32
		orgCount, err = organize(force, dest, dn.years)
		fr.orgDuration += time.Since(orgStart)
		fr.orgCount += orgCount
		if err != nil {
			return errors.Wrapf(err, "Failed organizing %q", dest)
		}
	}

	tasks := len(allParsed)

	// move the inbox files into place
	for i, parsed := range allParsed {
		if src, dest:= cc(config, inbox, parsed); src != "" {
			dir, _ := path.Split(dest)
			if !isDir(dir) {
				if err := os.Mkdir(dir, 0700); err != nil {
					fmt.Printf("Failed to create dir %q: %+v\n", dir, err)
					fr.failureCount++
					continue
				}
			}
			if err := copyFile(src, dest); err != nil {
				fmt.Printf("Unable to copy from %q to %q: %+v\n", src, dest, err)
				fr.failureCount++
				continue
			}
		}


		dest := config.dest(parsed.dest)
		oldPath := path.Join(inbox, parsed.baseName)
		newPath := path.Join(dest, parsed.year, parsed.baseName)
		err = move(oldPath, newPath)
		if err != nil {
			fmt.Printf("Unable to move from %q to %q: %+v\n", oldPath, newPath, err)
			if !fr.missingDirs[dest] {
				fr.failureCount++
			}
			continue
		}
		fmt.Printf("(%d/%d) Filed\r", i+1, tasks)
		fr.okCount++
	}
	fmt.Print(" \n")

	return nil
}

func cc(config *Config, inbox string, parsed *parsedName) (src, dest string) {
	dest = config.ccDest(parsed.dest)
	if dest == "" {
		return "", ""
	}
	dest = path.Join(dest, parsed.year, parsed.baseName)
	src = path.Join(inbox, parsed.baseName)
	return src, dest
}

func copyFile(src, dest string) error {
	var from, to *os.File
	var err error
	defer func() {
		if from != nil {
			from.Close()
		}
		if to != nil {
			closeError := to.Close()
			if err == nil {
				err = closeError
			}
		}
	}()

	from, err = os.Open(src)
	if err != nil {
		return err
	}
	to, err = os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0666)
	if err != nil {
		return err
	}
	_, err = io.Copy(to, from)
	return err
}

func organize(force bool, destDir string, years []string) (cnt uint32, err error) {
	start := time.Now()

	dirsHave := map[string]bool{}
	filesHave := []string{}
	children, err := ioutil.ReadDir(destDir)
	if err != nil {
		return cnt, errors.Wrap(err, "ReadDir")
	}
	for _, c := range children {
		name := c.Name()
		if c.IsDir() {
			dirsHave[name] = true
		} else {
			filesHave = append(filesHave, name)
		}
	}

	// Amount of work we need to do.  We don't count the years work as
	// it often will be a nop (the directory will often already exists)
	tasks := len(filesHave)

	for i, f := range filesHave {
		var parsed *parsedName
		parsed, err = parseFileName(force, f)
		if err != nil {
			return cnt, errors.Wrap(err, "organize")
		}
		if err = ensureHave(destDir, parsed.year, &dirsHave); err != nil {
			return cnt, errors.Wrap(err, "organize")
		}
		oldPath := path.Join(destDir, f)
		newPath := path.Join(destDir, parsed.year, f)
		err = move(oldPath, newPath)
		if err != nil {
			return cnt, errors.Wrapf(err, "organizing %q", oldPath)
		}
		cnt++
		fmt.Printf("(%d/%d) organizing %s\r", i+1, tasks, destDir)
	}

	for _, y := range years {
		if err = ensureHave(destDir, y, &dirsHave); err != nil {
			return cnt, errors.Wrap(err, "organize")
		}
	}

	if tasks != 0 {
		fmt.Printf("Organized %s in %s\n", destDir, time.Since(start))
	}

	return cnt, nil
}

func move(fromName, toName string) error {
	err := os.Rename(fromName, toName)
	if err == nil {
		return nil
	}
	if _, ok := err.(*os.LinkError); !ok {
		return err
	}

	var from, to *os.File
	defer func() {
		if from != nil {
			from.Close()
		}
		if to != nil {
			closeError := to.Close()
			if err == nil {
				err = closeError
			}
		}

		if err == nil {
			err = os.Remove(fromName)
		} else {
			os.Remove(toName)
		}
	}()

	from, err = os.Open(fromName)
	if err != nil {
		return err
	}
	to, err = os.OpenFile(toName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0666)
	if err != nil {
		return err
	}
	_, err = io.Copy(to, from)
	return err
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

var fileRe = regexp.MustCompile(`^(\d\d\d\d)(\d\d)(\d\d)_([^_.]+).*$`)

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
