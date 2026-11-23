.DEFAULT_GOAL := help

GO ?= go
URL ?=
FORMAT ?= mp4
SINGLE ?= false
JOBS ?= 3
QUALITY ?= 1080
OUT ?= ./downloads
ARCHIVE ?=
RETRIES ?= 5
RETRY_DELAY ?= 5s
SOCKET_TIMEOUT ?= 60
NETWORK_RETRIES ?= 3
FRAGMENT_RETRIES ?= 3
JS_RUNTIMES ?=
VERBOSE ?= false
UPDATES ?= daily
UPDATE_CHANNEL ?= nightly
UPDATE_TIMEOUT ?= 2m
UPDATE_SUDO ?= false
UPDATE_CACHE ?=

# Export values rather than interpolating URLs/paths into shell commands.
export URL FORMAT SINGLE JOBS QUALITY OUT ARCHIVE RETRIES RETRY_DELAY
export SOCKET_TIMEOUT NETWORK_RETRIES FRAGMENT_RETRIES JS_RUNTIMES VERBOSE
export UPDATES UPDATE_CHANNEL UPDATE_TIMEOUT UPDATE_SUDO UPDATE_CACHE

.PHONY: help build download update update-system check

help:
	@echo 'make download URL="..."     Download a video or playlist (default MP4; FORMAT=mp3 for audio)'
	@echo 'make update                 Update yt-dlp and Deno; prompt via sudo when needed'
	@echo 'make update-system          Upgrade Ubuntu FFmpeg packages via sudo'
	@echo 'make check                  Run race tests, vet, and formatting checks'
	@echo 'See README.md for one download command with every optional parameter.'

build:
	@mkdir -p bin
	$(GO) build -o bin/ytgo .

download: build
	@test -n "$${URL}" || { echo 'Set URL="https://..."'; exit 2; }
	@case "$${FORMAT}" in mp3|mp4) ;; *) echo 'FORMAT must be mp3 or mp4'; exit 2;; esac
	./bin/ytgo -mode "$${FORMAT}" -single="$${SINGLE}" -j "$${JOBS}" -q "$${QUALITY}" \
		-o "$${OUT}" -archive "$${ARCHIVE}" -retries "$${RETRIES}" -retry-delay "$${RETRY_DELAY}" \
		-js-runtimes "$${JS_RUNTIMES}" -v="$${VERBOSE}" \
		-socket-timeout "$${SOCKET_TIMEOUT}" -network-retries "$${NETWORK_RETRIES}" \
		-fragment-retries "$${FRAGMENT_RETRIES}" -updates "$${UPDATES}" \
		-update-channel "$${UPDATE_CHANNEL}" -update-timeout "$${UPDATE_TIMEOUT}" \
		-update-sudo="$${UPDATE_SUDO}" -update-cache "$${UPDATE_CACHE}" "$${URL}"

update: UPDATE_SUDO = true
update: build
	./bin/ytgo -update-only -js-runtimes "$${JS_RUNTIMES}" \
		-update-channel "$${UPDATE_CHANNEL}" -update-timeout "$${UPDATE_TIMEOUT}" \
		-update-sudo="$${UPDATE_SUDO}" -update-cache "$${UPDATE_CACHE}"

update-system:
	sudo apt-get update
	sudo apt-get install --only-upgrade ffmpeg

check:
	$(GO) test -race ./...
	$(GO) vet ./...
	@test -z "$$(gofmt -l *.go)" || { echo 'Run gofmt -w *.go'; exit 1; }
