package http

import (
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	defaultStopTimeout = time.Minute
	defaultKillTimeout = time.Minute
)

//////////////////////////////////////////////////////////////////////////////////
//////////////////////////////////////////////////////////////////////////////////
// THIS IS NO LONGER USED SINCE THE GRACEFUL SHUTDOWN WAS IMPLEMENTED IN GO 1.8 //
//////////////////////////////////////////////////////////////////////////////////
//////////////////////////////////////////////////////////////////////////////////

// A Server allows encapsulates the process of accepting new connections and
// serving them, and gracefully shutting down the listener without dropping
// active connections.
type Server interface {

	// Wait waits for the serving loop to finish. This will happen when Stop is
	// called, at which point it returns no error, or if there is an error in the
	// serving loop. You must call Wait after calling Serve or ListenAndServe.
	Wait() error

	// Stop stops the listener. It will block until all connections have been
	// closed.
	Stop() error
}

// HTTP defines the configuration for serving a http.Server. Multiple calls to
// Serve or ListenAndServe can be made on the same HTTP instance. The default
// timeouts of 1 minute each result in a maximum of 2 minutes before a Stop()
// returns.
type HTTP struct {

	// StopTimeout is the duration before we begin force closing connections.
	// Defaults to 1 minute.
	StopTimeout time.Duration

	// KillTimeout is the duration before which we completely give up and abort
	// even though we still have connected clients. This is useful when a large
	// number of client connections exist and closing them can take a long time.
	// Note, this is in addition to the StopTimeout. Defaults to 1 minute.
	KillTimeout time.Duration
}

// Serve provides the low-level API which is useful if you're creating your own
// net.Listener.
func (h HTTP) Serve(s *http.Server, l net.Listener) Server {
	stopTimeout := h.StopTimeout
	if stopTimeout == 0 {
		stopTimeout = defaultStopTimeout
	}
	killTimeout := h.KillTimeout
	if killTimeout == 0 {
		killTimeout = defaultKillTimeout
	}
	ss := &server{
		stopTimeout:  stopTimeout,
		killTimeout:  killTimeout,
		oldConnState: s.ConnState,
		listener:     l,
		server:       s,
		serveDone:    make(chan struct{}),
		serveErr:     make(chan error, 1),
		new:          make(chan net.Conn),
		active:       make(chan net.Conn),
		idle:         make(chan net.Conn),
		closed:       make(chan net.Conn),
		stop:         make(chan chan struct{}),
		kill:         make(chan chan struct{}),
	}
	s.ConnState = ss.connState
	go ss.manage()
	go ss.serve()
	return ss
}

// server manages the serving process and allows for gracefully stopping it.
type server struct {
	stopTimeout time.Duration // The stop timeout
	killTimeout time.Duration // The kill timeout

	oldConnState func(net.Conn, http.ConnState) // handler for old connection
	server       *http.Server                   // the underlying server
	serveDone    chan struct{}                  // channel for sending done signal
	serveErr     chan error                     // channel for sending server errors
	listener     net.Listener                   // the listener used for this server

	new    chan net.Conn
	active chan net.Conn
	idle   chan net.Conn
	closed chan net.Conn
	stop   chan chan struct{}
	kill   chan chan struct{}

	stopOnce sync.Once // ensure that the server can only be stopped once
	stopErr  error     // The error that can occur on stop
}

// connState will manage the connection state and send the message to the
// appropriate channel on the server
func (s *server) connState(c net.Conn, cs http.ConnState) {
	if s.oldConnState != nil {
		s.oldConnState(c, cs)
	}

	switch cs {
	case http.StateNew:
		s.new <- c
	case http.StateActive:
		s.active <- c
	case http.StateIdle:
		s.idle <- c
	case http.StateHijacked, http.StateClosed:
		s.closed <- c
	}
}

// manage will handle the channels messaging for the server
func (s *server) manage() {

	// Defer the closing of the channels
	// This will happen after the server is closed
	defer func() {
		close(s.new)
		close(s.active)
		close(s.idle)
		close(s.closed)
		close(s.stop)
		close(s.kill)
	}()

	var stopDone chan struct{}

	// Hold the connection state for each of the current connections
	conns := map[net.Conn]http.ConnState{}
	for {
		select {
		case c := <-s.new:
			conns[c] = http.StateNew
		case c := <-s.active:
			conns[c] = http.StateActive
		case c := <-s.idle:
			conns[c] = http.StateIdle

			// If we're already stopping, close it
			if stopDone != nil {
				c.Close()
			}
		case c := <-s.closed:
			delete(conns, c)

			// If we're waiting to stop and are all empty, we just closed the last
			// connection and we're done.
			if stopDone != nil && len(conns) == 0 {
				close(stopDone)
				return
			}
		case stopDone = <-s.stop:

			// If we're already all empty, we're already done
			if len(conns) == 0 {
				close(stopDone)
				return
			}

			// Close current idle connections right away
			for c, cs := range conns {
				if cs == http.StateIdle {
					c.Close()
				}
			}

		// Continue the loop and wait for all the ConnState updates which will
		// eventually close(stopDone) and return from this goroutine.

		case killDone := <-s.kill:

			// Force close all connections
			for c := range conns {
				c.Close()
			}

			// don't block the kill.
			close(killDone)

			// continue the loop and we wait for all the ConnState updates and will
			// return from this goroutine when we're all done. otherwise we'll try to
			// send those ConnState updates on closed channels.
		}
	}
}

// serve will start the underlying server using the listener
func (s *server) serve() {
	s.serveErr <- s.server.Serve(s.listener)
	close(s.serveDone)
	close(s.serveErr)
}

// Wait will wait for the close to occur by waiting for the
func (s *server) Wait() error {
	if err := <-s.serveErr; !isUseOfClosedError(err) {
		return err
	}
	return nil
}

// Stop will attempt to stop the server
func (s *server) Stop() error {
	s.stopOnce.Do(func() {

		// First disable keep-alive for new connections
		s.server.SetKeepAlivesEnabled(false)

		// Then close the listener so new connections can't connect
		closeErr := s.listener.Close()
		<-s.serveDone

		// Then trigger the background goroutine to stop and wait for it
		stopDone := make(chan struct{})
		s.stop <- stopDone

		// Wait for the stop
		select {
		case <-stopDone:
			// Drop out

		case <-time.After(s.stopTimeout):

			// Stop timed out so go in an kill'em buggers
			killDone := make(chan struct{})
			s.kill <- killDone

			// Wait for response
			<-killDone
		}

		// Ensure that the error is not a closed error
		if closeErr != nil && !isUseOfClosedError(closeErr) {
			s.stopErr = closeErr
		}
	})
	return s.stopErr
}

// isUseOfClosedError will determine if the error is because of a closed network connection
func isUseOfClosedError(err error) bool {
	if err == nil {
		return false
	}
	if opErr, ok := err.(*net.OpError); ok {
		err = opErr.Err
	}
	return err.Error() == "use of closed network connection"
}
