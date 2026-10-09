// Command supavise turns a Linux machine into a multi-project Supabase.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/supavise/supavise/internal/nodeupgrade"
)

func main() {
	hardenProcess()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := rootCmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "supavise:", followerHint(err))
		// `supavise upgrade` and `supavise rollback` have their own exit statuses (2, 3 and 4).
		var f *nodeupgrade.Failure
		if errors.As(err, &f) {
			os.Exit(f.Code)
		}
		os.Exit(1)
	}
}
