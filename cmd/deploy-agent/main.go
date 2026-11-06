// Command deploy-agent runs permanently on a Docker host: it receives signed deploy
// webhooks (e.g. from GitHub Actions), pulls the new Docker image and does a
// rolling deploy of the containers (HTTP services: update an nginx upstream used as
// a load balancer for zero downtime; workers: replace the container once it is
// stable), rolling back if the health check fails. See README.md for setup.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pixalabteam/deploy-agent/internal/agent"
)

func main() {
	cfg, err := agent.Load()
	if err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	state, err := agent.LoadState(cfg.StateFile)
	if err != nil {
		log.Fatalf("read state file: %v", err)
	}

	srv := agent.NewServer(cfg, state)
	if err := srv.RestoreQueue(); err != nil {
		log.Fatalf("restore job queue: %v", err)
	}
	httpSrv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: srv.Handler(),
	}

	go func() {
		log.Printf("deploy-agent listening on %s", cfg.ListenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("shutting down deploy-agent...")
	// Never cancel a /deploy request mid-flight (it may be halfway through a
	// rolling deploy of one instance); only wait for net/http's graceful
	// shutdown. Each deploy has its own timeout (see server.go).
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
