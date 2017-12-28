# gonet

[![Go Report Card](https://goreportcard.com/badge/github.com/landonia/gograceful)](https://goreportcard.com/report/github.com/landonia/gograceful)
[![GoDoc](https://godoc.org/github.com/landonia/gograceful?status.svg)](https://godoc.org/github.com/landonia/gograceful)

Allows you to gracefully shutdown HTTP connections as opposed to cutting them dead.

## Overview

Go is great, but not being able to gracefully shutdown an application and any current connections is a bit of a pain, especially when you want to add in graceful shutdown (for restarts etc). By using this package, you are able to restart an application gracefully, launching a new process that takes over the current file descriptors, sends a shutdown to the original process that then finishes it's clients before shutting down. Any new requests will be handled by the new process. Upon receiving the correct shutdown call it will spawn a new process handing off the current socket file descriptors to the new process.

This did use a custom implementation of an HTTP server that could manage the connection state, but since
`HTTP.Shutdown` has been added in 1.8, I decided to integrate this.

For an example, see the cmd/example.go implementation.

## Installation

With a healthy Go Language installed, simply run `go get github.com/landonia/gograceful`

## Out of Box Example

The example will not run with `go run` as it will create a different tmp file that it executes.
You need to build the file using `go build $GOPATH/src/github.com/landonia/gograceful/cmd/example.go` and then run `./example`.

To test the graceful restart you will have to find the process ID `ps a | grep example` and then
issue `kill -SIGUSR2 {process_id}`. To test that it fully works you can find the current server uuid
by going to http://localhost:8081/uuid and then set off a long running request using http://localhost:8081/delayed. Immediately after firing the request, issue the kill command from above. If you go to http://localhost:8081/uuid you will see a new uuid but the last delayed request will show the previous uuid.

Currently, the example only shows a HTTP server in operation, but I will soon be adding examples
for TCP and WebSocket connections.

## About

gograceful was written by [Landon Wainwright](http://www.landotube.com) | [GitHub](https://github.com/landonia).

Follow me on [Twitter @landotube](http://www.twitter.com/landotube)! Although I don't really tweet much tbh.
