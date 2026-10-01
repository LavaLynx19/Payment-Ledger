// Package metrics logs expvar values so the harness can read them from
// container logs. The worker has no HTTP server, and run.sh reads both
// processes the same way.
package metrics

import (
	"context"
	"expvar"
	"log"
	"time"
)

// LogEvery logs "<name>: <json>" every interval until ctx ends, then once more,
// so a graceful stop always leaves the final totals as the last line.
func LogEvery(ctx context.Context, name string, v expvar.Var, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("%s: %s", name, v.String())
			return
		case <-t.C:
			log.Printf("%s: %s", name, v.String())
		}
	}
}
