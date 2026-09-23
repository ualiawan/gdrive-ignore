// Package watch reports changes anywhere below a directory.
package watch

import (
	"errors"
	"sync/atomic"
)

// ErrUnsupported means only periodic rescans are available.
var ErrUnsupported = errors.New("file watching is not implemented on this platform yet")

// Watcher delivers batches of changed paths, relative to the root and
// "/"-separated. When events are lost (kernel buffer or channel overflow),
// TakeOverflow returns true and the consumer should rescan everything.
type Watcher struct {
	C   <-chan []string
	Err <-chan error // receives at most one fatal error, then the watcher stops

	c        chan []string
	err      chan error
	overflow atomic.Bool
	stop     func()
}

// TakeOverflow reports and clears the overflow flag.
func (w *Watcher) TakeOverflow() bool { return w.overflow.Swap(false) }

// Close stops watching. It is safe to call more than once.
func (w *Watcher) Close() { w.stop() }

func newWatcher() *Watcher {
	w := &Watcher{c: make(chan []string, 256), err: make(chan error, 1)}
	w.C, w.Err = w.c, w.err
	return w
}

func (w *Watcher) send(batch []string) {
	select {
	case w.c <- batch:
	default:
		w.overflow.Store(true)
		// Wake the consumer even if the batch is lost.
		select {
		case w.c <- nil:
		default:
		}
	}
}
