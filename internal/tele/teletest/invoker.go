// Package teletest fakes the Telegram API for tests: a tg.Client built on
// Invoker records the requests it sends and gets a scripted answer.
package teletest

import (
	"context"

	"github.com/gotd/td/bin"
)

// Invoker records every request and answers with Reply, or fails with Err.
type Invoker struct {
	Requests []bin.Encoder
	Reply    bin.Encoder // decoded into the caller's result
	Err      error
}

// Invoke implements tg.Invoker.
func (i *Invoker) Invoke(_ context.Context, in bin.Encoder, out bin.Decoder) error {
	i.Requests = append(i.Requests, in)
	if i.Err != nil {
		return i.Err
	}
	var b bin.Buffer
	if err := i.Reply.Encode(&b); err != nil {
		return err
	}
	return out.Decode(&b)
}
