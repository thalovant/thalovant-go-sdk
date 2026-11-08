package thalovant

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The loopback listener a browser sign-in waits on (RFC 8252 section 7.3).
//
// BrowserLogin binds 127.0.0.1 on a port the system picks, sends the browser
// to the consent page with http://127.0.0.1:<port>/callback as its redirect,
// and waits here for the browser to come back. Only GET /callback is served;
// anything else is a 404. A callback whose state is not this sign-in's is a
// 400 and changes nothing: a stray tab -- an old attempt's, another
// program's, a page probing local ports -- must not end a real sign-in. The
// first callback with the right state is accepted, the port stops listening,
// and its tab is answered once the sign-in has ended, so that the page tells
// the truth.
//
// It reads HTTP/1.x request heads itself rather than through net/http's
// server, as the Python reference's _loopback.py does over asyncio streams:
// net/http answers a head it cannot read (or one over its limit) with its
// own text/plain 400 or 431, without the page headers every answer here
// carries; it refuses an HTTP/1.1 request without a Host header before any
// handler sees it; and it does not wait for its connections' goroutines on
// Close. Here every goroutine is counted, and close returns once all of them
// have. Nothing is logged, so the code and the state in a request line never
// reach a log.
//
// Everything that decides what a request means is classifyRequest, which
// touches no socket, so it is checked against the shared vectors on its own.

const (
	loopbackSignedInText     = "Signed in to Thalovant. You can close this tab."
	loopbackNotCompletedText = "Sign-in was not completed."
	loopbackWrongStateText   = "This answer does not belong to the sign-in in progress."
	loopbackNotFoundText     = "Not found."

	// loopbackMaxRequestHead is the largest request head read; a callback's
	// is a few hundred bytes.
	loopbackMaxRequestHead = 16 * 1024
	// loopbackRequestReadTimeout is how long one connection may take to send
	// its request head.
	loopbackRequestReadTimeout = 10 * time.Second
	// loopbackCloseGrace is how long closing waits for an accepted tab's
	// page to be written.
	loopbackCloseGrace = time.Second
	// loopbackWriteTimeout bounds writing one answer.
	loopbackWriteTimeout = 5 * time.Second
	// loopbackMaxConnections bounds the connections handled at once. A
	// browser opens a handful; a local process opening them faster than
	// the read timeout closes them would otherwise hold a file descriptor
	// and a goroutine for each, in this process. Past the bound the listener
	// stops accepting until one closes, and the kernel's backlog holds the
	// rest.
	loopbackMaxConnections = 64
)

// After an answer, how long and how much of what the browser still sends is
// read and dropped before the connection closes. A socket closed with bytes
// unread answers them with a reset, and macOS then throws away the answer
// the browser had not read yet (an oversized head's 400, a 404 left with an
// unread body). Tests shorten them.
var (
	loopbackDrainTimeout       = 2 * time.Second
	loopbackDrainLimit   int64 = 1 << 20
)

// oauthErrors are the error values RFC 6749 section 4.1.2.1 defines. Only
// these are kept from a callback: anything else is a value somebody else
// chose, and it could be the state or a code, which must never reach an
// error.
var oauthErrors = map[string]bool{
	"invalid_request": true, "unauthorized_client": true, "access_denied": true,
	"unsupported_response_type": true, "invalid_scope": true, "server_error": true,
	"temporarily_unavailable": true,
}

// loopbackPageHeaders go with every answer. The pages load nothing, and
// nothing they show may be cached or leak the callback's address (code and
// state) as a referrer.
var loopbackPageHeaders = [][2]string{
	{"Content-Type", "text/html; charset=utf-8"},
	{"Cache-Control", "no-store"},
	{"Referrer-Policy", "no-referrer"},
	{"Content-Security-Policy", "default-src 'none'"},
	{"X-Content-Type-Options", "nosniff"},
}

var loopbackReasons = map[int]string{200: "OK", 400: "Bad Request", 404: "Not Found"}

// callbackOutcome is what an accepted callback says.
type callbackOutcome string

const (
	callbackCode   callbackOutcome = "code"
	callbackDenied callbackOutcome = "denied"
	callbackFailed callbackOutcome = "failed"
)

// loopbackCallback is an accepted callback: the browser came back with this
// sign-in's state. code is set for callbackCode; errorParam is the error the
// page sent, "" when it sent none or an empty one.
type loopbackCallback struct {
	outcome    callbackOutcome
	code       string
	errorParam string
}

// String keeps a printed callback from showing the code.
func (c loopbackCallback) String() string {
	return fmt.Sprintf("loopbackCallback{outcome:%s code:%s error:%q}", c.outcome, redactSecret(c.code), c.errorParam)
}

// classifyRequest is what the listener answers one request with, and the
// callback it accepts: 404 for anything but GET on exactly /callback; 400
// when state is missing, given more than once, or not this sign-in's
// (compared in constant time, as UTF-8 bytes); otherwise 200 and the
// callback. An error wins over a code sent with it: a refusal that also
// carries a code is still a refusal. Query values are decoded once, as a
// form decodes them.
func classifyRequest(method, target, state string) (int, *loopbackCallback) {
	path, query, _ := strings.Cut(target, "?")
	if method != "GET" || path != LoopbackCallbackPath {
		return 404, nil
	}
	values := map[string][]string{}
	for _, pair := range formPairs(query) {
		values[pair[0]] = append(values[pair[0]], pair[1])
	}
	given := values["state"]
	if len(given) != 1 || subtle.ConstantTimeCompare([]byte(given[0]), []byte(state)) != 1 {
		return 400, nil
	}
	if errs := values["error"]; len(errs) > 0 {
		if len(errs) == 1 && errs[0] == "access_denied" {
			return 200, &loopbackCallback{outcome: callbackDenied, errorParam: "access_denied"}
		}
		errorParam := ""
		if oauthErrors[errs[0]] {
			errorParam = errs[0]
		}
		return 200, &loopbackCallback{outcome: callbackFailed, errorParam: errorParam}
	}
	if codes := values["code"]; len(codes) == 1 && codes[0] != "" {
		return 200, &loopbackCallback{outcome: callbackCode, code: codes[0]}
	}
	return 200, &loopbackCallback{outcome: callbackFailed}
}

// formPairs splits a query the way Python's parse_qsl(keep_blank_values=True)
// does, which is how the reference reads a callback: on "&" only, skipping
// empty pieces, a piece without "=" being a name with an empty value, and
// each name and value decoded by formUnquote. url.ParseQuery differs: it
// drops a piece holding ";" and the whole of a value with a bad escape.
func formPairs(query string) [][2]string {
	var pairs [][2]string
	if query == "" {
		return pairs
	}
	for _, piece := range strings.Split(query, "&") {
		if piece == "" {
			continue
		}
		name, value, _ := strings.Cut(piece, "=")
		pairs = append(pairs, [2]string{formUnquote(name), formUnquote(value)})
	}
	return pairs
}

// formUnquote decodes one form value as Python's unquote_plus does: "+" is a
// space; in each run of ASCII characters a %XX (either case) is that byte and
// any other "%" stays as it is, and the bytes are read as UTF-8, an invalid
// sequence becoming U+FFFD once per maximal subpart; a character outside
// ASCII stays as it is.
func formUnquote(text string) string {
	text = strings.ReplaceAll(text, "+", " ")
	if !strings.Contains(text, "%") {
		return text
	}
	var out strings.Builder
	for start := 0; start < len(text); {
		end := start
		if text[start] < utf8.RuneSelf {
			for end < len(text) && text[end] < utf8.RuneSelf {
				end++
			}
			out.WriteString(utf8Replacing(percentBytes(text[start:end])))
		} else {
			// Bytes of characters outside ASCII are never below 0x80.
			for end < len(text) && text[end] >= utf8.RuneSelf {
				end++
			}
			out.WriteString(text[start:end])
		}
		start = end
	}
	return out.String()
}

// percentBytes turns every %XX into its byte and leaves any other "%".
func percentBytes(text string) []byte {
	out := make([]byte, 0, len(text))
	for index := 0; index < len(text); index++ {
		if text[index] == '%' && index+2 < len(text) {
			high, highOK := hexValue(text[index+1])
			low, lowOK := hexValue(text[index+2])
			if highOK && lowOK {
				out = append(out, high<<4|low)
				index += 2
				continue
			}
		}
		out = append(out, text[index])
	}
	return out
}

func hexValue(digit byte) (byte, bool) {
	switch {
	case digit >= '0' && digit <= '9':
		return digit - '0', true
	case digit >= 'a' && digit <= 'f':
		return digit - 'a' + 10, true
	case digit >= 'A' && digit <= 'F':
		return digit - 'A' + 10, true
	}
	return 0, false
}

// utf8Replacing reads raw as UTF-8, replacing each maximal subpart of an
// ill-formed sequence with one U+FFFD (Unicode's recommended practice, and
// what Python's "replace" does). utf8.DecodeRune would give one U+FFFD per
// byte, and strings.ToValidUTF8 one per run.
func utf8Replacing(raw []byte) string {
	if utf8.Valid(raw) {
		return string(raw)
	}
	var out strings.Builder
	for index := 0; index < len(raw); {
		r, size := utf8.DecodeRune(raw[index:])
		if r != utf8.RuneError || size > 1 {
			out.Write(raw[index : index+size])
			index += size
			continue
		}
		out.WriteRune(utf8.RuneError)
		index += illFormedLength(raw[index:])
	}
	return out.String()
}

// illFormedLength is the length of the maximal subpart of an ill-formed
// sequence at the start of raw: its lead byte and the continuation bytes
// that could still have completed it.
func illFormedLength(raw []byte) int {
	lead := raw[0]
	low, high := byte(0x80), byte(0xBF)
	var continuations int
	switch {
	case lead >= 0xC2 && lead <= 0xDF:
		continuations = 1
	case lead == 0xE0:
		continuations, low = 2, 0xA0
	case lead >= 0xE1 && lead <= 0xEC, lead == 0xEE, lead == 0xEF:
		continuations = 2
	case lead == 0xED:
		continuations, high = 2, 0x9F
	case lead == 0xF0:
		continuations, low = 3, 0x90
	case lead >= 0xF1 && lead <= 0xF3:
		continuations = 3
	case lead == 0xF4:
		continuations, high = 3, 0x8F
	default:
		return 1
	}
	length := 1
	for taken := 0; taken < continuations && length < len(raw); taken++ {
		next := raw[length]
		if taken > 0 {
			low, high = 0x80, 0xBF
		}
		if next < low || next > high {
			break
		}
		length++
	}
	return length
}

// loopbackPage is a page that loads nothing: no script, no style sheet, no
// image, no link.
func loopbackPage(text string) []byte {
	return []byte("<!doctype html>\n<html lang=\"en\"><head><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">" +
		"<title>Thalovant</title></head><body><p>" + text + "</p></body></html>\n")
}

// loopbackResponse is a whole HTTP/1.1 answer: the status, the page
// headers, and a small page. Every connection is closed after one answer.
// The answer to a HEAD carries no body (RFC 9110 section 9.3.2); its
// Content-Length is the length the page would have had.
func loopbackResponse(status int, text string, head bool) []byte {
	body := loopbackPage(text)
	var out bytes.Buffer
	out.WriteString("HTTP/1.1 " + strconv.Itoa(status) + " " + loopbackReasons[status] + "\r\n")
	for _, header := range loopbackPageHeaders {
		out.WriteString(header[0] + ": " + header[1] + "\r\n")
	}
	out.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\nConnection: close\r\n\r\n")
	if !head {
		out.Write(body)
	}
	return out.Bytes()
}

var errHeadTooLarge = errors.New("request head too large")

// readRequestHead reads up to the blank line that ends a request head, at
// most loopbackMaxRequestHead bytes of it. A head over that is
// errHeadTooLarge, with what was read of it, so that its method still
// decides whether the 400 carries a body.
func readRequestHead(conn net.Conn) ([]byte, error) {
	separator := []byte("\r\n\r\n")
	buffer := make([]byte, 0, 1024)
	chunk := make([]byte, 2048)
	for {
		n, err := conn.Read(chunk)
		buffer = append(buffer, chunk[:n]...)
		if index := bytes.Index(buffer, separator); index >= 0 {
			if index > loopbackMaxRequestHead {
				return buffer, errHeadTooLarge
			}
			return buffer[:index], nil
		}
		if len(buffer) >= loopbackMaxRequestHead+len(separator) {
			return buffer, errHeadTooLarge
		}
		if err != nil {
			return nil, err
		}
	}
}

// requestLine is the method and target of a request head, read as Latin-1
// as the reference reads it; ok is false when it has no request line.
func requestLine(head []byte) (method, target string, ok bool) {
	line, _, _ := bytes.Cut(head, []byte("\r\n"))
	parts := strings.Split(latin1(line), " ")
	if len(parts) != 3 || !strings.HasPrefix(parts[2], "HTTP/") {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// latin1 reads every byte as the code point of the same number.
func latin1(raw []byte) string {
	runes := make([]rune, len(raw))
	for index, b := range raw {
		runes[index] = rune(b)
	}
	return string(runes)
}

// loopbackListener waits on 127.0.0.1 for the browser to bring one sign-in's
// answer back. start binds the port (before any URL is built), wait returns
// the accepted callback or nil at the deadline, finish says which page its
// tab gets, and close stops everything and waits for every goroutine it ran.
type loopbackListener struct {
	state    string
	listener net.Listener
	// accepted carries the one accepted callback to wait.
	accepted chan *loopbackCallback
	// answer is closed by finish, once signedIn is set.
	answer     chan struct{}
	finishOnce sync.Once
	signedIn   bool
	// goroutines counts the accept loop and every connection's handler.
	goroutines sync.WaitGroup
	// slots holds one token for each connection being handled, at most
	// loopbackMaxConnections.
	slots chan struct{}
	// stopped is closed by close, so that an accept loop waiting for a slot
	// returns.
	stopped chan struct{}

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	// closing is set by close: no connection is kept or accepted after it.
	closing bool
	// acceptedDone is closed once the accepted tab has had its page; nil
	// until a callback is accepted.
	acceptedDone chan struct{}
}

func newLoopbackListener(state string) *loopbackListener {
	return &loopbackListener{
		state:    state,
		accepted: make(chan *loopbackCallback, 1),
		answer:   make(chan struct{}),
		conns:    map[net.Conn]struct{}{},
		slots:    make(chan struct{}, loopbackMaxConnections),
		stopped:  make(chan struct{}),
	}
}

// start binds 127.0.0.1 on a port the system picks and returns the port.
// Never 0.0.0.0, ::1 or a name such as localhost. On Windows the socket is
// exclusive (SO_EXCLUSIVEADDRUSE), so no other process can share the port.
func (l *loopbackListener) start() (int, error) {
	config := net.ListenConfig{Control: loopbackListenControl}
	listener, err := config.Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("%w: could not listen on 127.0.0.1 for the browser to come back: %v", ErrAPI, err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return 0, fmt.Errorf("%w: the loopback listener has no TCP port", ErrAPI)
	}
	l.listener = listener
	l.goroutines.Add(1)
	go l.serve()
	return address.Port, nil
}

// serve accepts connections until the listener is closed, at most
// loopbackMaxConnections handled at once: it takes a slot before each
// accept, and the connection's handler gives it back.
func (l *loopbackListener) serve() {
	defer l.goroutines.Done()
	for {
		select {
		case l.slots <- struct{}{}:
		case <-l.stopped:
			return
		}
		conn, err := l.listener.Accept()
		if err != nil {
			<-l.slots
			if errors.Is(err, net.ErrClosed) || l.isClosing() {
				return
			}
			// A passing failure, such as running out of file descriptors:
			// try again shortly. Closing the listener ends the loop.
			time.Sleep(10 * time.Millisecond)
			continue
		}
		l.goroutines.Add(1)
		go l.handle(conn)
	}
}

func (l *loopbackListener) isClosing() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closing
}

// track keeps conn so that close can end it; false once closing.
func (l *loopbackListener) track(conn net.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing {
		return false
	}
	l.conns[conn] = struct{}{}
	return true
}

func (l *loopbackListener) untrack(conn net.Conn) {
	l.mu.Lock()
	delete(l.conns, conn)
	l.mu.Unlock()
	_ = conn.Close()
}

// handle reads one request head, answers it, and hands an accepted callback
// to wait. One connection's failure is that connection's alone.
func (l *loopbackListener) handle(conn net.Conn) {
	defer l.goroutines.Done()
	// Given back once the connection is closed (untrack runs before this).
	defer func() { <-l.slots }()
	if !l.track(conn) {
		_ = conn.Close()
		return
	}
	defer l.untrack(conn)
	_ = conn.SetReadDeadline(time.Now().Add(loopbackRequestReadTimeout))
	head, err := readRequestHead(conn)
	if errors.Is(err, errHeadTooLarge) {
		// A HEAD answer carries no body, oversized or not.
		l.reply(conn, 400, loopbackWrongStateText, bytes.HasPrefix(head, []byte("HEAD ")))
		return
	}
	if err != nil {
		return // gone, or silent, before a whole head arrived
	}
	method, target, ok := requestLine(head)
	if !ok {
		l.reply(conn, 400, loopbackWrongStateText, false)
		return
	}
	status, callback := classifyRequest(method, target, l.state)
	if callback == nil {
		text := loopbackWrongStateText
		if status == 404 {
			text = loopbackNotFoundText
		}
		l.reply(conn, status, text, method == "HEAD")
		return
	}
	done, accepted := l.accept(callback)
	if !accepted {
		// A second answer with the right state, after the first was
		// accepted, is not this sign-in's any more either.
		l.reply(conn, 400, loopbackWrongStateText, false)
		return
	}
	defer close(done)
	// Until the sign-in has ended, however long the exchange takes: the page
	// says how it ended, so it cannot be written before it has. close always
	// calls finish first, so this never outlives the sign-in.
	<-l.answer
	text := loopbackNotCompletedText
	if l.signedIn {
		text = loopbackSignedInText
	}
	l.reply(conn, 200, text, false)
}

// accept takes the first callback with the right state, and stops listening:
// a new connection is refused from then on, and one already open is still
// answered.
func (l *loopbackListener) accept(callback *loopbackCallback) (chan struct{}, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing || l.acceptedDone != nil {
		return nil, false
	}
	l.acceptedDone = make(chan struct{})
	// Closed before wait hears of the callback, so that once it has, a new
	// connection is refused.
	_ = l.listener.Close()
	l.accepted <- callback // buffered for the one callback ever accepted
	return l.acceptedDone, true
}

// reply sends one answer, then lets the browser finish before the
// connection closes: the write side is shut first, so the browser sees the
// answer end, and what it still sends (the rest of an oversized head, a body
// nobody asked for) is read and dropped until it closes its side, for at
// most loopbackDrainTimeout and loopbackDrainLimit bytes. Closing with bytes
// unread would reset the connection instead, and macOS would lose the
// answer. close ends a drain at once.
func (l *loopbackListener) reply(conn net.Conn, status int, text string, head bool) {
	_ = conn.SetWriteDeadline(time.Now().Add(loopbackWriteTimeout))
	if _, err := conn.Write(loopbackResponse(status, text, head)); err != nil {
		return
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.CloseWrite(); err != nil {
			return
		}
	}
	_ = conn.SetReadDeadline(time.Now().Add(loopbackDrainTimeout))
	_, _ = io.CopyN(io.Discard, conn, loopbackDrainLimit+1)
}

// wait returns the first accepted callback, nil once timeout has passed, or
// ctx's error. The deadline is on the monotonic clock and never cut short:
// a timer that fires early waits again for what is left, and a request
// turned away neither ends the wait nor restarts it.
func (l *loopbackListener) wait(ctx context.Context, timeout time.Duration) (*loopbackCallback, error) {
	deadline := time.Now().Add(timeout)
	for {
		select {
		case callback := <-l.accepted:
			return callback, nil
		default:
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, nil
		}
		timer := time.NewTimer(remaining)
		select {
		case callback := <-l.accepted:
			timer.Stop()
			return callback, nil
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// finish answers the accepted tab: signed in, or not completed. Only the
// first call counts.
func (l *loopbackListener) finish(signedIn bool) {
	l.finishOnce.Do(func() {
		l.signedIn = signedIn
		close(l.answer)
	})
}

// close stops listening, lets an accepted tab have its page (waiting at most
// loopbackCloseGrace), closes every other connection without waiting for it
// -- a browser opens connections it never sends anything on -- and returns
// once every goroutine the listener ran has returned.
func (l *loopbackListener) close() {
	l.finish(false)
	l.mu.Lock()
	if !l.closing {
		close(l.stopped)
	}
	l.closing = true
	done := l.acceptedDone
	l.mu.Unlock()
	if l.listener != nil {
		_ = l.listener.Close()
	}
	if done != nil {
		timer := time.NewTimer(loopbackCloseGrace)
		select {
		case <-done:
		case <-timer.C:
		}
		timer.Stop()
	}
	l.mu.Lock()
	for conn := range l.conns {
		_ = conn.Close()
	}
	l.mu.Unlock()
	l.goroutines.Wait()
}
