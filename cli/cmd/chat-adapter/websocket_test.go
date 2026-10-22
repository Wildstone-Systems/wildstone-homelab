package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// wsPeer is the server end of one test connection: just enough of RFC
// 6455 to talk to wsConn, and strict about what a client must do.
type wsPeer struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

// wsServer serves handle over a WebSocket at the returned ws:// URL.
func wsServer(t *testing.T, handle func(*wsPeer)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Sec-WebSocket-Key")
		if r.Header.Get("Upgrade") != "websocket" || key == "" {
			http.Error(w, "not a websocket", http.StatusBadRequest)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		sum := sha1.Sum([]byte(key + wsGUID))
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
		rw.Flush()
		p := &wsPeer{t: t, conn: conn, r: rw.Reader}
		defer conn.Close()
		handle(p)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// send writes an unmasked server frame.
func (p *wsPeer) send(fin bool, op byte, payload string) {
	b := byte(op)
	if fin {
		b |= 0x80
	}
	hdr := []byte{b}
	switch n := len(payload); {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	p.conn.Write(append(hdr, payload...))
}

func (p *wsPeer) text(s string) { p.send(true, wsText, s) }

// read returns the next client frame, failing the test if it was not
// masked — every client frame must be.
func (p *wsPeer) read() (byte, string, bool) {
	p.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var h [2]byte
	if _, err := io.ReadFull(p.r, h[:]); err != nil {
		return 0, "", false
	}
	if h[1]&0x80 == 0 {
		p.t.Error("client sent an unmasked frame")
	}
	n := int(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		io.ReadFull(p.r, b[:])
		n = int(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		io.ReadFull(p.r, b[:])
		n = int(binary.BigEndian.Uint64(b[:]))
	}
	mask := make([]byte, 4)
	io.ReadFull(p.r, mask)
	body := make([]byte, n)
	io.ReadFull(p.r, body)
	for i := range body {
		body[i] ^= mask[i%4]
	}
	return h[0] & 0x0F, string(body), true
}

func TestWebSocketRoundTrip(t *testing.T) {
	got := make(chan string, 4)
	url := wsServer(t, func(p *wsPeer) {
		p.text("hello")
		// A fragmented message, with a ping in the middle of it.
		p.send(false, wsText, "frag")
		p.send(true, wsPing, "are you there")
		p.send(true, wsCont, "mented")
		// A message long enough to need the 16-bit length.
		p.text(strings.Repeat("x", 300))
		for {
			op, body, ok := p.read()
			if !ok {
				return
			}
			switch op {
			case wsPong:
				got <- "pong:" + body
			case wsText:
				got <- "text:" + body
				p.send(true, wsClose, "")
				return
			}
		}
	})
	c, err := wsDial(url, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, want := range []string{"hello", "fragmented", strings.Repeat("x", 300)} {
		m, err := c.ReadMessage()
		if err != nil || string(m) != want {
			t.Fatalf("read %q, %v; want %q", m, err, want)
		}
	}
	if err := c.WriteText([]byte(`{"op":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadMessage(); err != errWSClosed {
		t.Errorf("after the server's close: %v", err)
	}
	for _, want := range []string{"pong:are you there", `text:{"op":1}`} {
		select {
		case g := <-got:
			if g != want {
				t.Errorf("server saw %q, want %q", g, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("server never saw %q", want)
		}
	}
}

func TestWebSocketRefusesABadHandshake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // not 101
	}))
	defer srv.Close()
	if _, err := wsDial("ws"+strings.TrimPrefix(srv.URL, "http"), 5*time.Second); err == nil {
		t.Fatal("a plain 200 was taken for a WebSocket")
	}
}
