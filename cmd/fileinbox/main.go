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
	"syscall"

	yaml "gopkg.in/yaml.v2"

	"github.com/urfave/cli"
)

// Config represents some configuration we can store/read
type Config struct {
	Root string
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

	app := cli.NewApp()
	app.Name = "fileinbox"
	app.Usage = "Move files into the correct place, using their names."
	app.Action = doFile
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "root",
			Usage: "Specifies the root directory.  Will be saved into ~/.config/fileinbox/fileinbox.yaml"},
	}
	app.Run(os.Args)
}

func doFile(ctx *cli.Context) error {
	config := &Config{}
	if err := config.read(); err != nil {
		return err
	}

	if ctx.String("root") == "" && config.Root == "" {
		log.Print("You must use the --root flag to specify a root directory.  This will be stored for later use.")
		return nil
	}

	if ctx.String("root") != "" {
		config.Root = ctx.String("root")
		if err := config.write(); err != nil {
			return err
		}
	}

	log.Printf("Using root of %q", config.Root)
	return nil
}
