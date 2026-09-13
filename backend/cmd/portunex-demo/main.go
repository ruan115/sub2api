// Command portunex-demo runs synthetic recovery modules, never the production app.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/portunex/app"
)

func main() {
	port := flag.Int("port", 18090, "Loopback-only demo port")
	flag.Parse()
	if *port < 1 || *port > 65535 {
		log.Fatal("invalid demo port")
	}
	handler, err := app.NewDemo(app.Options{})
	if err != nil {
		log.Fatal("cannot initialize synthetic demo")
	}
	server := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", *port), Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 * 1024}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("SYNTHETIC DEMO only: http://%s; no production routes or credentials", server.Addr)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal("demo listener failed")
	}
}
