# Go In Action — Code Examples

Source code for the "Go In Action" book, organized by chapter. Each chapter
contains standalone Go programs (listings) demonstrating Go concepts.

## Running in Base44

This repo is a collection of Go examples, not a web app. A showcase web server
(`showcase/`) was added so the examples are browsable and runnable from the
preview. It serves a single-page UI on port 3000 that lists every example,
shows its source, and runs it on demand.

```bash
docker compose -f docker-compose.base44.yml up -d --build
```

## How it works

- `go.mod` at the repo root declares the module `github.com/goinaction/code`,
  which makes all internal GOPATH-style imports resolve under Go modules.
- The showcase server (`showcase/main.go`) walks the repo at startup to find
  every directory containing a `package main` with a `func main()`.
- When you click "Run", the server builds the example (`go build -o`) and
  executes the resulting binary with a 10-second timeout. Build and run output
  is streamed back to the browser.
- Some examples (e.g. `chapter9/listing17`) start an HTTP server and block
  forever — those are killed after the timeout and the output collected so
  far is shown.
- `chapter2/sample` fetches external RSS feeds and may show network errors
  if the sandbox has no outbound network access.

## No external dependencies

All examples use only the Go standard library and internal `goinaction`
packages. No credentials or external services are required.

## Adding examples

New examples are discovered automatically — just add a directory with a
`package main` containing `func main()`. The showcase re-scans on each
server restart.
