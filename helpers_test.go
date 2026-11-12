package crosswork

import (
	"context"
	"time"
)

func contextWithCancelAfter(d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(d); cancel() }()
	return ctx, cancel
}
