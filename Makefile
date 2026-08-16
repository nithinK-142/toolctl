BINARY := bin/toolctl
IMAGE := toolctl/tools:latest

.PHONY: build image install clean

build:
	go build -o $(BINARY) ./cmd/toolctl

image:
	docker build -t $(IMAGE) -f docker/Dockerfile .

install: build
	cp $(BINARY) /usr/local/bin/toolctl

clean:
	rm -rf bin
