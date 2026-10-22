//go:build linux

package agent

// ListPorts parses /proc/net/tcp and /proc/net/tcp6 for sockets in LISTEN
// state (st field == "0A"). No netstat/ss shell-out (ADR-W003 §A4.5).

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// procNetPaths maps proto name -> /proc/net file. Package-level var so
// tests can point it at fixture files.
var procNetPaths = map[string]string{
	"tcp":  "/proc/net/tcp",
	"tcp6": "/proc/net/tcp6",
}

func (a *Agent) listPorts() (*wsv1.OpResult_ListPorts, *wsv1.OpError) {
	var ports []*wsv1.ListeningPort
	for _, proto := range []string{"tcp", "tcp6"} {
		path := procNetPaths[proto]
		p, err := parseProcNetListen(path, proto)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "parse %s: %v", path, err)
		}
		ports = append(ports, p...)
	}
	return &wsv1.OpResult_ListPorts{ListPorts: &wsv1.ListPortsResult{Ports: ports}}, nil
}

// parseProcNetListen parses one /proc/net/tcp{,6}-formatted file, returning
// only sockets in LISTEN state (st == 0A).
func parseProcNetListen(path, proto string) ([]*wsv1.ListeningPort, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []*wsv1.ListeningPort
	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue // header line
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		if fields[3] != "0A" { // TCP_LISTEN
			continue
		}
		hostPort := strings.SplitN(fields[1], ":", 2)
		if len(hostPort) != 2 {
			continue
		}
		ip, err := decodeHexIP(hostPort[0])
		if err != nil {
			continue
		}
		portNum, err := strconv.ParseUint(hostPort[1], 16, 32)
		if err != nil {
			continue
		}
		out = append(out, &wsv1.ListeningPort{Proto: proto, Port: uint32(portNum), LocalIp: ip})
	}
	return out, sc.Err()
}

// decodeHexIP decodes /proc/net/tcp{,6}'s hex-encoded, little-endian
// (per 32-bit word) address representation into a dotted/colon string.
func decodeHexIP(hexStr string) (string, error) {
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		return "", err
	}
	switch len(b) {
	case 4:
		return fmt.Sprintf("%d.%d.%d.%d", b[3], b[2], b[1], b[0]), nil
	case 16:
		var out [16]byte
		for w := 0; w < 4; w++ {
			out[w*4+0] = b[w*4+3]
			out[w*4+1] = b[w*4+2]
			out[w*4+2] = b[w*4+1]
			out[w*4+3] = b[w*4+0]
		}
		return net.IP(out[:]).String(), nil
	default:
		return "", fmt.Errorf("unexpected address length %d", len(b))
	}
}
