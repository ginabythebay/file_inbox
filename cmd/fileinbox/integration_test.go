package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/urfave/cli"
)

// assert fails the test if the condition is false.
func assert(tb testing.TB, condition bool, msg string, v ...interface{}) {
	if !condition {
		_, file, line, _ := runtime.Caller(1)
		fmt.Printf("\033[31m%s:%d: "+msg+"\033[39m\n\n", append([]interface{}{filepath.Base(file), line}, v...)...)
		tb.FailNow()
	}
}

// ok fails the test if an err is not nil.
func ok(tb testing.TB, err error) {
	if err != nil {
		_, file, line, _ := runtime.Caller(1)
		fmt.Printf("\033[31m%s:%d: unexpected error: %s\033[39m\n\n", filepath.Base(file), line, err.Error())
		tb.FailNow()
	}
}

// equals fails the test if exp is not equal to act.
func equals(tb testing.TB, exp, act interface{}) {
	if !reflect.DeepEqual(exp, act) {
		_, file, line, _ := runtime.Caller(1)
		fmt.Printf("\033[31m%s:%d:\n\n\texp: %#v\n\n\tgot: %#v\033[39m\n\n", filepath.Base(file), line, exp, act)
		tb.FailNow()
	}
}

func createFiles(t *testing.T, root string, allNames []string) error {
	for _, n := range allNames {
		p := path.Join(root, n)
		if strings.HasSuffix(n, "/") {
			ok(t, os.MkdirAll(p, 0700))
		} else {
			parent := path.Dir(p)
			ok(t, os.MkdirAll(parent, 0700))
			base := path.Base(p)
			ok(t, ioutil.WriteFile(p, []byte(fmt.Sprintf("contents for %s", base)), 0600))
		}
	}
	return nil
}

func readFiles(t *testing.T, root string) []string {
	if !strings.HasSuffix(root, "/") {
		root = root + "/"
	}

	var found []string
	walkFunc := func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			p = p + "/"
		} else {
			base := path.Base(p)
			bytes, err := ioutil.ReadFile(p)
			ok(t, err)
			expectedContents := fmt.Sprintf("contents for %s", base)
			assert(t,
				string(bytes) == expectedContents,
				"Error reading %q.  Expected contents to be %s but found %s",
				p, expectedContents, string(bytes))
		}
		p = strings.TrimPrefix(p, root)
		if p != "/" {
			found = append(found, p)
		}
		return nil
	}

	ok(t, filepath.Walk(root, walkFunc))
	return found
}

func TestSimple(t *testing.T) {
	start := []string{
		"filed/foo/",
		"filed/bar/",
		"inbox/20160701_foo.pdf",
		"inbox/20160702_foo.pdf",
		"inbox/20160702_bar.pdf",
	}
	expected := []string{
		"filed/",
		"filed/foo/",
		"filed/bar/",
		"filed/foo/20160701_foo.pdf",
		"filed/foo/20160702_foo.pdf",
		"filed/bar/20160702_bar.pdf",
		"inbox/",
	}

	root, err := ioutil.TempDir("", "file_inbox_test")
	ok(t, err)
	defer func() {
		if !t.Failed() {
			// if the test failed, we leave this around for forensics
			os.RemoveAll(root)
		}
	}()

	createFiles(t, root, start)

	args := []string{
		"file_inbox",
		"--root", root,
		"--skipconfig",
	}
	ok(t, newCli().Run(args))

	found := readFiles(t, root)
	sort.Sort(sort.StringSlice(found))
	sort.Sort(sort.StringSlice(expected))
	equals(t, expected, found)
}

func TestMissingDirs(t *testing.T) {
	start := []string{
		"filed/foo/",
		"filed/bar/",
		"inbox/20160701_foo.pdf",
		"inbox/20160702_foo.pdf",
		"inbox/20160702_bar.pdf",
		"inbox/20160702_baz.pdf",
		"inbox/20160703_baz.pdf",
		"inbox/20160702_gus.pdf",
	}
	expectedFiles := []string{
		"filed/",
		"filed/foo/",
		"filed/bar/",
		"filed/foo/20160701_foo.pdf",
		"filed/foo/20160702_foo.pdf",
		"filed/bar/20160702_bar.pdf",
		"inbox/",
		"inbox/20160702_baz.pdf",
		"inbox/20160703_baz.pdf",
		"inbox/20160702_gus.pdf",
	}

	root, err := ioutil.TempDir("", "file_inbox_test")
	ok(t, err)
	defer func() {
		if !t.Failed() {
			// if the test failed, we leave this around for forensics
			os.RemoveAll(root)
		}
	}()

	createFiles(t, root, start)

	args := []string{
		"file_inbox",
		"--root", root,
		"--skipconfig",
	}
	app := newCli()
	var result *fileResult
	app.Action = func(ctx *cli.Context) error {
		fr, err := doFileInner(ctx)
		result = &fr
		return err
	}
	ok(t, app.Run(args))
	assert(t, result.summarize() != nil, "Expected failure, but got nil error")

	foundFiles := readFiles(t, root)
	sort.Sort(sort.StringSlice(foundFiles))
	sort.Sort(sort.StringSlice(expectedFiles))
	equals(t, expectedFiles, foundFiles)

	expectedMissingDirs := map[string]bool{
		path.Join(root, "filed", "baz"): true,
		path.Join(root, "filed", "gus"): true,
	}
	equals(t, expectedMissingDirs, result.missingDirs)
}
