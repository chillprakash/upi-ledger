// Command upi-ledger serves the app's payment API over a simulated UPI
// network (switch, banks, RBI), on Postgres or in memory.
//
//	go run ./cmd/upi-ledger -db postgres://upi@127.0.0.1:5432/upi_ledger -addr :8080
//	curl -H 'Idempotency-Key: k1' -d '{"payer":"user0.0@bank0","payee":"user1.0@bank1","amount":"100"}' localhost:8080/payments
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/chillprakash/upi-ledger/internal/world"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	db := flag.String("db", "", "Postgres URL (empty: in memory)")
	schema := flag.String("schema", "upi", "schema prefix")
	faults := flag.Float64("faults", 0, "share of network calls that fail (0-1)")
	speed := flag.Float64("speed", 1, "simulated seconds per real second")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	w, err := world.Build(ctx, world.Config{DatabaseURL: *db, Schema: *schema, FaultRate: *faults, Speed: *speed, Seed: uint64(time.Now().UnixNano())})
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close(context.Background(), false)
	go w.Run(ctx)
	srv := &http.Server{Addr: *addr, Handler: w.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background()) //nolint:errcheck
	}()
	log.Printf("upi-ledger on http://%s (users like %s)", *addr, w.Users[0].VPA)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
