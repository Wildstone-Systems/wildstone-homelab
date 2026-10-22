package agent

// ListRadios (protocol minor 25): the platform-neutral half — the
// classification table and the string hygiene every value read from
// sysfs passes through. The reads themselves live in radios_linux.go.

import (
	"strings"
	"unicode"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// radioUSBKinds classifies a serial adapter by its EXACT USB
// vendor:product pair, and only where that pair belongs to a product that
// speaks exactly one radio protocol. Generic USB-UART bridges (CP210x
// 10c4:ea60, CH340 1a86:7523, FTDI 0403:6001/6015, CH9102 1a86:55d4) are
// deliberately absent: the same chip ships in Zigbee, Thread and
// multiprotocol sticks and in things that are not radios at all, and a
// multiprotocol stick's protocol is whatever firmware it was flashed with.
// An adapter not in this table is RADIO_KIND_UNKNOWN.
var radioUSBKinds = map[string]wsv1.RadioKind{
	"1cf1:0030": wsv1.RadioKind_RADIO_KIND_ZIGBEE, // dresden elektronik ConBee II
	"0658:0200": wsv1.RadioKind_RADIO_KIND_ZWAVE,  // Sigma Designs Z-Wave (Aeotec Z-Stick Gen5, ZST10)
}

// classifySerial picks a serial adapter's kind and says why.
func classifySerial(vendor, product string) (wsv1.RadioKind, string) {
	if vendor == "" || product == "" {
		return wsv1.RadioKind_RADIO_KIND_UNKNOWN, "no usb id; serial adapter"
	}
	pair := vendor + ":" + product
	if k, ok := radioUSBKinds[pair]; ok {
		return k, "usb id " + pair
	}
	return wsv1.RadioKind_RADIO_KIND_UNKNOWN, "usb id " + pair + " does not identify a single radio protocol"
}

// maxRadioAttrLen caps every sysfs string relayed to Keystone. Real
// values are a few dozen bytes; the cap only bounds a hostile or broken
// device descriptor.
const maxRadioAttrLen = 128

// cleanAttr trims a sysfs attribute and drops anything that is not a
// printable character, so a USB descriptor cannot smuggle control
// characters into the API, the Console or a log line.
func cleanAttr(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		}
		if b.Len() >= maxRadioAttrLen {
			break
		}
	}
	return b.String()
}
