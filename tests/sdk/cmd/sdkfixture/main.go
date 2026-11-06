// Command sdkfixture starts what the Node and Python SDK tests run against, prints where everything is as one line of
// JSON, and stops it all when its standard input closes. So a test run cannot leave servers behind, however it ends.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"elnova-streamkit.local/tests/sdk/fixture"
)

func main() {
	workers := flag.Bool("workers", false, "also start the egress and ingress workers")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	f, err := fixture.Start(ctx, fixture.Options{Workers: *workers, Log: os.Stderr})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sdkfixture:", err)
		os.Exit(1)
	}
	defer f.Stop()
	if err := json.NewEncoder(os.Stdout).Encode(f.Info); err != nil {
		fmt.Fprintln(os.Stderr, "sdkfixture:", err)
		return
	}
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		stop()
	}()
	<-ctx.Done()
}
