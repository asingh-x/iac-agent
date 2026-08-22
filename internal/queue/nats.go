package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

const (
	natsStream  = "TF_AGENT"
	natsAckWait = 5 * time.Minute
)

func natsSubject(name string) string { return "tf.tasks." + name }
func natsDurable(name string) string { return "tf-workers-" + name }

// NATSQueue is a durable NATS JetStream-backed Queue.
// Tasks survive server restarts and can be consumed by multiple workers.
type NATSQueue struct {
	nc   *nats.Conn
	js   nats.JetStreamContext
	sub  *nats.Subscription
	name string // queue name, used as subject suffix
}

// DefaultNATSMaxMsgs is used when NewNATSQueue is called with maxMsgs <= 0.
const DefaultNATSMaxMsgs = 5000

// NewNATSQueue connects to NATS, ensures the shared stream exists, creates a
// per-name durable pull consumer, and returns a queue ready to push and pop.
// name is used as the subject suffix (e.g. "default" → tf.tasks.default).
//
// maxMsgs bounds the shared stream's total backlog for backpressure: once the
// stream holds maxMsgs messages, Push returns an error instead of blocking or
// silently dropping tasks. Pass <= 0 to use DefaultNATSMaxMsgs.
//
// IMPORTANT: MaxMsgs is a property of the STREAM, not of any one consumer or
// subject. Since ALL named queues share one stream (TF_AGENT, subjects
// tf.tasks.>, see natStream/natsSubject), this limit caps the TOTAL backlog
// across every named queue combined — it is NOT an independent per-queue-name
// limit. A burst on one named queue can therefore consume the whole budget
// and cause Push to fail for other named queues too. That's an accepted
// limitation of the current shared-stream design, not a bug.
func NewNATSQueue(url, name string, maxMsgs int) (*NATSQueue, error) {
	if name == "" {
		name = "default"
	}
	if maxMsgs <= 0 {
		maxMsgs = DefaultNATSMaxMsgs
	}
	nc, err := nats.Connect(url,
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}

	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("nats jetstream: %w", err)
	}

	subject := natsSubject(name)
	durable := natsDurable(name)

	// Create (or no-op) the shared stream covering all named queues (tf.tasks.>).
	// MaxMsgs + Discard: DiscardNew caps the stream's total backlog and
	// causes Publish (and therefore Push) to fail with an error once the
	// limit is reached, giving real backpressure. The alternative,
	// DiscardOld (the zero value), would silently evict the oldest queued
	// tasks to make room for new ones instead of erroring — that would lose
	// tasks rather than reject them, which is the opposite of what
	// backpressure is for here.
	streamCfg := &nats.StreamConfig{
		Name:      natsStream,
		Subjects:  []string{"tf.tasks.>"},
		Storage:   nats.FileStorage,
		Retention: nats.WorkQueuePolicy, // each message delivered to exactly one consumer
		Replicas:  1,
		MaxMsgs:   int64(maxMsgs),
		Discard:   nats.DiscardNew,
	}
	_, err = js.AddStream(streamCfg)
	if err == nats.ErrStreamNameAlreadyInUse {
		// AddStream returns this specifically when a stream with this name
		// already exists but its config differs from streamCfg (e.g. MaxMsgs
		// changed since the stream was first created, or this is the first
		// startup after this backpressure feature was added) — reconcile it
		// via UpdateStream. NOTE: the original form of this check was
		// inverted (`err != nats.ErrStreamNameAlreadyInUse`), which skipped
		// UpdateStream in exactly the case it's needed and meant config
		// changes such as MaxMsgs would silently never take effect against
		// an already-existing stream. Fixed here.
		_, err = js.UpdateStream(streamCfg)
	}
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("nats stream: %w", err)
	}

	// Per-name pull consumer — durable so it survives reconnects.
	sub, err := js.PullSubscribe(subject, durable,
		nats.BindStream(natsStream),
		nats.AckWait(natsAckWait),
		nats.MaxDeliver(5),
	)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("nats pull subscribe (%s): %w", name, err)
	}

	return &NATSQueue{nc: nc, js: js, sub: sub, name: name}, nil
}

func (q *NATSQueue) Push(ctx context.Context, item Item) error {
	data, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("marshal item: %w", err)
	}
	_, err = q.js.Publish(natsSubject(q.name), data)
	return err
}

// Name returns the queue name.
func (q *NATSQueue) Name() string { return q.name }

// natsDelivery acknowledges a single NATS JetStream message. The message is
// only removed from the stream when Ack is called — the caller is
// responsible for calling it only after the item's work is durably persisted.
type natsDelivery struct{ msg *nats.Msg }

func (d natsDelivery) Ack() error    { return d.msg.Ack() }
func (d natsDelivery) Nak() error    { return d.msg.Nak() }
func (d natsDelivery) Extend() error { return d.msg.InProgress() }

// Pop blocks until a task is available or ctx is cancelled. It does NOT ack
// the message — the returned Delivery must be Ack'd by the caller once the
// item's result is durably persisted, or Nak'd to request redelivery.
func (q *NATSQueue) Pop(ctx context.Context) (Item, Delivery, error) {
	for {
		select {
		case <-ctx.Done():
			return Item{}, nil, ctx.Err()
		default:
		}

		msgs, err := q.sub.Fetch(1, nats.MaxWait(500*time.Millisecond))
		if err == nats.ErrTimeout {
			continue
		}
		if err != nil {
			// On connection errors, back off and retry.
			select {
			case <-ctx.Done():
				return Item{}, nil, ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		if len(msgs) == 0 {
			continue
		}

		msg := msgs[0]
		var item Item
		if err := json.Unmarshal(msg.Data, &item); err != nil {
			// Malformed message: not our data, ack it so it doesn't jam the
			// stream, and move on to the next one.
			_ = msg.Ack()
			continue
		}
		return item, natsDelivery{msg: msg}, nil
	}
}

// Len returns the approximate number of pending messages in the stream.
func (q *NATSQueue) Len() int {
	info, err := q.js.StreamInfo(natsStream)
	if err != nil {
		return 0
	}
	return int(info.State.Msgs)
}

// Close drains and closes the NATS connection.
func (q *NATSQueue) Close() error {
	_ = q.sub.Unsubscribe()
	q.nc.Close()
	return nil
}
