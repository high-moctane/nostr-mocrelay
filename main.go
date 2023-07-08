package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gobwas/ws"
	"github.com/google/uuid"
)

var logStdout = log.New(os.Stdout, "I: ", log.Default().Flags())
var logStderr = log.New(os.Stderr, "E: ", log.Default().Flags())

func main() {
	logStderr.Printf("server start")
	if err := Run(context.Background()); err != nil {
		logStderr.Fatalf("server terminated with error: %v", err)
	}
	logStderr.Printf("server stop")
}

func Run(ctx context.Context) error {
	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt, os.Kill, syscall.SIGPIPE)
	defer stop()

	router := new(Router)
	db := NewDB(5)

	mux := http.NewServeMux()

	mux.HandleFunc("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(sigCtx)

		connID := uuid.NewString()

		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			log.Printf("[%v]: failed to upgrade http: %v", connID, err)
			return
		}
		defer conn.Close()

		if err := HandleWebsocket(r.Context(), r, connID, conn, router, db); err != nil {
			log.Printf("[%v]: websocket error: %v", connID, err)
		}
	}))

	srv := &http.Server{
		Addr:    "127.0.0.1:8234",
		Handler: mux,
	}

	go func() {
		<-sigCtx.Done()

		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		srv.Shutdown(ctx)
	}()

	return srv.ListenAndServe()
}
