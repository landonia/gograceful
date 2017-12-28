package main

import (
	"net/http"
	"time"

	"github.com/landonia/gograceful"
	"github.com/landonia/golog"
	"github.com/landonia/util"
)

const (
	ADDR = ":8081"
)

var (
	log = golog.NewPrettyZeroLogger("graceful.demo")
)

// An example of setting up a gograceful app
// In order to see it restart you have to send the SIGUSR2 to the process ID
// which will spawn a new process, take over the connections and then shutdown
// the original.
// This will not work if using `go run` as it uses a different tmp file. You will
// need to use `go build example.go` and then `./example` to start the example file
// Once running, you then need to issue a `kill -SIGUSR2 {process_id}` for it
// to gracefully spin up a new process and then shutdown.
func main() {
	uuid := util.GenerateUUID()

	// Create a new gograceful instance to track the services
	gg, err := gograceful.New(1)
	if err != nil {
		log.Fatal("Could not create new graceful state... %s", err.Error())
	}

	// Setup a new HTTP server
	mx := http.NewServeMux()
	hs := &http.Server{
		Addr:    ADDR,
		Handler: mx,
	}

	// Instantly tell the uuid for this server
	mx.HandleFunc("/uuid", func(rw http.ResponseWriter, req *http.Request) {
		rw.Write([]byte("Served from: " + uuid))
	})

	// This will cause a delay allowing the system to be restarted
	// If you send the SIGUSR2 during this event, you will see that it is finished
	// by the previous process. Any new requests (using /im) will be served from the new
	// process
	mx.HandleFunc("/delayed", func(rw http.ResponseWriter, req *http.Request) {
		log.Info("Serving delayed from: %s", uuid)
		time.Sleep(10 * time.Second)
		log.Info("Served delayed from: %s", uuid)
		rw.Write([]byte("Served from: " + uuid))
	})

	// Start the server
	log.Info("Starting server using address: %s", ADDR)

	// Setup a graceful HTTP server
	gs := gg.NewServer(hs)

	// Start the webserver
	go func() { gs.ListenAndServe() }()

	// The service has now finished
	// This will block until the exit signal is sent
	log.Info("Ready: %s", uuid)
	gg.Ready()

	// Now we can wait
	log.Info("Waiting for shutdown signal: %s", uuid)
	gg.Wait()

	// Now gracefully shutdown the services
	log.Info("Stopping HTTP server: %s", uuid)
	gs.Stop(20 * time.Second)

	// Wait for it to end
	log.Info("Waiting for HTTP server: %s", uuid)
	gs.Wait()

	// Exit
	log.Info("Exiting: %s", uuid)
}
