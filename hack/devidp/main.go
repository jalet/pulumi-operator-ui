// Command devidp runs the oidctest stub IdP for local development. It approves every
// login as user "dev" in group "dev". Never expose it beyond localhost.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jalet/pulumi-operator-ui/internal/auth/oidctest"
)

func main() {
	p, err := oidctest.NewServer(oidctest.Options{
		Addr:         "127.0.0.1:5556",
		ClientID:     "pou",
		ClientSecret: "dev-secret",
		Claims:       map[string]any{"sub": "dev", "email": "dev@localhost", "groups": []string{"dev"}},
		RedirectURIs: []string{"http://localhost:8080/auth/callback"},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("dev IdP issuer: %s (client pou / dev-secret)\n", p.URL)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	p.Close()
}
