package thalovant

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestNoiseInteropAgainstLiveHub connects to a real HiveMind-core 5.x listener
// and completes a v3 handshake against it.
//
// The unit tests pin the PSK, canonical JSON and prologue against vectors taken
// from the reference implementation, which catches every byte-level divergence
// we know to look for. This catches the ones we do not: the wire type names,
// the envelope shapes, the order of the exchange, and whether the hub actually
// accepts what we encrypt.
//
// It is skipped unless a hub is named, because it needs a running listener:
//
//	hivemind-core add-client --name go --access-key <key> --password <password>
//	hivemind-core allow-msg recognizer_loop:utterance <id>
//	hivemind-core listen
//
//	THALOVANT_INTEROP_WSS=ws://127.0.0.1:5678 \
//	THALOVANT_INTEROP_ACCESS_KEY=<key> \
//	THALOVANT_INTEROP_PASSWORD=<password> \
//	go test -run TestNoiseInteropAgainstLiveHub -v
//
// A hub pins the client static key on first contact, so a run whose state
// directory has been discarded needs `hivemind-core reset-noise-pin <key>`.
func TestNoiseInteropAgainstLiveHub(t *testing.T) {
	endpoint := os.Getenv("THALOVANT_INTEROP_WSS")
	accessKey := os.Getenv("THALOVANT_INTEROP_ACCESS_KEY")
	password := os.Getenv("THALOVANT_INTEROP_PASSWORD")
	if endpoint == "" || accessKey == "" || password == "" {
		t.Skip("set THALOVANT_INTEROP_WSS, THALOVANT_INTEROP_ACCESS_KEY and THALOVANT_INTEROP_PASSWORD to run the live interop test")
	}

	stateDir := os.Getenv("THALOVANT_INTEROP_STATE_DIR")
	if stateDir == "" {
		stateDir = t.TempDir()
	}

	transport := NewWSSTransport(Identity{
		AccessKey:          accessKey,
		Password:           password,
		SiteID:             "go-interop",
		DataPlaneEndpoints: HubDataPlaneEndpoints{WSS: endpoint},
	})
	transport.NoiseStateDir = stateDir

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	if err := transport.Connect(ctx); err != nil {
		t.Fatalf("v3 handshake against %s failed: %v", endpoint, err)
	}
	t.Cleanup(func() { _ = transport.Disconnect(context.Background()) })

	if key := transport.RemoteStaticKey(); len(key) != 64 {
		t.Fatalf("the hub's static key came back as %q; there is nothing to pin", key)
	}

	// A message the hub has to decrypt and route proves the transport, not just
	// the handshake.
	if err := transport.EmitBus(ctx, "recognizer_loop:utterance",
		Data{"utterances": []string{"hello from go"}, "lang": "en-US"},
		Context{}); err != nil {
		t.Fatalf("the hub rejected an encrypted bus message: %v", err)
	}

	// Give the hub a moment to close the socket if it disliked the frame.
	time.Sleep(1500 * time.Millisecond)
	health := transport.Healthcheck()
	if !health.Connected || !health.HandshakeComplete {
		t.Fatalf("the session dropped after the first encrypted message: %+v", health)
	}
}
