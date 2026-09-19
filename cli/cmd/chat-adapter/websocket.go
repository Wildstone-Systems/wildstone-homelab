package main

// A minimal WebSocket client (RFC 6455), for the transports that deliver
// messages over one: Discord's gateway and Slack's Socket Mode.
//
// Standard library only, like the rest of this module. It does exactly
// what those two need and refuses the rest: text messages in and out,
// fragmented messages reassembled, pings answered, close honoured. No
// extensions (no permessage-deflate: it is not offered, so a server may
// not use it), no subprotocols, and no server side.

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	wsText   = 0x1
	wsBinary = 0x2
	wsClose  = 0x8
	wsPing   = 0x9
	wsPong   = 0xA
	wsCont   = 0x0

	// wsMaxMessage bounds one reassembled message. Gateway payloads are
	// kilobytes; a server sending more is not one to buffer for.
	wsMaxMessage = 4 << 20
	// wsGUID is RFC 6455's fixed accept-key suffix.
	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

// errWSClosed is a connection the server closed.
var errWSClosed = errors.New("websocket: closed by the server")

type wsConn struct {
	conn net.Conn
	r    *bufio.Reader
	wmu  sync.Mutex
}

// wsDial opens a WebSocket to a ws:// or wss:// URL.
func wsDial(rawURL string, timeout time.Duration) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	var conn net.Conn
	d := &net.Dialer{Timeout: timeout}
	switch u.Scheme {
	case "wss":
		if u.Port() == "" {
			host += ":443"
		}
		conn, err = tls.DialWithDialer(d, "tcp", host, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
	case "ws":
		if u.Port() == "" {
			host += ":80"
		}
		conn, err = d.Dial("tcp", host)
	default:
		return nil, fmt.Errorf("websocket: unsupported scheme %q", u.Scheme)
	}
	if err != nil {
		return nil, err
	}
	keyRaw := make([]byte, 16)
	if _, err := rand.Read(keyRaw); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyRaw)
	path := u.RequestURI()
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"User-Agent: wildstone-chat-adapter\r\n\r\n"
	conn.SetDeadline(time.Now().Add(timeout)) //nolint:errcheck
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("websocket handshake: %w", err)
	}
	resp.Body.Close()
	sum := sha1.Sum([]byte(key + wsGUID))
	if resp.StatusCode != http.StatusSwitchingProtocols ||
		!strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") ||
		resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		conn.Close()
		return nil, fmt.Errorf("websocket handshake refused: %s", resp.Status)
	}
	conn.SetDeadline(time.Time{}) //nolint:errcheck
	return &wsConn{conn: conn, r: br}, nil
}

// WriteText sends one text message. Safe for concurrent use.
func (c *wsConn) WriteText(p []byte) error { return c.writeFrame(wsText, p) }

func (c *wsConn) writeFrame(op byte, p []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	hdr := []byte{0x80 | op}
	switch n := len(p); {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	// Every client frame is masked (RFC 6455 5.3).
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	hdr = append(hdr, mask...)
	body := make([]byte, len(p))
	for i := range p {
		body[i] = p[i] ^ mask[i%4]
	}
	c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second)) //nolint:errcheck
	_, err := c.conn.Write(append(hdr, body...))
	return err
}

// ReadMessage returns the next text or binary message, answering pings
// and reassembling fragments on the way. A close from the server is
// errWSClosed.
func (c *wsConn) ReadMessage() ([]byte, error) {
	var msg []byte
	started := false
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case wsPing:
			if err := c.writeFrame(wsPong, payload); err != nil {
				return nil, err
			}
			continue
		case wsPong:
			continue
		case wsClose:
			c.writeFrame(wsClose, nil) //nolint:errcheck // best effort
			return nil, errWSClosed
		case wsText, wsBinary:
			if started {
				return nil, errors.New("websocket: new message inside a fragmented one")
			}
			started = true
			msg = append(msg[:0], payload...)
		case wsCont:
			if !started {
				return nil, errors.New("websocket: continuation with no message")
			}
			msg = append(msg, payload...)
		default:
			return nil, fmt.Errorf("websocket: unknown opcode %d", op)
		}
		if len(msg) > wsMaxMessage {
			return nil, errors.New("websocket: message too large")
		}
		if fin {
			return msg, nil
		}
	}
}

func (c *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.r, h[:]); err != nil {
		return
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0F
	if h[0]&0x70 != 0 {
		// RSV bits mean an extension, and none was negotiated.
		return false, 0, nil, errors.New("websocket: reserved bits set")
	}
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.r, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.r, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > wsMaxMessage {
		return false, 0, nil, errors.New("websocket: frame too large")
	}
	var mask []byte
	if h[1]&0x80 != 0 {
		// A server must not mask; tolerate it rather than trust it.
		mask = make([]byte, 4)
		if _, err = io.ReadFull(c.r, mask); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.r, payload); err != nil {
		return
	}
	if mask != nil {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

// Close closes the connection, telling the server first.
func (c *wsConn) Close() error {
	c.writeFrame(wsClose, []byte{0x03, 0xE8}) //nolint:errcheck // 1000, normal closure; best effort
	return c.conn.Close()
}
