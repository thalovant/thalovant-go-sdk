package thalovant

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"io"
)

var hiveTypeToInt = map[string]byte{
	"shake":      0,
	"handshake":  0,
	"bus":        1,
	"shared_bus": 2,
	"broadcast":  3,
	"propagate":  4,
	"escalate":   5,
	"hello":      6,
	"query":      7,
	"cascade":    8,
	"ping":       9,
	"rendezvous": 10,
	"3rdparty":   11,
	"bin":        12,
}

var hiveIntToType = map[byte]string{
	0:  "shake",
	1:  "bus",
	2:  "shared_bus",
	3:  "broadcast",
	4:  "propagate",
	5:  "escalate",
	6:  "hello",
	7:  "query",
	8:  "cascade",
	9:  "ping",
	10: "rendezvous",
	11: "3rdparty",
	12: "bin",
}

func EncodeHiveBinaryFrame(message HiveMessage) ([]byte, error) {
	typeID, ok := hiveTypeToInt[message.MsgType]
	if !ok {
		typeID = 11
	}
	metadata, err := json.Marshal(nonNilMap(message.Metadata))
	if err != nil {
		return nil, err
	}
	if len(metadata) > 255 {
		return nil, fmt.Errorf("HiveMind binary metadata cannot exceed 255 bytes")
	}
	payload, err := json.Marshal(nonNilMap(message.Payload))
	if err != nil {
		return nil, err
	}
	// Keep capacity arithmetic bounded by the protocol-limited metadata size.
	// append grows safely for the caller-controlled payload length.
	out := make([]byte, 0, 2+len(metadata))
	out = append(out, 0x80|((typeID&0x1f)<<1), byte(len(metadata)))
	out = append(out, metadata...)
	out = append(out, payload...)
	return out, nil
}

func DecodeHiveBinaryFrame(payload []byte) (HiveMessage, error) {
	reader := newBitReader(payload)
	if err := reader.skipLeftPadding(); err != nil {
		return HiveMessage{}, err
	}
	versioned, err := reader.readBit()
	if err != nil {
		return HiveMessage{}, err
	}
	if versioned == 1 {
		version, err := reader.readUint(8)
		if err != nil {
			return HiveMessage{}, err
		}
		if version > 1 {
			return HiveMessage{}, fmt.Errorf("unsupported HiveMind binary protocol version: %d", version)
		}
	}
	typeID, err := reader.readUint(5)
	if err != nil {
		return HiveMessage{}, err
	}
	compressed, err := reader.readBit()
	if err != nil {
		return HiveMessage{}, err
	}
	metaLen, err := reader.readUint(8)
	if err != nil {
		return HiveMessage{}, err
	}
	metaBytes, err := reader.readBytes(int(metaLen))
	if err != nil {
		return HiveMessage{}, err
	}
	metaText, err := decodeWireText(metaBytes, compressed == 1)
	if err != nil {
		return HiveMessage{}, err
	}
	payloadBytes, err := reader.readRemainingBytes()
	if err != nil {
		return HiveMessage{}, err
	}
	payloadText, err := decodeWireText(payloadBytes, compressed == 1)
	if err != nil {
		return HiveMessage{}, err
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(metaText), &metadata); err != nil {
		return HiveMessage{}, err
	}
	var messagePayload map[string]any
	if err := json.Unmarshal([]byte(payloadText), &messagePayload); err != nil {
		return HiveMessage{}, err
	}
	msgType := hiveIntToType[byte(typeID)]
	if msgType == "" {
		msgType = "3rdparty"
	}
	return HiveMessage{
		MsgType:      msgType,
		Payload:      messagePayload,
		Metadata:     metadata,
		Route:        []any{},
		Node:         nil,
		TargetSiteID: nil,
		TargetPubKey: nil,
		SourcePeer:   nil,
	}, nil
}

func decodeWireText(payload []byte, compressed bool) (string, error) {
	if !compressed {
		return string(payload), nil
	}
	reader, err := zlib.NewReader(bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	defer reader.Close()
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

func nonNilMap(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

type bitReader struct {
	payload []byte
	offset  int
}

func newBitReader(payload []byte) *bitReader {
	return &bitReader{payload: payload}
}

func (r *bitReader) skipLeftPadding() error {
	for {
		bit, err := r.readBit()
		if err != nil {
			return err
		}
		if bit == 1 {
			return nil
		}
	}
}

func (r *bitReader) readBit() (int, error) {
	if r.offset >= len(r.payload)*8 {
		return 0, fmt.Errorf("unexpected end of HiveMind binary frame")
	}
	value := int((r.payload[r.offset/8] >> (7 - (r.offset % 8))) & 1)
	r.offset++
	return value, nil
}

func (r *bitReader) readUint(width int) (int, error) {
	value := 0
	for i := 0; i < width; i++ {
		bit, err := r.readBit()
		if err != nil {
			return 0, err
		}
		value = (value << 1) | bit
	}
	return value, nil
}

func (r *bitReader) readBytes(length int) ([]byte, error) {
	out := make([]byte, length)
	for i := range out {
		value, err := r.readUint(8)
		if err != nil {
			return nil, err
		}
		out[i] = byte(value)
	}
	return out, nil
}

func (r *bitReader) readRemainingBytes() ([]byte, error) {
	bits := len(r.payload)*8 - r.offset
	return r.readBytes(bits / 8)
}
