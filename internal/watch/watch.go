// Package watch reports changes anywhere below a directory.
package watch

import (
	"errors"
	"sync/atomic"
	"time"
)

// ErrUnsupported means only periodic rescans are available.
var ErrUnsupported = errors.New("file watching is not implemented on this platform yet")

// Batch is a set of changed paths (relative to the root, "/"-separated),
// stamped with the time the change notification arrived.
type Batch struct {
	Paths []string
	Time  time.Time
}

// Watcher delivers batches of changed paths. When events are lost (kernel
// buffer or channel overflow), TakeOverflow returns true and the consumer
// should rescan everything.
type Watcher struct {
	C   <-chan Batch
	Err <-chan error // receives at most one fatal error, then the watcher stops

	c        chan Batch
	err      chan error
	overflow atomic.Bool
	stop     func()
}

// TakeOverflow reports and clears the overflow flag.
func (w *Watcher) TakeOverflow() bool { return w.overflow.Swap(false) }

// Close stops watching. It is safe to call more than once.
func (w *Watcher) Close() { w.stop() }

func newWatcher() *Watcher {
	w := &Watcher{c: make(chan Batch, 256), err: make(chan error, 1)}
	w.C, w.Err = w.c, w.err
	return w
}

func (w *Watcher) send(paths []string) {
	batch := Batch{Paths: paths, Time: time.Now()}
	select {
	case w.c <- batch:
	default:
		w.overflow.Store(true)
		// Wake the consumer even if the batch is lost.
		select {
		case w.c <- Batch{Time: batch.Time}:
		default:
		}
	}
}
