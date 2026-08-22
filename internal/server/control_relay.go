package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go"
)

// errNotOwned is returned by the local-only sendAnswerLocal/
// sendPermissionResponseLocal/cancelTaskLocal methods when this pod has no
// record of taskID — i.e. it isn't the pod running that task. It is
// distinguished from other local failures (e.g. "channel full") so
// NATSControlRelay's inbound handler knows to stay silent rather than reply
// with a false negative.
var errNotOwned = errors.New("task not owned by this pod")

// ControlLocalHandler is the subset of Runner's behavior NATSControlRelay
// needs: attempt to satisfy a control request using only local state. Runner
// implements this via sendAnswerLocal/sendPermissionResponseLocal/
// cancelTaskLocal — deliberately NOT the public SendAnswer/
// SendPermissionResponse/CancelTask, so the inbound NATS handler can never
// recurse back out over the relay.
type ControlLocalHandler interface {
	sendAnswerLocal(taskID, answer string) error
	sendPermissionResponseLocal(taskID string, allow bool) error
	cancelTaskLocal(taskID string) error
}

// ControlRelay lets a pod ask whichever pod actually owns a task to deliver
// an answer, permission decision, or cancellation.
type ControlRelay interface {
	RequestAnswer(ctx context.Context, taskID, answer string) error
	RequestPermission(ctx context.Context, taskID string, allow bool) error
	RequestCancel(ctx context.Context, taskID string) error
}

// noopControlRelay is the default: no cross-pod transport configured.
type noopControlRelay struct{}

func (noopControlRelay) RequestAnswer(context.Context, string, string) error {
	return fmt.Errorf("task not found on any pod")
}
func (noopControlRelay) RequestPermission(context.Context, string, bool) error {
	return fmt.Errorf("task not found on any pod")
}
func (noopControlRelay) RequestCancel(context.Context, string) error {
	return fmt.Errorf("task not found on any pod")
}

func controlSubject(taskID string) string { return "tf.control." + taskID }

type controlMessage struct {
	Kind   string `json:"kind"` // answer | permission | cancel
	Answer string `json:"answer,omitempty"`
	// Allow is deliberately NOT omitempty: a deny (false) is a
	// security-relevant decision that happens to coincide with Go's zero
	// value, and must be transmitted explicitly rather than inferred from an
	// absent field.
	Allow bool `json:"allow"`
}

type controlReply struct {
	OK bool `json:"ok"`
}

// NATSControlRelay dispatches answer/permission/cancel requests to whichever
// pod owns a task, over NATS core request-reply, and (via the subscription
// created in NewNATSControlRelay) answers those requests on behalf of this
// pod's own local handler when it turns out to be the owner.
type NATSControlRelay struct {
	nc    *nats.Conn
	local ControlLocalHandler
	sub   *nats.Subscription
}

// NewNATSControlRelay subscribes this pod to cross-pod control requests
// (routing anything this pod owns into local's local-only handling) and
// returns a relay usable to forward requests this pod can't satisfy locally.
func NewNATSControlRelay(nc *nats.Conn, local ControlLocalHandler) (*NATSControlRelay, error) {
	rel := &NATSControlRelay{nc: nc, local: local}
	sub, err := nc.Subscribe("tf.control.*", rel.handle)
	if err != nil {
		return nil, fmt.Errorf("control relay subscribe: %w", err)
	}
	rel.sub = sub
	return rel, nil
}

// handle runs on every pod for every control request. Only the pod that
// actually owns taskID replies — see errNotOwned's doc comment for why a
// non-owning pod must stay silent instead of replying with a negative ack.
func (r *NATSControlRelay) handle(msg *nats.Msg) {
	if msg.Reply == "" {
		return
	}
	taskID := strings.TrimPrefix(msg.Subject, "tf.control.")
	var cmd controlMessage
	if json.Unmarshal(msg.Data, &cmd) != nil {
		return
	}

	var err error
	switch cmd.Kind {
	case "answer":
		err = r.local.sendAnswerLocal(taskID, cmd.Answer)
	case "permission":
		err = r.local.sendPermissionResponseLocal(taskID, cmd.Allow)
	case "cancel":
		err = r.local.cancelTaskLocal(taskID)
	default:
		return
	}
	if err != nil {
		// Not owned by this pod (or some other local failure, e.g. a full
		// answer channel) — stay silent either way; a genuine "channel full"
		// on the true owner is rare enough that surfacing it as a relay
		// timeout (rather than plumbing a second error class through) is an
		// acceptable simplification.
		return
	}
	reply, _ := json.Marshal(controlReply{OK: true})
	_ = r.nc.Publish(msg.Reply, reply)
}

func (r *NATSControlRelay) request(ctx context.Context, taskID string, cmd controlMessage) error {
	data, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	msg, err := r.nc.RequestWithContext(ctx, controlSubject(taskID), data)
	if err != nil {
		return fmt.Errorf("task %s not found on any pod", taskID)
	}
	var reply controlReply
	if json.Unmarshal(msg.Data, &reply) != nil || !reply.OK {
		return fmt.Errorf("task %s: relay request failed", taskID)
	}
	return nil
}

func (r *NATSControlRelay) RequestAnswer(ctx context.Context, taskID, answer string) error {
	return r.request(ctx, taskID, controlMessage{Kind: "answer", Answer: answer})
}

func (r *NATSControlRelay) RequestPermission(ctx context.Context, taskID string, allow bool) error {
	return r.request(ctx, taskID, controlMessage{Kind: "permission", Allow: allow})
}

func (r *NATSControlRelay) RequestCancel(ctx context.Context, taskID string) error {
	return r.request(ctx, taskID, controlMessage{Kind: "cancel"})
}
