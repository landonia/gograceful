package gograceful

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/landonia/golog"
)

// Don't make the caller import syscall.
const (
	SIGINT  = syscall.SIGINT
	SIGQUIT = syscall.SIGQUIT
	SIGTERM = syscall.SIGTERM
	SIGUSR2 = syscall.SIGUSR2

	// Used to indicate a graceful restart in the new process.
	envPPIDKey  = "GOGRACEFUL_PPID"
	envCountKey = "LISTEN_FDS"
)

// In order to keep the working directory the same as when we started we record
// it at startup.
var originalWD, _ = os.Getwd()

// Graceful will store the listeners that have been created so that
// they can be inherited on a restart
type Graceful struct {

	// Ensure that the same listener is potentially not added n times
	mutex sync.Mutex

	// Only load the inherited listeners once
	inheritOnce sync.Once

	// The log to use
	log golog.Logger

	// The error that is returned from the interit method
	inheritOnceError error

	// If there are multiple services to start up, the signal will not be sent
	// until they have all registered
	serviceCount sync.WaitGroup

	// listeners will hold the current net listeners being managed by
	// this graceful collection.
	listeners []net.Listener

	// The old listeners that have been inherited from the previous process
	old []net.Listener

	// used in tests to override the default behavior of starting from fd 3.
	fdStart int
}

// WithConfiguration defines a function type to configure Graceful
type WithConfiguration func(*Graceful) error

// WithServiceCount will configure Graceful with the number of services that
// will initialise before it is ready (see #Ready())
func WithServiceCount(services int) WithConfiguration {
	return func(gf *Graceful) error {
		return gf.SetServiceCount(services)
	}
}

// WithLogger will override the logger
func WithLogger(log golog.Logger) WithConfiguration {
	return func(gf *Graceful) error {
		gf.log = log
		return nil
	}
}

// New will create a new Graceful wrapper. Note only one is expected per application
func New(withConfs ...WithConfiguration) (gf *Graceful, err error) {
	gf = &Graceful{log: &golog.EmptyLogger{}}

	// Add the WithConfiguration functions
	for _, withConf := range withConfs {
		if err = withConf(gf); err != nil {
			return
		}
	}
	return
}

/////////////////////////
// LISTENER EXTRACTION //
/////////////////////////

// activeListeners returns a snapshot copy of the active listeners.
func (gf *Graceful) activeListeners() ([]net.Listener, error) {
	gf.mutex.Lock()
	defer gf.mutex.Unlock()
	ls := make([]net.Listener, len(gf.listeners))
	copy(ls, gf.listeners)
	return ls, nil
}

// inherit will try to recover the connections from the previous process
func (gf *Graceful) inherit() error {
	gf.inheritOnce.Do(func() {
		gf.mutex.Lock()
		defer gf.mutex.Unlock()
		countStr := os.Getenv(envCountKey)
		if countStr == "" {
			return
		}
		count, err := strconv.Atoi(countStr)
		if err != nil {
			gf.inheritOnceError = fmt.Errorf("found invalid count value: %s=%s", envCountKey, countStr)
			return
		}

		// In tests this may be overridden.
		fdStart := gf.fdStart
		if fdStart == 0 {
			// In normal operations if we are inheriting, the listeners will begin at fd 3.
			fdStart = 3
		}

		for i := fdStart; i < fdStart+count; i++ {
			file := os.NewFile(uintptr(i), "listener")
			l, err := net.FileListener(file)
			if err != nil {
				file.Close()
				gf.inheritOnceError = fmt.Errorf("error inheriting socket fd %d: %s", i, err)
				return
			}
			if err := file.Close(); err != nil {
				gf.inheritOnceError = fmt.Errorf("error closing inherited socket fd %d: %s", i, err)
				return
			}
			gf.old = append(gf.old, l)
		}

		// Remove the environment variables
		os.Unsetenv(envCountKey)
	})
	return gf.inheritOnceError
}

/////////////////////////////////////////////////
// ALTERNATIVE WRAPPERS FOR GRACEFUL LISTENERS //
/////////////////////////////////////////////////

// Listen announces on the local network address laddr. The network net must be
// a stream-oriented network: "tcp", "tcp4", "tcp6", "unix" or "unixpacket". It
// returns an inherited net.Listener for the matching network and address, or
// creates a new one using net.Listen.
func (gf *Graceful) Listen(nett, laddr string) (net.Listener, error) {
	switch nett {
	default:
		return nil, net.UnknownNetworkError(nett)
	case "tcp", "tcp4", "tcp6":
		addr, err := net.ResolveTCPAddr(nett, laddr)
		if err != nil {
			return nil, err
		}
		return gf.ListenTCP(nett, addr)
	case "unix", "unixpacket", "invalid_unix_net_for_test":
		addr, err := net.ResolveUnixAddr(nett, laddr)
		if err != nil {
			return nil, err
		}
		return gf.ListenUnix(nett, addr)
	}
}

// ListenTCP announces on the local network address laddr. The network net must
// be: "tcp", "tcp4" or "tcp6". It returns an inherited net.Listener for the
// matching network and address, or creates a new one using net.ListenTCP.
func (gf *Graceful) ListenTCP(nett string, laddr *net.TCPAddr) (*net.TCPListener, error) {
	if err := gf.inherit(); err != nil {
		return nil, err
	}
	gf.mutex.Lock()
	defer gf.mutex.Unlock()

	// look for an inherited listener
	for i, l := range gf.old {

		// Once the listener has been used it will be shifted to the current listeners
		if l == nil {
			continue
		}
		switch l.(type) {
		case *net.TCPListener:
		default:
			return nil, fmt.Errorf("inherited file descriptor is %T not *net.TCPListener or *net.UnixListener", l)
		}

		// Check if these are equal
		if isSameAddr(l.Addr(), laddr) {
			gf.old[i] = nil
			gf.listeners = append(gf.listeners, l)
			return l.(*net.TCPListener), nil
		}
	}

	// Otherwise we need to make a fresh listener
	l, err := net.ListenTCP(nett, laddr)
	if err != nil {
		return nil, err
	}
	gf.listeners = append(gf.listeners, l)
	return l, nil
}

// ListenUnix announces on the local network address laddr. The network net
// must be a: "unix" or "unixpacket". It returns an inherited net.Listener for
// the matching network and address, or creates a new one using net.ListenUnix.
func (gf *Graceful) ListenUnix(nett string, laddr *net.UnixAddr) (*net.UnixListener, error) {
	if err := gf.inherit(); err != nil {
		return nil, err
	}
	gf.mutex.Lock()
	defer gf.mutex.Unlock()

	// look for an inherited listener
	for i, l := range gf.old {

		// Once the listener has been used it will be shifted to the current listeners
		if l == nil {
			continue
		}
		switch l.(type) {
		case *net.UnixListener:
		default:
			return nil, fmt.Errorf("inherited file descriptor is %T not *net.UnixListener", l)
		}

		// Check if these are equal
		if isSameAddr(l.Addr(), laddr) {
			gf.old[i] = nil
			gf.listeners = append(gf.listeners, l)
			return l.(*net.UnixListener), nil
		}
	}

	// make a fresh listener
	l, err := net.ListenUnix(nett, laddr)
	if err != nil {
		return nil, err
	}
	gf.listeners = append(gf.listeners, l)
	return l, nil
}

////////////////////////////
// SPAWNING A NEW PROCESS //
////////////////////////////

// restart starts a new process passing it the active listeners. It
// doesn't fork, but starts a new process using the same environment and
// arguments as when it was originally started. This allows for a newly
// deployed binary to be started. It returns the pid of the newly started
// process when successful.
//noinspection GoDeferInLoop
func (gf *Graceful) restart() (int, error) {
	listeners, err := gf.activeListeners()
	if err != nil {
		return 0, err
	}

	// Extract the fds from the listeners.
	files := make([]*os.File, len(listeners))
	for i, l := range listeners {
		files[i], err = l.(filer).File()
		if err != nil {
			return 0, err
		}
		defer files[i].Close()
	}

	// Use the original binary location. This works with symlinks such that if
	// the file it points to has been changed we will use the updated symlink.
	argv0, err := locatePath()
	if err != nil {
		return 0, err
	}

	// Add the PID of this process as the parent
	if err = os.Setenv(envPPIDKey, fmt.Sprint(syscall.Getpid())); nil != err {
		return 0, err
	}

	// Add the count of listeners
	if err = os.Setenv(envCountKey, fmt.Sprint(len(listeners))); nil != err {
		return 0, err
	}

	// Pass on the environment and replace the old count key with the new one.
	allFiles := append([]*os.File{os.Stdin, os.Stdout, os.Stderr}, files...)
	process, err := os.StartProcess(argv0, os.Args, &os.ProcAttr{
		Dir:   originalWD,
		Env:   os.Environ(),
		Files: allFiles,
	})
	if err != nil {
		return 0, err
	}
	return process.Pid, nil
}

// filer will return a File os pointer
type filer interface {
	File() (*os.File, error)
}

//////////////////////////////////////
// HANDLING FROM PROCESS TO PROCESS //
//////////////////////////////////////

// SetServiceCount allows you to specify the number of services that have to start before calling ready
// For example, if you have 3 listeners in the application that have to be started before the service
// is ready, then you can set the count. You can then call ready after initialising the service
// but the signal will not be sent until the number of calls matches the number of services.
func (gf *Graceful) SetServiceCount(count int) (err error) {
	if count <= 0 {
		err = fmt.Errorf("you must provide a positive count")
	}
	gf.serviceCount.Add(count)
	return
}

// ServiceReady should be called when one of the services are ready.
// When all the services have been registered as ready the Wait method will
// return, in turn sending a SIGINT to the parent process
func (gf *Graceful) ServiceReady() {

	// Mark the service as done
	gf.serviceCount.Done()
}

// TWO Stage shutdown using signals
// 1 -> SIGUSR2
//	---> Cause a new process to be spawned
// <-------  SIGINT - New Proc calls Ready()
// 2 -> SIGINT
//	---> Causes the process to shutdown cleanly and exit (as they do now)

// Ready to signal that the current command has started and it is in a ready
// state. If another process has spawned this, it will send the final shutdown to
// that process (that should be blocking on Wait()) to shutdown.
func (gf *Graceful) Ready() error {

	// Do not pass until the services are ready
	gf.serviceCount.Wait()

	// Get the parent process PID
	pidStr := os.Getenv(envPPIDKey)
	if pidStr == "" {
		return nil
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return err
	}

	// Kill the parent process
	go syscall.Wait4(pid, nil, 0, nil)
	gf.log.Trace("Sending signal %s to process %d", syscall.SIGINT, pid)
	return syscall.Kill(pid, syscall.SIGINT)
}

// Wait is called as the last blocking function in a command
// When the application is running, the cmd will wait on this
// for a shutdown signal where it will then gracefully shutdown
// and exit the process
func (gf *Graceful) Wait() error {

	// The channel where the os signals will be sent
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR2)
	forked := false
	for {
		sig := <-ch
		switch sig {

		// We have received the signal to shutdown
		case syscall.SIGINT, syscall.SIGTERM:
			gf.log.Trace("Sig: Shutdown: %d", syscall.Getpid())
			return nil

		// The SIGUSR2 means we should start the new process
		// When the new process calls ready it will send
		// a SIGINT to the new process
		case syscall.SIGUSR2:
			gf.log.Trace("Sig: Restart: %d", syscall.Getpid())
			if !forked {
				newPID, err := gf.restart()
				if err != nil {
					return err
				}
				gf.log.Trace("Started new PID: %d", newPID)
				forked = true
			}
		}
	}
}

////////////////////////////
// HTTP WRAPPER FUNCTIONS //
////////////////////////////

// Server encapsulates a standard HTTP server that can be gracefully
// shutdown by allowing the connection to be inherited by the new process
type Server struct {
	g           *Graceful    // The graceful manager
	http        *http.Server // The underlying http server
	listener    net.Listener // The listener to use
	serveErr    chan error   // The error whilst listening
	shutdownErr chan error   // The error upon waiting for shutdown
	stopOnce    sync.Once    // ensure that the server can only be stopped once
}

// NewServer will take a standard http.Server and wrap it to make it
// capable of being graceful
func (gf *Graceful) NewServer(server *http.Server) *Server {
	return &Server{
		g:           gf,
		http:        server,
		serveErr:    make(chan error, 1),
		shutdownErr: make(chan error, 1),
	}
}

// RegisterOnShutdown registers a function to call on Shutdown.
// This can be used to gracefully shutdown connections that have
// undergone NPN/ALPN protocol upgrade or that have been hijacked.
// This function should start protocol-specific graceful shutdown,
// but should not wait for shutdown to complete.
func (s *Server) RegisterOnShutdown(f func()) error {
	if s.http == nil {
		return fmt.Errorf("No server connection")
	}

	// Pass to the underlying HTTP server
	s.http.RegisterOnShutdown(f)
	return nil
}

// ListenAndServe will start the server and start serving the requests
// it will call ServiceReady when the server is started
func (s *Server) ListenAndServe() error {

	// Get the TCP connection to use for this server
	l, err := s.g.Listen("tcp", s.http.Addr)
	if err != nil {
		return err
	}
	if s.http.TLSConfig != nil {
		l = tls.NewListener(l, s.http.TLSConfig)
	}
	s.listener = l

	// Start the server using the graceful listener
	go func() { s.serveErr <- s.http.Serve(s.listener) }()
	s.g.ServiceReady()
	return nil
}

// Stop the server
func (s *Server) Stop(wait time.Duration) (context.CancelFunc, error) {
	if s.http == nil {
		return nil, fmt.Errorf("No server connection")
	}

	// We need to gracefully shutdown the HTTP server
	var ctx context.Context
	var cancel context.CancelFunc
	s.stopOnce.Do(func() {
		ctx, cancel = context.WithTimeout(context.Background(), wait)

		// Attempt to shutdown the server
		go func() {
			// Release any resources if the server shutsdown before expected time
			defer cancel()
			s.shutdownErr <- s.http.Shutdown(ctx)
		}()
	})
	return cancel, nil
}

// Wait for the server to shutdown
func (s *Server) Wait() error {
	if s.http == nil {
		return fmt.Errorf("No server connection")
	}

	// Wait for the server to shutdown
	if err := <-s.shutdownErr; !IsErrShutdown(err) {
		return err
	}
	return nil
}

//////////////////////
// HELPER FUNCTIONS //
//////////////////////

// IsErrClosing will test whether an error is equivalent to net.errClosing as returned by
// Accept during a graceful exit.
func IsErrClosing(err error) bool {
	if opErr, ok := err.(*net.OpError); ok {
		err = opErr.Err
	}
	return "use of closed network connection" == err.Error()
}

// IsErrShutdown will return true if the error is because of a shutdown
func IsErrShutdown(err error) bool {
	return err == http.ErrServerClosed
}

// locatePath will locate the path to the app
func locatePath() (binary string, err error) {

	// Locate the path to this process
	binary, err = exec.LookPath(os.Args[0])
	if err == nil {
		_, err = os.Stat(binary)
	}
	return
}

// isSameAddr will determine if the two addressed are equal
func isSameAddr(a1, a2 net.Addr) bool {
	if a1.Network() != a2.Network() {
		return false
	}
	a1s := a1.String()
	a2s := a2.String()
	if a1s == a2s {
		return true
	}

	// This allows for ipv6 vs ipv4 local addresses to compare as equal. This
	// scenario is common when listening on localhost.
	const ipv6prefix = "[::]"
	a1s = strings.TrimPrefix(a1s, ipv6prefix)
	a2s = strings.TrimPrefix(a2s, ipv6prefix)
	const ipv4prefix = "0.0.0.0"
	a1s = strings.TrimPrefix(a1s, ipv4prefix)
	a2s = strings.TrimPrefix(a2s, ipv4prefix)
	return a1s == a2s
}
