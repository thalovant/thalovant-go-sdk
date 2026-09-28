package thalovant

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

type blockedClientTransport struct {
	*dispatchTransport
	ready          atomic.Bool
	connects       atomic.Int32
	disconnects    atomic.Int32
	connectStarted chan struct{}
	connectRelease chan struct{}
	sendStarted    chan struct{}
	sendRelease    chan struct{}
	// sendErr is what a released send returns: a write that failed.
	sendErr      error
	closeStarted chan struct{}
	closeRelease chan struct{}
}

func newBlockedClientTransport() *blockedClientTransport {
	return &blockedClientTransport{dispatchTransport: &dispatchTransport{WSSTransport: NewWSSTransport(Identity{}), emitted: make(chan emittedQuery, 8)}}
}
func (t *blockedClientTransport) Healthcheck() TransportHealth {
	return TransportHealth{Connected: t.ready.Load(), HandshakeComplete: t.ready.Load()}
}
func (t *blockedClientTransport) Connect(context.Context) error {
	t.connects.Add(1)
	if t.connectStarted != nil {
		t.connectStarted <- struct{}{}
		<-t.connectRelease
	}
	t.ready.Store(true)
	return nil
}
func (t *blockedClientTransport) Disconnect(context.Context) error {
	t.disconnects.Add(1)
	if t.closeStarted != nil {
		t.closeStarted <- struct{}{}
		<-t.closeRelease
	}
	t.ready.Store(false)
	return nil
}
func (t *blockedClientTransport) EmitBus(ctx context.Context, name string, data Data, c Context) error {
	if t.sendStarted != nil {
		t.sendStarted <- struct{}{}
		<-t.sendRelease
		if t.sendErr != nil {
			return t.sendErr
		}
	}
	return t.dispatchTransport.EmitBus(ctx, name, data, c)
}
func awaitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("operation did not reach expected barrier")
	}
}
func awaitCleanup(t *testing.T, transport *blockedClientTransport) {
	t.Helper()
	until := time.Now().Add(time.Second)
	for transport.disconnects.Load() == 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if transport.disconnects.Load() == 0 {
		t.Fatal("abandoned operation was not cleaned up")
	}
}

func TestClientRetainsLateConnectionUntilCleanup(t *testing.T) {
	transport := newBlockedClientTransport()
	transport.connectStarted = make(chan struct{}, 2)
	transport.connectRelease = make(chan struct{})
	transport.closeStarted = make(chan struct{}, 2)
	transport.closeRelease = make(chan struct{})
	defer close(transport.closeRelease)
	client := &Client{Transport: transport, ConnectTimeout: 25 * time.Millisecond}
	first := make(chan error, 1)
	go func() { first <- client.Connect(context.Background()) }()
	awaitSignal(t, transport.connectStarted)
	if err := <-first; !errors.Is(err, ErrTimeout) {
		t.Fatal(err)
	}
	close(transport.connectRelease)
	awaitSignal(t, transport.closeStarted)
	if err := client.Connect(context.Background()); !errors.Is(err, ErrTimeout) {
		t.Fatal("cleanup barrier was bypassed", err)
	}
	if transport.connects.Load() != 1 {
		t.Fatal("another connection started while old cleanup owned transport")
	}
}
func TestClientJoiningDeadlineDoesNotCancelOwner(t *testing.T) {
	transport := newBlockedClientTransport()
	transport.connectStarted = make(chan struct{}, 2)
	transport.connectRelease = make(chan struct{})
	client := &Client{Transport: transport, ConnectTimeout: time.Second}
	owner := make(chan error, 1)
	go func() { owner <- client.Connect(context.Background()) }()
	awaitSignal(t, transport.connectStarted)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := client.Connect(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if transport.disconnects.Load() != 0 {
		t.Fatal("joining timeout canceled connection owner")
	}
	close(transport.connectRelease)
	if err := <-owner; err != nil {
		t.Fatal(err)
	}
	if err := client.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if transport.connects.Load() != 1 {
		t.Fatal("ready connection was not coalesced")
	}
}
func TestClientSendDeadlineRetainsOwnershipUntilActualCompletion(t *testing.T) {
	for _, failed := range []bool{true, false} {
		transport := newBlockedClientTransport()
		transport.ready.Store(true)
		transport.sendStarted = make(chan struct{}, 2)
		transport.sendRelease = make(chan struct{})
		if failed {
			transport.sendErr = fmt.Errorf("%w: the write failed", ErrConnection)
		}
		client := &Client{Transport: transport, ConnectTimeout: 25 * time.Millisecond}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		done := make(chan error, 1)
		go func() { done <- client.Emit(ctx, "test", Data{}, Context{}) }()
		awaitSignal(t, transport.sendStarted)
		if err := <-done; !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		if err := client.Connect(context.Background()); !errors.Is(err, ErrTimeout) {
			t.Fatal("in-flight send ownership lost", err)
		}
		close(transport.sendRelease)
		if failed {
			// A send that failed after its caller left is cleaned up.
			awaitCleanup(t, transport)
		} else {
			// One that finished is a frame written whole: the link stays.
			awaitNoCleanup(t, client, transport)
		}
		cancel()
	}
}

// awaitNoCleanup waits until the client is free again and checks that the
// transport was not torn down on the way.
func awaitNoCleanup(t *testing.T, client *Client, transport *blockedClientTransport) {
	t.Helper()
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := client.connectionGate.Lock(ctx)
		cancel()
		if err == nil {
			client.connectionGate.Unlock()
			if transport.disconnects.Load() != 0 {
				t.Fatal("a send that finished tore the link down")
			}
			return
		}
	}
	t.Fatal("the finished send never gave the client back")
}
func TestClientCloseBoundsCustomTransportAndRetainsOwnership(t *testing.T) {
	transport := newBlockedClientTransport()
	transport.ready.Store(true)
	transport.closeStarted = make(chan struct{}, 2)
	transport.closeRelease = make(chan struct{})
	defer close(transport.closeRelease)
	client := &Client{Transport: transport, ConnectTimeout: 25 * time.Millisecond}
	done := make(chan error, 1)
	go func() { done <- client.Close(context.Background()) }()
	awaitSignal(t, transport.closeStarted)
	if err := <-done; !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := client.Connect(context.Background()); !errors.Is(err, ErrTimeout) {
		t.Fatal("pending close ownership lost", err)
	}
	if transport.connects.Load() != 0 {
		t.Fatal("connect raced pending close")
	}
}

type blockedHealthTransport struct {
	*blockedClientTransport
	healthStarted chan struct{}
	healthRelease chan struct{}
}

func (t *blockedHealthTransport) Healthcheck() TransportHealth {
	t.healthStarted <- struct{}{}
	<-t.healthRelease
	return TransportHealth{}
}
func TestClientDeadlineIncludesCustomHealthProbe(t *testing.T) {
	transport := &blockedHealthTransport{blockedClientTransport: newBlockedClientTransport(), healthStarted: make(chan struct{}, 1), healthRelease: make(chan struct{})}
	client := &Client{Transport: transport, ConnectTimeout: 25 * time.Millisecond}
	done := make(chan error, 1)
	go func() { done <- client.Connect(context.Background()) }()
	awaitSignal(t, transport.healthStarted)
	select {
	case err := <-done:
		if !errors.Is(err, ErrTimeout) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("health probe defeated connect deadline")
	}
	if transport.connects.Load() != 0 {
		t.Fatal("connect started after probe deadline")
	}
	close(transport.healthRelease)
	awaitCleanup(t, transport.blockedClientTransport)
}

// closedAtReadyTransport finishes its handshake and is closed before Connect
// looks at it, with the verdict it recorded on that close.
type closedAtReadyTransport struct {
	*blockedClientTransport
	refused     bool
	keyRejected bool
}

func (t *closedAtReadyTransport) Connect(context.Context) error {
	t.connects.Add(1)
	return nil
}
func (t *closedAtReadyTransport) ClosedRefused() bool { return t.refused }
func (t *closedAtReadyTransport) keyRejection() error {
	if !t.keyRejected {
		return nil
	}
	return &ClientKeyRejectedError{KeyFolder: "here", OtherKeyFolder: "there"}
}

// A close between the transport's return and Connect's own look is read as
// the transport read it, as HubSession reads one a moment later: on a slow
// runner that window is where a rejected key used to become a plain failure.
func TestClientConnectKeepsTheVerdictOfACloseAtReadiness(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		refused, keyRejected  bool
		isRefused, isRejected bool
	}{
		{"a rejected key", true, true, true, true},
		{"a refusal", true, false, true, false},
		{"a drop", false, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &closedAtReadyTransport{blockedClientTransport: newBlockedClientTransport(), refused: tc.refused, keyRejected: tc.keyRejected}
			err := (&Client{Transport: transport}).Connect(context.Background())
			if !errors.Is(err, ErrConnection) || errors.Is(err, ErrHubRefused) != tc.isRefused || errors.Is(err, ErrClientKeyRejected) != tc.isRejected {
				t.Fatalf("connect = %v", err)
			}
			if tc.isRejected {
				var rejected *ClientKeyRejectedError
				if !errors.As(err, &rejected) || rejected.KeyFolder != "here" {
					t.Fatalf("the rejection lost its folders: %#v", err)
				}
			}
		})
	}
}
