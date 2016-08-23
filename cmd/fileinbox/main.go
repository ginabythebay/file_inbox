package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"os/user"
	"path"
	"runtime"
	"strings"
	"syscall"

	yaml "gopkg.in/yaml.v2"

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
	failureCount uint32
	missingDirs  map[string]bool
}

func (fr fileResult) summarize() error {
	log.Printf("\n\n%d files moved.", fr.okCount)
	if len(fr.missingDirs) != 0 {
		log.Println("\n\nThe following directories are missing:")
		for k := range fr.missingDirs {
			log.Printf("    %s", k)
		}
		log.Printf("\n\nYou can automatically create the above directories but running this command again with the --%s flag", forceFlag)
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
		// e.g. 20160822_dest.pdf or 20160822_dest_tag.pdf
		tokens := strings.SplitN(b, "_", 3)

		if len(tokens) < 2 {
			log.Printf("Unable to parse %q, skipping", path.Join(inbox, b))
			fr.failureCount++
			continue
		}
		ext := path.Ext(b)
		dest := strings.TrimSuffix(tokens[1], ext)
		dest = config.dest(dest)
		if !isDir(dest) {
			if force {
				if err = os.MkdirAll(dest, 0700); err != nil {
					log.Printf("Failed creating dir for %s, %v", dest, err)
					return fr, err
				}
			} else {
				fr.missingDirs[dest] = true
				fr.failureCount++
				continue
			}
		}

		oldPath := path.Join(inbox, b)
		newPath := path.Join(dest, b)
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

func doFile(ctx *cli.Context) error {
	fr, err := doFileInner(ctx)
	if err != nil {
		return err
	}
	return fr.summarize()
}
