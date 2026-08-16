BINARY := bin/toolctl
IMAGE := toolctl/tools:latest
PREFIX ?= $(HOME)/.local

.PHONY: build image install clean

build:
	go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/toolctl

image:
	docker build -t $(IMAGE) -f Dockerfile .

install: build
	mkdir -p $(PREFIX)/bin
	cp $(BINARY) $(PREFIX)/bin/toolctl

clean:
	rm -rf bin
