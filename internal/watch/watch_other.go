//go:build !windows

package watch

// New is not implemented yet outside Windows (macOS will use FSEvents).
func New(root string) (*Watcher, error) { return nil, ErrUnsupported }
