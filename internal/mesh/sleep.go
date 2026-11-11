package mesh

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"
)

const (
	sleepCheck = time.Second     // how often the node looks whether the machine slept
	sleepMin   = 2 * time.Second // a shorter sleep goes unsaid
)

// watchSleep says each time this machine slept, until ctx is done. While
// it sleeps nothing arrives or leaves, so the UDP paths fall silent on
// both sides and the links may too; the log and the traces of the UDP
// paths then say why.
func (n *Node) watchSleep(ctx context.Context) {
	tick := time.NewTicker(sleepCheck)
	defer tick.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := time.Now()
		if d := sleptFor(last, now); d >= sleepMin {
			n.slept(d)
		}
		last = now
	}
}

// sleptFor returns for how long the machine slept between two readings of
// time.Now: the time that the wall clock counted and the monotonic one did
// not, as the monotonic clock stops in sleep on macOS and Linux. A wall
// clock set forward counts too.
func sleptFor(last, now time.Time) time.Duration {
	return now.Round(0).Sub(last.Round(0)) - now.Sub(last)
}

// slept tells the log and the trace with every node that the machine
// slept for d.
func (n *Node) slept(d time.Duration) {
	text := fmt.Sprintf("this machine slept for %s", d.Round(100*time.Millisecond))
	n.log.Print(text)
	n.mu.Lock()
	ids := slices.Collect(maps.Keys(n.directTrace))
	n.mu.Unlock()
	for _, id := range ids {
		n.trace(id, "%s", text)
	}
}
