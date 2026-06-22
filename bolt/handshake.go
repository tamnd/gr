// Package bolt is gr's Bolt server: the wire protocol the Neo4j drivers speak
// (spec 2060 doc 18). It sits on top of the PackStream codec (the pack package,
// doc 18 §4) and is built up in layers: the version handshake (this file,
// doc 18 §3.1), the chunked message framing (doc 18 §3.3), the message set and
// session state machine (doc 18 §5), and the graph value mapping (doc 18 §6).
//
// The handshake is the very first thing on a Bolt connection, before any
// chunking and before any PackStream: a 20-byte client preamble-and-proposal
// followed by a 4-byte server selection.
package bolt

import (
	"errors"
	"fmt"
	"io"
)

// Magic is the 4-byte identification preamble a client sends first (doc 18 §3.1).
// It lets a server tell a Bolt connection apart from HTTP or garbage on the same
// port. It is NORMATIVE that the first four bytes of a Bolt connection are
// exactly these.
var Magic = [4]byte{0x60, 0x60, 0xB0, 0x17}

// Version is one Bolt protocol version, a major and a minor number (doc 18 §3.1,
// §4.10). gr negotiates a version per connection and conditions a handful of
// encoding choices on it (node arity, datetime encoding, whether LOGON is split
// out), but the message protocol is otherwise the same across the range.
type Version struct {
	Major uint8
	Minor uint8
}

// String renders a version as "major.minor", the form drivers and logs use.
func (v Version) String() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// word encodes a version as the 4-byte server-selection reply 00 00 minor major
// (doc 18 §3.1 step 3). The reply never uses the range encoding: it names exactly
// one version.
func (v Version) word() [4]byte { return [4]byte{0x00, 0x00, v.Minor, v.Major} }

// Zero is the version word a server sends to mean "no compatible version"
// (doc 18 §3.1 step 3): four zero bytes, after which the server closes the
// connection.
var zeroWord = [4]byte{0x00, 0x00, 0x00, 0x00}

// Supported is gr's set of Bolt versions, highest first (doc 18 §4.10, §15.1).
// 4.4 is the floor (older 4.x and all 3.x drivers are refused); 5.7 is the
// ceiling. The list is the configuration of the codec, not a fork per version.
var Supported = []Version{
	{5, 7},
	{5, 6},
	{5, 4},
	{5, 3},
	{5, 2},
	{5, 1},
	{5, 0},
	{4, 4},
}

// manifestSentinel is the proposal word a 5.7+ client sends to request the
// richer handshake manifest negotiation (doc 18 §3.2): 00 00 01 FF. gr does not
// implement the manifest, so it ignores this slot and selects from the client's
// three real proposals, which a driver always sends alongside the sentinel.
var manifestSentinel = proposal{Reserved: 0x00, Range: 0x00, Minor: 0x01, Major: 0xFF}

// proposal is one 4-byte version proposal from a client (doc 18 §3.1 step 2).
// A proposal offers a major version, a highest minor, and a count of consecutive
// lower minors that are also acceptable: 00 RR mm MM, where MM is the major, mm
// the highest minor, and RR how many lower minors down from mm are also offered.
type proposal struct {
	Reserved uint8
	Range    uint8
	Minor    uint8
	Major    uint8
}

// ErrNotBolt is returned when the client's first four bytes are not the Bolt
// magic preamble (doc 18 §3.1 step 1); the caller closes the connection.
var ErrNotBolt = errors.New("bolt: connection did not begin with the Bolt magic preamble")

// ErrNoCompatibleVersion is returned when none of the client's proposals
// intersect gr's supported set (doc 18 §3.1 step 3). The handshake has already
// written the 00 00 00 00 "no version" reply; the caller closes the connection.
var ErrNoCompatibleVersion = errors.New("bolt: no Bolt version in common with the client")

// Handshake performs the server side of the Bolt version handshake on conn
// (doc 18 §3.1). It reads the 4-byte magic preamble and the four 4-byte version
// proposals (20 bytes total), selects the highest version gr supports that the
// client offered, writes the 4-byte selection, and returns it. If the preamble
// is wrong it returns ErrNotBolt without writing a reply; if no version is in
// common it writes the 00 00 00 00 "no version" reply and returns
// ErrNoCompatibleVersion. After a successful handshake the connection switches to
// chunked framing and the message protocol begins.
func Handshake(conn io.ReadWriter) (Version, error) {
	var preamble [4]byte
	if _, err := io.ReadFull(conn, preamble[:]); err != nil {
		return Version{}, fmt.Errorf("bolt: reading magic preamble: %w", err)
	}
	if preamble != Magic {
		return Version{}, ErrNotBolt
	}

	var raw [16]byte
	if _, err := io.ReadFull(conn, raw[:]); err != nil {
		return Version{}, fmt.Errorf("bolt: reading version proposals: %w", err)
	}
	var proposals [4]proposal
	for i := range proposals {
		off := i * 4
		proposals[i] = proposal{
			Reserved: raw[off],
			Range:    raw[off+1],
			Minor:    raw[off+2],
			Major:    raw[off+3],
		}
	}

	selected, ok := selectVersion(proposals[:])
	if !ok {
		if _, err := conn.Write(zeroWord[:]); err != nil {
			return Version{}, fmt.Errorf("bolt: writing no-version reply: %w", err)
		}
		return Version{}, ErrNoCompatibleVersion
	}

	word := selected.word()
	if _, err := conn.Write(word[:]); err != nil {
		return Version{}, fmt.Errorf("bolt: writing version selection: %w", err)
	}
	return selected, nil
}

// selectVersion picks the highest version gr supports that the client offered,
// scanning the proposals in the client's preference order and, within each, the
// range from the highest minor down to minor-range (doc 18 §3.1 step 3). The
// padding slot 00 00 00 00 and the manifest sentinel 00 00 01 FF are skipped. It
// returns the first offered version that is in gr's supported set, which, because
// the client lists proposals in descending preference and gr scans in that
// order, is the highest mutually supported version.
func selectVersion(proposals []proposal) (Version, bool) {
	supported := make(map[Version]bool, len(Supported))
	for _, v := range Supported {
		supported[v] = true
	}
	for _, p := range proposals {
		if p == (proposal{}) || p == manifestSentinel {
			continue
		}
		for off := 0; off <= int(p.Range); off++ {
			minor := int(p.Minor) - off
			if minor < 0 {
				break
			}
			v := Version{Major: p.Major, Minor: uint8(minor)}
			if supported[v] {
				return v, true
			}
		}
	}
	return Version{}, false
}
