package client

import (
	"context"
	"errors"

	"github.com/Verboo/Verboo-SDK-go/pkg/frame"
)

func (c *Client) sendPrioritizedBlocking(ctx context.Context, f *frame.Frame, prio int) error {
	if f == nil {
		return errors.New("nil frame")
	}
	var ch chan *prioritizedFrame
	switch prio {
	case 0:
		ch = c.highCh
	case 1:
		ch = c.normalCh
	default:
		ch = c.lowCh
	}
	select {
	case ch <- &prioritizedFrame{f: f, prio: prio}:
		return nil
	case <-ctx.Done():
		frame.PutFrame(f)
		return ctx.Err()
	}
}
