PKG = github.com/ginabythebay/file_inbox

all: clean build

build: test
	go install ${PKG}/...

test:
	go test -v ${PKG}/...

gomod:
	go mod tidy
	go mod vendor

.PHONY: all clean build
