// git.go: the git object model — pkt-line framing, zlib objects, packfiles.
//
// This is standard git, not suite-specific: object ids are SHA-1 over
// "<type> <len>\0" + content, packfiles are version 2 with a SHA-1 trailer,
// deltas are REF_DELTA/OFS_DELTA as git writes them. The suite's client is a
// git client, so speaking git exactly is what the checks measure.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
)

// ---------------------------------------------------------------- pkt-line

// pktLine encodes one pkt-line frame: a 4-hex-digit length (including the
// 4 length bytes) followed by the payload. A nil payload is the flush packet.
func pktLine(payload []byte) []byte {
	if payload == nil {
		return []byte("0000")
	}
	out := make([]byte, 4, 4+len(payload))
	length := 4 + len(payload)
	out[0] = hexDigit(byte(length >> 12))
	out[1] = hexDigit(byte(length >> 8))
	out[2] = hexDigit(byte(length >> 4))
	out[3] = hexDigit(byte(length))
	return append(out, payload...)
}

func hexDigit(b byte) byte {
	b &= 0xf
	if b < 10 {
		return '0' + b
	}
	return 'a' + b - 10
}

// appendPktLine appends one pkt-line frame to buf: a 4-hex-digit length
// (including the 4 length bytes) followed by the payload. A nil payload is
// the flush packet ("0000"). Unlike pktLine it allocates nothing: the caller
// owns buf and reuses it across lines. The bytes produced are identical to
// pktLine's.
func appendPktLine(buf []byte, payload []byte) []byte {
	if payload == nil {
		return append(buf, '0', '0', '0', '0')
	}
	length := 4 + len(payload)
	buf = append(buf,
		hexDigit(byte(length>>12)),
		hexDigit(byte(length>>8)),
		hexDigit(byte(length>>4)),
		hexDigit(byte(length)))
	return append(buf, payload...)
}

// hexVal parses one hex digit, -1 when the byte is not hex.
func hexVal(b byte) int {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0')
	case b >= 'a' && b <= 'f':
		return int(b-'a') + 10
	case b >= 'A' && b <= 'F':
		return int(b-'A') + 10
	}
	return -1
}

// pktLineReader reads pkt-line frames from a stream.
type pktLineReader struct {
	data []byte
	pos  int
}

func (r *pktLineReader) next() ([]byte, error) {
	if r.pos+4 > len(r.data) {
		return nil, io.EOF
	}
	// Parse the 4-hex-digit length by hand: strconv.ParseInt(string(...))
	// allocates a string per packet. Equivalent to ParseInt(s, 16, 32) on
	// the same 4 bytes, including its acceptance of a leading '+'.
	raw := r.data[r.pos : r.pos+4]
	length := 0
	for i, b := range raw {
		if i == 0 && b == '+' {
			continue
		}
		v := hexVal(b)
		if v < 0 {
			return nil, fmt.Errorf("bad pkt-line length %q", string(raw))
		}
		length = length*16 + v
	}
	if length < 0 || length > 65520 {
		return nil, fmt.Errorf("bad pkt-line length %q", string(raw))
	}
	r.pos += 4
	if length == 0 {
		return nil, nil // flush
	}
	if length == 1 || length == 2 {
		return []byte{}, nil // delim / response-end: control packet, no payload
	}
	if length < 4 || r.pos+int(length)-4 > len(r.data) {
		return nil, io.ErrUnexpectedEOF
	}
	payload := r.data[r.pos : r.pos+int(length)-4]
	r.pos += int(length) - 4
	return payload, nil
}

// ---------------------------------------------------------------- objects

// objectHeader is the "<type> <len>\0" prefix git hashes and stores.
func objectHeader(typ string, size int) []byte {
	return []byte(typ + " " + strconv.Itoa(size) + "\x00")
}

// encodeObject returns the bytes git hashes for an object (header + content).
func encodeObject(typ string, data []byte) []byte {
	header := objectHeader(typ, len(data))
	out := make([]byte, 0, len(header)+len(data))
	out = append(out, header...)
	return append(out, data...)
}

// zlibCompress deflates data at the default level (git's default).
func zlibCompress(data []byte) []byte {
	var buf bytes.Buffer
	w, _ := zlib.NewWriterLevel(&buf, zlib.DefaultCompression)
	_, _ = w.Write(data)
	_ = w.Close()
	return buf.Bytes()
}

// zlibDecompress inflates a zlib stream.
func zlibDecompress(data []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// fastGitOID is GitOID without the per-write allocations: the "<type> <len>\0"
// header is built on the stack and the hex encoding lands in a stack array,
// so the only allocation is the returned string itself (the map key).
func fastGitOID(typ string, data []byte) string {
	var hdr [32]byte
	n := copy(hdr[:], typ)
	hdr[n] = ' '
	n++
	n += appendDecimal(hdr[n:], len(data))
	hdr[n] = 0
	n++
	h := sha1.New()
	h.Write(hdr[:n])
	h.Write(data)
	var sum [20]byte
	h.Sum(sum[:0])
	var out [40]byte
	hex.Encode(out[:], sum[:])
	return string(out[:])
}

// appendDecimal writes n in decimal into buf, returning the bytes written.
func appendDecimal(buf []byte, n int) int {
	if n == 0 {
		buf[0] = '0'
		return 1
	}
	// Count digits, then fill backwards.
	digits := 0
	for m := n; m > 0; m /= 10 {
		digits++
	}
	for i := digits - 1; i >= 0; i-- {
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return digits
}

// zlibReaderPool reuses zlib readers across inflates: a fresh zlib.NewReader
// allocates a bufio buffer and a flate decompressor per stream. Pooled
// readers are reset with zlib.Resetter; the underlying bytes.Reader
// implements flate.Reader, so the decompressor never over-reads and the
// consumed-length accounting in inflateAt stays exact.
var zlibReaderPool sync.Pool // io.ReadCloser

// inflateBufPool reuses the intermediate buffer inflateAt reads into; the
// retained object data is copied out of it into an exactly-sized slice, so a
// pooled buffer is never aliased by a stored object.
var inflateBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func pooledZlibReader(br *bytes.Reader) (io.ReadCloser, error) {
	if v := zlibReaderPool.Get(); v != nil {
		r := v.(io.ReadCloser)
		if rs, ok := r.(zlib.Resetter); ok {
			if err := rs.Reset(br, nil); err == nil {
				return r, nil
			}
		}
		// Reset failed: drop the pooled reader; rewind (Reset may have
		// consumed the header) and fall through to a fresh one.
		_, _ = br.Seek(0, io.SeekStart)
	}
	return zlib.NewReader(br)
}

func releaseZlibReader(r io.ReadCloser) {
	_ = r.Close()
	zlibReaderPool.Put(r)
}

// ---------------------------------------------------------------- packfile writing

// packObjectType numbers, as git numbers them in a pack.
const (
	packCommit = 1
	packTree   = 2
	packBlob   = 3
	packTag    = 4
)

func packTypeNumber(typ string) (int, error) {
	switch typ {
	case "commit":
		return packCommit, nil
	case "tree":
		return packTree, nil
	case "blob":
		return packBlob, nil
	case "tag":
		return packTag, nil
	}
	return 0, fmt.Errorf("unknown object type %q", typ)
}

// packObjectHeader is the varint type+size header of one pack entry.
func packObjectHeader(typ int, size int) []byte {
	var out []byte
	b := byte((size & 0x0f) | (typ << 4))
	size >>= 4
	for size > 0 {
		out = append(out, b|0x80)
		b = byte(size & 0x7f)
		size >>= 7
	}
	return append(out, b)
}

// writePack serialises objects as a packfile (version 2, plain entries, no deltas).
// A pack entry's declared size is the raw content length and the deflated
// stream holds exactly the raw content (no "<type> <len>\0" header — that
// header exists only in the hashed/loose form, not in packs).
func writePack(objects []*GitObject) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	_ = binary.Write(&buf, binary.BigEndian, uint32(2))
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(objects)))
	for _, obj := range objects {
		typ, err := packTypeNumber(obj.Type)
		if err != nil {
			continue
		}
		buf.Write(packObjectHeader(typ, len(obj.Data)))
		buf.Write(zlibCompress(obj.Data))
	}
	// Trailer: SHA-1 of everything before it.
	sum := sha1Sum(buf.Bytes())
	buf.Write(sum)
	return buf.Bytes()
}

// sha1Sum is SHA-1 of data.
func sha1Sum(data []byte) []byte {
	sum := sha1.Sum(data)
	return sum[:]
}

// ---------------------------------------------------------------- packfile reading

// packEntry is one parsed pack entry before delta resolution.
type packEntry struct {
	typ    string
	data   []byte // full object content (without header), deltas already applied
	base   string // REF_DELTA base oid, "" when none
	baseOf int    // OFS_DELTA base offset, -1 when none
}

// packedObject is one parsed pack entry with its oid already computed.
type packedObject struct {
	oid string
	obj *GitObject
}

// readPack parses a packfile into objects (deltas applied) with each
// object's oid computed once, while parsing, so callers can store objects
// with PutObjectPrecomputed instead of hashing them a second time. It
// returns the objects in pack order.
func readPack(pack []byte) ([]packedObject, error) {
	if len(pack) < 12 || string(pack[:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	pos := 12
	entries := make([]*packEntry, 0, count)
	raw := make([][]byte, 0, count) // raw deflated bytes per entry, for OFS_DELTA
	for i := 0; i < count; i++ {
		if pos >= len(pack) {
			return nil, io.ErrUnexpectedEOF
		}
		start := pos
		typ, size, n, err := readPackEntryHeader(pack[pos:])
		if err != nil {
			return nil, err
		}
		pos += n
		switch typ {
		case packCommit, packTree, packBlob, packTag:
			data, used, err := inflateAt(pack[pos:])
			if err != nil {
				return nil, err
			}
			pos += used
			// The inflated stream is the raw object content; the declared
			// size is the content length.
			if len(data) != size {
				return nil, fmt.Errorf("pack entry inflated to %d bytes, header said %d", len(data), size)
			}
			entries = append(entries, &packEntry{typ: typeName(typ), data: data, baseOf: -1})
			raw = append(raw, pack[start:pos])
		case 6: // OFS_DELTA
			offset, m, err := readOffsetEncoding(pack[pos:])
			if err != nil {
				return nil, err
			}
			pos += m
			data, used, err := inflateAt(pack[pos:])
			if err != nil {
				return nil, err
			}
			pos += used
			entries = append(entries, &packEntry{typ: "", data: data, baseOf: start - offset})
			raw = append(raw, pack[start:pos])
		case 7: // REF_DELTA
			if pos+20 > len(pack) {
				return nil, io.ErrUnexpectedEOF
			}
			base := hex.EncodeToString(pack[pos : pos+20])
			pos += 20
			data, used, err := inflateAt(pack[pos:])
			if err != nil {
				return nil, err
			}
			pos += used
			entries = append(entries, &packEntry{typ: "", data: data, base: base, baseOf: -1})
			raw = append(raw, pack[start:pos])
		default:
			return nil, fmt.Errorf("unknown pack entry type %d", typ)
		}
	}
	// Resolve deltas. Entries are resolved in pack order; a delta's base may
	// itself be a delta, so resolve recursively with memoisation.
	resolved := make([]packedObject, len(entries))
	var resolve func(i int) (packedObject, error)
	resolve = func(i int) (packedObject, error) {
		if resolved[i].obj != nil {
			return resolved[i], nil
		}
		entry := entries[i]
		if entry.typ != "" {
			po := packedObject{oid: fastGitOID(entry.typ, entry.data), obj: &GitObject{Type: entry.typ, Data: entry.data}}
			resolved[i] = po
			return po, nil
		}
		var base *GitObject
		var err error
		if entry.base != "" {
			// REF_DELTA: find the base by oid among the already-known entries.
			base, err = findPackBase(entries, resolved, entry.base)
			if err != nil {
				return packedObject{}, err
			}
		} else {
			// OFS_DELTA: find the entry starting at baseOf.
			baseIndex := -1
			off := 12
			for j := 0; j < len(entries); j++ {
				if off == entry.baseOf {
					baseIndex = j
					break
				}
				off += len(raw[j])
			}
			if baseIndex < 0 {
				return packedObject{}, fmt.Errorf("ofs-delta base at offset %d not found", entry.baseOf)
			}
			basePO, err := resolve(baseIndex)
			if err != nil {
				return packedObject{}, err
			}
			base = basePO.obj
		}
		data, err := applyDelta(base.Data, entry.data)
		if err != nil {
			return packedObject{}, err
		}
		po := packedObject{oid: fastGitOID(base.Type, data), obj: &GitObject{Type: base.Type, Data: data}}
		resolved[i] = po
		return po, nil
	}
	objects := make([]packedObject, 0, len(entries))
	for i := range entries {
		obj, err := resolve(i)
		if err != nil {
			return nil, err
		}
		objects = append(objects, obj)
	}
	return objects, nil
}

func typeName(typ int) string {
	switch typ {
	case packCommit:
		return "commit"
	case packTree:
		return "tree"
	case packBlob:
		return "blob"
	case packTag:
		return "tag"
	}
	return ""
}

// findPackBase locates a REF_DELTA base by oid (already resolved entries first,
// then by hashing unresolved plain entries).
func findPackBase(entries []*packEntry, resolved []packedObject, oid string) (*GitObject, error) {
	for _, po := range resolved {
		if po.obj == nil {
			continue
		}
		if po.oid == oid {
			return po.obj, nil
		}
	}
	for _, entry := range entries {
		if entry.typ == "" {
			continue
		}
		if GitOID(entry.typ, entry.data) == oid {
			return &GitObject{Type: entry.typ, Data: entry.data}, nil
		}
	}
	return nil, fmt.Errorf("ref-delta base %s not in pack", oid)
}

// readPackEntryHeader reads a pack entry's varint type+size header.
func readPackEntryHeader(data []byte) (typ int, size int, used int, err error) {
	if len(data) == 0 {
		return 0, 0, 0, io.ErrUnexpectedEOF
	}
	b := data[0]
	typ = int(b>>4) & 0x7
	size = int(b & 0x0f)
	shift := 4
	used = 1
	for b&0x80 != 0 {
		if used >= len(data) {
			return 0, 0, 0, io.ErrUnexpectedEOF
		}
		b = data[used]
		used++
		size |= int(b&0x7f) << shift
		shift += 7
	}
	return typ, size, used, nil
}

// readOffsetEncoding reads an OFS_DELTA negative offset.
func readOffsetEncoding(data []byte) (offset int, used int, err error) {
	if len(data) == 0 {
		return 0, 0, io.ErrUnexpectedEOF
	}
	b := data[0]
	offset = int(b & 0x7f)
	used = 1
	for b&0x80 != 0 {
		if used >= len(data) {
			return 0, 0, io.ErrUnexpectedEOF
		}
		b = data[used]
		used++
		offset = ((offset + 1) << 7) | int(b&0x7f)
	}
	return offset, used, nil
}

// inflateAt inflates one zlib stream starting at data[0] and reports how many
// input bytes it consumed. A bytes.Reader implements flate's Reader interface,
// so the decompressor reads byte-by-byte with no over-read, and the reader's
// position after the stream is the exact consumed length. The zlib reader and
// the intermediate buffer are pooled; the returned slice is freshly allocated
// at exactly the inflated size.
func inflateAt(data []byte) ([]byte, int, error) {
	br := bytes.NewReader(data)
	r, err := pooledZlibReader(br)
	if err != nil {
		return nil, 0, err
	}
	buf := inflateBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	_, rerr := buf.ReadFrom(r)
	used, _ := br.Seek(0, io.SeekCurrent)
	releaseZlibReader(r)
	if rerr != nil {
		inflateBufPool.Put(buf)
		return nil, 0, rerr
	}
	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	inflateBufPool.Put(buf)
	return out, int(used), nil
}

// applyDelta applies a git delta to base data.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int, error) {
		value := 0
		shift := 0
		for {
			if pos >= len(delta) {
				return 0, io.ErrUnexpectedEOF
			}
			b := delta[pos]
			pos++
			value |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return value, nil
			}
		}
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, fmt.Errorf("delta base size %d, have %d", srcSize, len(base))
	}
	dstSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Grow(dstSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			// Copy from base: offset and size encoded in the following bytes.
			offset, size := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, io.ErrUnexpectedEOF
					}
					offset |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, io.ErrUnexpectedEOF
					}
					size |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if offset+size > len(base) {
				return nil, fmt.Errorf("delta copy out of range (%d+%d > %d)", offset, size, len(base))
			}
			out.Write(base[offset : offset+size])
		} else if op != 0 {
			// Insert literal bytes.
			if pos+int(op) > len(delta) {
				return nil, io.ErrUnexpectedEOF
			}
			out.Write(delta[pos : pos+int(op)])
			pos += int(op)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if out.Len() != dstSize {
		return nil, fmt.Errorf("delta result %d bytes, header said %d", out.Len(), dstSize)
	}
	return out.Bytes(), nil
}

// ---------------------------------------------------------------- trees and commits

// TreeEntry is one entry of a git tree.
type TreeEntry struct {
	Mode string // "100644", "100755", "120000", "40000"
	Name string
	OID  string // hex oid
}

// parseTree parses tree object content into entries.
func parseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	pos := 0
	for pos < len(data) {
		space := bytes.IndexByte(data[pos:], ' ')
		if space < 0 {
			return nil, errors.New("bad tree entry (no space)")
		}
		mode := string(data[pos : pos+space])
		pos += space + 1
		nul := bytes.IndexByte(data[pos:], 0)
		if nul < 0 || pos+nul+20 > len(data) {
			return nil, errors.New("bad tree entry (no NUL or short oid)")
		}
		name := string(data[pos : pos+nul])
		pos += nul + 1
		oid := hex.EncodeToString(data[pos : pos+20])
		pos += 20
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
	}
	return entries, nil
}

// buildTree serialises tree entries. Entries are sorted the way git sorts
// them: by name, with directories ordered as if their name had a trailing "/".
func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i].Name, sorted[j].Name
		if sorted[i].Mode == "40000" {
			a += "/"
		}
		if sorted[j].Mode == "40000" {
			b += "/"
		}
		return a < b
	})
	var buf bytes.Buffer
	for _, entry := range sorted {
		oid, err := hex.DecodeString(entry.OID)
		if err != nil || len(oid) != 20 {
			continue
		}
		buf.WriteString(entry.Mode)
		buf.WriteByte(' ')
		buf.WriteString(entry.Name)
		buf.WriteByte(0)
		buf.Write(oid)
	}
	return buf.Bytes()
}

// Commit is a parsed commit object.
type Commit struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// parseCommit parses commit object content.
func parseCommit(data []byte) (*Commit, error) {
	commit := &Commit{}
	text := string(data)
	header, message, found := cutString(text, "\n\n")
	if !found {
		header = text
		message = ""
	}
	commit.Message = message
	for _, line := range splitLines(header) {
		if line == "" {
			continue
		}
		key, value, ok := cutString(line, " ")
		if !ok {
			continue
		}
		switch key {
		case "tree":
			commit.Tree = value
		case "parent":
			commit.Parents = append(commit.Parents, value)
		case "author":
			commit.Author = value
		case "committer":
			commit.Committer = value
		}
	}
	if commit.Tree == "" {
		return nil, errors.New("commit without a tree")
	}
	return commit, nil
}

// buildCommit serialises a commit. Author/committer are full ident lines
// ("Name <email> epoch tz"); when empty the conformance identity is used.
func buildCommit(tree string, parents []string, author, committer, message string) []byte {
	var buf bytes.Buffer
	buf.WriteString("tree " + tree + "\n")
	for _, parent := range parents {
		buf.WriteString("parent " + parent + "\n")
	}
	buf.WriteString("author " + author + "\n")
	buf.WriteString("committer " + committer + "\n")
	buf.WriteString("\n")
	buf.WriteString(message)
	return buf.Bytes()
}

// ConformanceIdentity is the author/committer ident the suite's client uses
// for the commits it builds (from git-client.ts CONFORMANCE_IDENTITY).
const ConformanceIdentity = "Forge Test <forge-test@example.invalid> 1700000000 +0000"

func cutString(s, sep string) (before, after string, found bool) {
	i := bytes.Index([]byte(s), []byte(sep))
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
