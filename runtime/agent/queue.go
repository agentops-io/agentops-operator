package main

import (
	"context"
	"os"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Task is a unit of work pulled from the task queue.
type Task struct {
	ID     string
	Prompt string
	Meta   map[string]string
}

// TaskQueue wraps a Redis Stream consumer group.
type TaskQueue struct {
	rdb        *redis.Client
	stream     string
	group      string
	consumer   string
	maxRetries int
}

// NewTaskQueue connects to Redis and ensures the consumer group exists.
func NewTaskQueue(addr string, maxRetries int) *TaskQueue {
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	q := &TaskQueue{
		rdb:        rdb,
		stream:     "agent-tasks",
		group:      "agent-workers",
		consumer:   podName(),
		maxRetries: maxRetries,
	}
	// Create consumer group if it doesn't exist yet; "$" means only new messages.
	_ = rdb.XGroupCreateMkStream(context.Background(), q.stream, q.group, "$").Err()
	return q
}

// Poll blocks up to 2 s waiting for a task. Returns (nil, nil) when the
// stream is empty so callers can check ctx.Done() between polls.
func (q *TaskQueue) Poll(ctx context.Context) (*Task, error) {
	results, err := q.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    q.group,
		Consumer: q.consumer,
		Streams:  []string{q.stream, ">"},
		Count:    1,
		Block:    2 * time.Second,
	}).Result()

	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(results) == 0 || len(results[0].Messages) == 0 {
		return nil, nil
	}

	msg := results[0].Messages[0]
	prompt, _ := msg.Values["prompt"].(string)

	meta := make(map[string]string)
	for k, v := range msg.Values {
		if k == "prompt" {
			continue
		}
		if s, ok := v.(string); ok {
			meta[k] = s
		}
	}

	return &Task{ID: msg.ID, Prompt: prompt, Meta: meta}, nil
}

// Ack acknowledges successful task completion and stores the result.
func (q *TaskQueue) Ack(taskID, result string) {
	ctx := context.Background()
	_ = q.rdb.XAck(ctx, q.stream, q.group, taskID).Err()
	_ = q.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: q.stream + "-results",
		Values: map[string]any{"task_id": taskID, "result": result},
	}).Err()
}

// Nack marks a task as failed. If the task has not exceeded maxRetries it is
// requeued on the main stream with an incremented attempt counter. On the final
// attempt it is written to the dead-letter stream instead.
func (q *TaskQueue) Nack(task Task, reason string) {
	ctx := context.Background()

	attempt := 0
	if a, err := strconv.Atoi(task.Meta["attempt"]); err == nil {
		attempt = a
	}

	// Always acknowledge so the message leaves the PEL.
	_ = q.rdb.XAck(ctx, q.stream, q.group, task.ID).Err()

	if attempt < q.maxRetries {
		// Rebuild the message values, preserving all original meta.
		values := map[string]any{
			"prompt":  task.Prompt,
			"attempt": strconv.Itoa(attempt + 1),
		}
		for k, v := range task.Meta {
			if k != "attempt" {
				values[k] = v
			}
		}
		_ = q.rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: q.stream,
			Values: values,
		}).Err()
		return
	}

	// Final failure — dead-letter.
	_ = q.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: q.stream + "-dead",
		Values: map[string]any{
			"task_id": task.ID,
			"prompt":  task.Prompt,
			"error":   reason,
			"attempt": strconv.Itoa(attempt),
		},
	}).Err()
}

// Close releases the Redis connection.
func (q *TaskQueue) Close() {
	_ = q.rdb.Close()
}

func podName() string {
	if name := os.Getenv("POD_NAME"); name != "" {
		return name
	}
	return "agent-local"
}
