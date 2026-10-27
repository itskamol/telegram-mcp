package feed

import "sync"

// Notifier wakes every waiter when the buffer changes, without polling:
// waiters hold the current channel, and Publish closes it (a close is seen
// by all receivers) and installs a fresh one.
//
// A waiter must take Ready before it checks the buffer. Taking it after
// could miss a Publish that lands between the check and the wait.
type Notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

// NewNotifier returns a notifier with nobody waiting.
func NewNotifier() *Notifier {
	return &Notifier{ch: make(chan struct{})}
}

// Ready returns a channel that is closed at the next Publish.
func (n *Notifier) Ready() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

// Publish wakes everyone waiting on the current Ready channel.
func (n *Notifier) Publish() {
	n.mu.Lock()
	defer n.mu.Unlock()
	close(n.ch)
	n.ch = make(chan struct{})
}
