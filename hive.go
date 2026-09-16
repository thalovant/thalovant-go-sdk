package thalovant

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BinaryPayloadKinds name the payload types a BINARY frame can carry, by their
// wire number.
//
// A hub answers speak:synth by rendering the utterance and sending one of these
// back, so a client with no synthesiser of its own can still speak; a file
// arrives the same way. The wire numbers the type, this names it.
var BinaryPayloadKinds = map[int]string{
	1: "raw_audio",
	2: "numpy_image",
	3: "file",
	4: "stt_transcribe",
	5: "stt_handle",
	6: "tts_audio",
}

// BinaryKindName names a payload type. One nobody has named still arrives,
// under its number, rather than being dropped.
func BinaryKindName(wireNumber int) string {
	if name, known := BinaryPayloadKinds[wireNumber]; known {
		return name
	}
	return "binary:" + strconv.Itoa(wireNumber)
}

// ThalovantBinary is a binary frame: the bytes a hub sent, and what it said
// about them.
type ThalovantBinary struct {
	// Kind is tts_audio, file, ... or binary:<wire number> for an unnamed type.
	Kind string
	// Data is the payload itself. Never parsed, never decompressed.
	Data []byte
	// Metadata is what the hub sent beside it.
	Metadata map[string]any
	// Utterance is what was said, when this is rendered speech.
	Utterance string
	// Lang is the language it was said in.
	Lang string
	// FileName is the name a file arrived under. An empty name is no name.
	FileName string
}

// BinaryFrame reads a hub's metadata into the shape above. A value the hub did
// not send and one it sent empty both read as empty: rendering "" as a filename
// would put a blank name in front of somebody as though the hub had chosen it.
func BinaryFrame(kind string, data []byte, metadata map[string]any) *ThalovantBinary {
	text := func(key string) string {
		if value, ok := metadata[key].(string); ok {
			return value
		}
		return ""
	}
	return &ThalovantBinary{
		Kind:      kind,
		Data:      data,
		Metadata:  metadata,
		Utterance: text("utterance"),
		Lang:      text("lang"),
		FileName:  text("file_name"),
	}
}

// ListenHive connects and returns an independent stream of one hive frame kind.
//
// A hub relays more than this client's conversation: broadcast is aimed down at
// every child, propagate walks the whole hive, escalate goes up to the parent,
// intercom is addressed node to node, and rendezvous is the mailbox peers use
// to find each other through NAT. See HiveKinds.
//
// Range over C and inspect Err afterward; call Close when done.
func (c *Client) ListenHive(ctx context.Context, kind string, options ListenOptions) (*Subscription[HiveMessage], error) {
	kind = strings.TrimSpace(kind)
	if !slices.Contains(HiveKinds, kind) {
		// Named rather than silently never firing: subscribing to "bus" or to a
		// typo is the kind of mistake that looks like a quiet hub.
		return nil, fmt.Errorf("%q is not a hive frame kind; expected one of %s", kind, strings.Join(HiveKinds, ", "))
	}
	return listenHiveFrames(ctx, c, options, func(message HiveMessage) bool {
		return message.MsgType == kind
	})
}

// ListenBinary connects and returns an independent stream of binary frames:
// rendered speech, and files.
//
// This is what a hub sends back for speak:synth -- the audio itself, so a
// client with no synthesiser can still speak -- and how it hands over a file.
// Delivered as a stream and not on a reply, because a binary frame carries no
// request id: it cannot be attributed to one Ask. Its Utterance is the only
// thread back to a turn.
func (c *Client) ListenBinary(ctx context.Context, options ListenOptions) (*Subscription[ThalovantBinary], error) {
	frames, err := listenHiveFrames(ctx, c, options, func(message HiveMessage) bool {
		return message.MsgType == "bin" && message.Binary != nil
	})
	if err != nil {
		return nil, err
	}
	output := make(chan ThalovantBinary, cap(frames.C))
	stream := &Subscription[ThalovantBinary]{C: output, stop: frames.Close}
	go func() {
		defer close(output)
		for message := range frames.C {
			select {
			case output <- *message.Binary:
			default:
				stream.mu.Lock()
				stream.err = ErrEventOverflow
				stream.mu.Unlock()
				frames.Close()
				return
			}
		}
		stream.mu.Lock()
		stream.err = frames.Err()
		stream.mu.Unlock()
	}()
	return stream, nil
}

// listenHiveFrames is Listen's lifecycle over the hive stream: one bounded,
// independent subscription, closed by timeout, cancellation, disconnect or
// overflow, with the reason readable from Err.
func listenHiveFrames(ctx context.Context, c *Client, options ListenOptions, keep func(HiveMessage) bool) (*Subscription[HiveMessage], error) {
	if options.Timeout < 0 || options.MaxEvents < 0 || options.Capacity < 0 {
		return nil, fmt.Errorf("listen timeout, max events and capacity must not be negative")
	}
	var cancel context.CancelFunc
	if options.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, options.Timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	capacity := options.Capacity
	if capacity == 0 {
		capacity = 256
	}
	if capacity > 65536 {
		capacity = 65536
	}
	transport, ok := c.Transport.(hiveMessageTransport)
	if !ok {
		cancel()
		return nil, fmt.Errorf("%w: this transport does not carry hive frames", ErrRuntime)
	}
	source := subscribeHiveMessages(transport)
	// Retain frames relayed immediately before Connect returns.
	if err := c.Connect(ctx); err != nil {
		source.Close()
		cancel()
		return nil, err
	}
	output := make(chan HiveMessage, capacity)
	stream := &Subscription[HiveMessage]{C: output}
	done := make(chan struct{})
	var completed sync.Once
	finish := func(err error) {
		completed.Do(func() {
			stream.mu.Lock()
			stream.err = err
			close(output)
			close(done)
			stream.mu.Unlock()
			source.Close()
			cancel()
		})
	}
	stream.stop = func() { finish(nil) }
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		delivered := 0
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				finish(fmt.Errorf("%w: %w", ErrTimeout, ctx.Err()))
				return
			case <-ticker.C:
				health := c.Transport.Healthcheck()
				if !health.Connected || !health.HandshakeComplete {
					finish(ErrConnection)
					return
				}
			case message, open := <-source.C:
				if !open {
					finish(subscriptionError(source.Err()))
					return
				}
				if !keep(message) {
					continue
				}
				select {
				case output <- message:
				default:
					finish(ErrEventOverflow)
					return
				}
				delivered++
				if options.MaxEvents > 0 && delivered >= options.MaxEvents {
					finish(nil)
					return
				}
			}
		}
	}()
	return stream, nil
}

// Propagate sends an event across the hive; every node sees it once.
func (c *Client) Propagate(ctx context.Context, eventType string, data Data, eventContext Context) error {
	return c.sendHive(ctx, "propagate", eventType, data, eventContext)
}

// Escalate sends an event up to the parent node.
func (c *Client) Escalate(ctx context.Context, eventType string, data Data, eventContext Context) error {
	return c.sendHive(ctx, "escalate", eventType, data, eventContext)
}

// Broadcast sends an event down to every child of this hub. Admin only.
//
// A hub requires admin standing and the can_broadcast grant, and a client that
// sends one without them is not answered with an error -- it is disconnected
// for misbehaviour. Nothing here can check first: a hub's HELLO carries its
// public key, peer name and node id, and nothing about what this client may do,
// so a refusal arrives as a closed connection on the next read.
func (c *Client) Broadcast(ctx context.Context, eventType string, data Data, eventContext Context) error {
	return c.sendHive(ctx, "broadcast", eventType, data, eventContext)
}

func (c *Client) sendHive(ctx context.Context, kind, eventType string, data Data, eventContext Context) error {
	transport, ok := c.Transport.(hiveMessageTransport)
	if !ok {
		return fmt.Errorf("%w: this transport does not carry hive frames", ErrRuntime)
	}
	if err := c.Connect(ctx); err != nil {
		return err
	}
	if data == nil {
		data = Data{}
	}
	if eventContext == nil {
		eventContext = Context{}
	}
	// Nested on purpose: a hub reads message.payload as a HiveMessage of its own
	// and rewrites the route on it, so a flat frame loses the route.
	return c.runOwned(ctx, false, func() error {
		return transport.SendHiveMessage(ctx, HiveMessage{
			MsgType: kind,
			Payload: map[string]any{
				"msg_type": "bus",
				"payload": map[string]any{
					"type":    eventType,
					"data":    map[string]any(data),
					"context": map[string]any(c.contextWithIdentityMetadata(eventContext)),
				},
			},
			Metadata: map[string]any{},
			Route:    []any{},
		}, true)
	})
}
