package taskq_test

import (
	"context"
	"fmt"
	"time"

	"github.com/frain-dev/taskq/v3"
	"github.com/frain-dev/taskq/v3/memqueue"
)

type RateLimitError string

func (e RateLimitError) Error() string {
	return string(e)
}

func (RateLimitError) Delay() time.Duration {
	return 3 * time.Second
}

func Example_customRateLimit() {
	start := time.Now()
	q := memqueue.NewQueue(&taskq.QueueOptions{
		Name: "test",
	})
	task := taskq.RegisterTask(&taskq.TaskOptions{
		Name: "Example_customRateLimit",
		Handler: func() error {
			fmt.Println("retried in", timeSince(start))
			return RateLimitError("calm down")
		},
		RetryLimit: 2,
		MinBackoff: time.Millisecond,
	})

	ctx := context.Background()
	q.Add(task.WithArgs(ctx))

	// Wait for all messages to be processed.
	_ = q.Close()

	// Output: retried in 0s
	// retried in 3s
}
