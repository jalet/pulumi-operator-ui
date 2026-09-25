// Command pulumi-operator-ui serves a read-only UI for Pulumi Kubernetes Operator stacks.
package main

import (
	"fmt"
	"os"

	"github.com/jalet/pulumi-operator-ui/internal/config"
)

func main() {
	if _, err := config.Parse(os.Args[1:], os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
