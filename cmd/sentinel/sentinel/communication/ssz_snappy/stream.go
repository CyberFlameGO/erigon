/*
   Copyright 2022 Erigon-Lightclient contributors
   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at
       http://www.apache.org/licenses/LICENSE-2.0
   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package ssz_snappy

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	ssz "github.com/ferranbt/fastssz"
	"github.com/golang/snappy"
	"github.com/ledgerwatch/erigon/cl/cltypes"
	"github.com/ledgerwatch/erigon/cmd/sentinel/sentinel/communication"
	"github.com/libp2p/go-libp2p-core/network"
)

type StreamCodec struct {
	s  network.Stream
	sr *snappy.Reader
}

func NewStreamCodec(
	s network.Stream,
) communication.StreamCodec {
	return &StreamCodec{
		s:  s,
		sr: snappy.NewReader(s),
	}
}

func (d *StreamCodec) Close() error {
	if err := d.s.Close(); err != nil {
		return err
	}
	return nil
}

func (d *StreamCodec) CloseWriter() error {
	if err := d.s.CloseWrite(); err != nil {
		return err
	}
	return nil
}

func (d *StreamCodec) CloseReader() error {
	if err := d.s.CloseRead(); err != nil {
		return err
	}
	return nil
}

// write packet to stream. will add correct header + compression
// will error if packet does not implement ssz.Marshaler interface
func (d *StreamCodec) WritePacket(pkt communication.Packet, prefix ...byte) (err error) {
	val, ok := pkt.(ssz.Marshaler)
	if !ok {
		return nil
	}
	if len(prefix) > 0 {
		return EncodeAndWrite(d.s, val, prefix[0])
	}
	return EncodeAndWrite(d.s, val)
}

// write raw bytes to stream
func (d *StreamCodec) Write(payload []byte) (n int, err error) {
	return d.s.Write(payload)
}

// read raw bytes to stream
func (d *StreamCodec) Read(b []byte) (n int, err error) {
	return d.s.Read(b)
}

// read raw bytes to stream
func (d *StreamCodec) ReadByte() (b byte, err error) {
	o := [1]byte{}
	_, err = io.ReadFull(d.s, o[:])
	if err != nil {
		return
	}
	return o[0], nil
}

// decode into packet p, then return the packet context
func (d *StreamCodec) Decode(p communication.Packet) (ctx *communication.StreamContext, err error) {
	ctx, err = d.readPacket(p)
	return
}

func (d *StreamCodec) readPacket(p communication.Packet) (ctx *communication.StreamContext, err error) {
	c := &communication.StreamContext{
		Packet:   p,
		Stream:   d.s,
		Codec:    d,
		Protocol: d.s.Protocol(),
	}
	if val, ok := p.(ssz.Unmarshaler); ok {
		ln, _, err := readUvarint(d.s)
		if err != nil {
			return c, err
		}
		c.Raw = make([]byte, ln)
		_, err = io.ReadFull(d.sr, c.Raw)
		if err != nil {
			return c, fmt.Errorf("readPacket: %w", err)
		}
		err = val.UnmarshalSSZ(c.Raw)
		if err != nil {
			return c, fmt.Errorf("readPacket: %w", err)
		}
	}
	return c, nil
}

func EncodeAndWrite(w io.Writer, val ssz.Marshaler, prefix ...byte) error {
	// create prefix for length of packet
	lengthBuf := make([]byte, 10)
	vin := binary.PutUvarint(lengthBuf, uint64(val.SizeSSZ()))
	// Create writer size
	wr := bufio.NewWriterSize(w, 10+val.SizeSSZ())
	defer wr.Flush()
	// Write length of packet
	wr.Write(prefix)
	wr.Write(lengthBuf[:vin])
	// start using streamed snappy compression
	sw := snappy.NewBufferedWriter(wr)
	defer sw.Flush()
	// Marshall and snap it
	xs := make([]byte, 0, val.SizeSSZ())
	enc, err := val.MarshalSSZTo(xs)
	if err != nil {
		return err
	}
	_, err = sw.Write(enc)
	return err
}

func DecodeAndRead(r io.Reader, val cltypes.ObjectSSZ) error {
	forkDigest := make([]byte, 4)
	// TODO(issues/5884): assert the fork digest matches the expectation for
	// a specific configuration.
	if _, err := r.Read(forkDigest); err != nil {
		return err
	}
	return DecodeAndReadNoForkDigest(r, val)
}

func DecodeAndReadNoForkDigest(r io.Reader, val cltypes.ObjectSSZ) error {
	// Read varint for length of message.
	encodedLn, _, err := readUvarint(r)
	if err != nil {
		return fmt.Errorf("unable to read varint from message prefix: %v", err)
	}
	expectedLn := val.SizeSSZ()
	if encodedLn != uint64(expectedLn) {
		return fmt.Errorf("encoded length not equal to expected size: want %d, got %d", expectedLn, encodedLn)
	}

	sr := snappy.NewReader(r)
	raw := make([]byte, expectedLn)
	if _, err := io.ReadFull(sr, raw); err != nil {
		return fmt.Errorf("unable to readPacket: %w", err)
	}

	err = val.UnmarshalSSZ(raw)
	if err != nil {
		return fmt.Errorf("enable to unmarshall message: %v", err)
	}
	return nil
}

func readUvarint(r io.Reader) (x, n uint64, err error) {
	currByte := make([]byte, 1)
	for shift := uint(0); shift < 64; shift += 7 {
		_, err := r.Read(currByte)
		n++
		if err != nil {
			return 0, 0, err
		}
		b := uint64(currByte[0])
		x |= (b & 0x7F) << shift
		if (b & 0x80) == 0 {
			return x, n, nil
		}
	}

	// The number is too large to represent in a 64-bit value.
	return 0, n, nil
}

func DecodeListSSZBeaconBlock(data []byte, count uint64, list []cltypes.ObjectSSZ) error {
	r := bytes.NewReader(data)
	for i := 0; i < int(count); i++ {
		forkDigest := make([]byte, 4)
		// TODO(issues/5884): assert the fork digest matches the expectation for
		// a specific configuration.
		if _, err := r.Read(forkDigest); err != nil {
			return err
		}

		// Read varint for length of message.
		encodedLn, _, err := readUvarint(r)
		if err != nil {
			return fmt.Errorf("unable to read varint from message prefix: %v", err)
		}

		// Read bytes using snappy into a new raw buffer of side encodedLn.
		raw := make([]byte, encodedLn)
		sr := snappy.NewReader(r)
		bytesRead := 0
		for bytesRead < int(encodedLn) {
			n, err := sr.Read(raw[bytesRead:])
			if err != nil {
				return fmt.Errorf("Read error: %w", err)
			}
			bytesRead += n
		}

		if err := list[i].UnmarshalSSZ(raw); err != nil {
			return fmt.Errorf("unmarshalling: %w", err)
		}

		// TODO(issues/5884): figure out why there is this extra byte.
		r.ReadByte()
	}
	return nil
}

func DecodeListSSZ(data []byte, count uint64, list []cltypes.ObjectSSZ) error {
	objSize := list[0].SizeSSZ()

	r := bytes.NewReader(data)
	forkDigest := make([]byte, 4)
	// TODO(issues/5884): assert the fork digest matches the expectation for
	// a specific configuration.
	if _, err := r.Read(forkDigest); err != nil {
		return err
	}

	// Read varint for length of message.
	encodedLn, bytesCount, err := readUvarint(r)
	if err != nil {
		return fmt.Errorf("unable to read varint from message prefix: %v", err)
	}
	pos := 4 + bytesCount
	if encodedLn != uint64(objSize) || len(list) != int(count) {
		return fmt.Errorf("encoded length not equal to expected size: want %d, got %d", objSize, encodedLn)
	}

	sr := snappy.NewReader(r)
	for i := 0; i < int(count); i++ {
		var n int
		raw := make([]byte, objSize)
		if n, err = sr.Read(raw); err != nil {
			return fmt.Errorf("readPacket: %w", err)
		}
		pos += uint64(n)

		if err := list[i].UnmarshalSSZ(raw); err != nil {
			return fmt.Errorf("unmarshalling: %w", err)
		}
		r.Reset(data[pos:])
		sr.Reset(r)
	}

	return nil
}
