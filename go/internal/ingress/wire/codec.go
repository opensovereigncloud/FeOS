// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"google.golang.org/grpc/encoding"
)

const (
	// CodecName is the codec's name, the content subtype of every call.
	CodecName = "json"
	// ContentType is the content type a call of IngressService carries.
	ContentType = "application/grpc+" + CodecName
)

// Codec codes the messages of IngressService as JSON. Both ends force it,
// the server with grpc.ForceServerCodec(Codec{}), the client with
// grpc.ForceCodec(Codec{}) on each call (Client does that). It is not
// registered with grpc's codec registry: nothing else on the nodes' port
// may answer in JSON.
//
// Marshal does not escape HTML: '<', '>' and '&' take one byte each, not
// six, so a route's path takes on the wire what it takes in the spec.
// Unmarshal ignores unknown fields and reads empty data as the zero
// message, as protobuf does.
type Codec struct{}

var _ encoding.Codec = Codec{}

// Name is CodecName.
func (Codec) Name() string { return CodecName }

// Marshal codes v as JSON, without escaping HTML.
func (Codec) Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := newEncoder(&buf).Encode(v); err != nil {
		return nil, fmt.Errorf("wire: marshal %T: %w", v, err)
	}
	// Encode ends the value with a newline.
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Size returns the bytes v takes coded by Codec, without keeping them.
func Size(v any) (int, error) {
	var n counter
	if err := newEncoder(&n).Encode(v); err != nil {
		return 0, fmt.Errorf("wire: size of %T: %w", v, err)
	}
	return int(n) - 1, nil // without Encode's newline
}

// newEncoder returns the encoder of Codec on w: JSON without escaping
// HTML.
func newEncoder(w io.Writer) *json.Encoder {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc
}

// counter counts the bytes written to it.
type counter int

func (c *counter) Write(p []byte) (int, error) {
	*c += counter(len(p))
	return len(p), nil
}

// Unmarshal decodes JSON into v.
func (Codec) Unmarshal(data []byte, v any) error {
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("wire: unmarshal %T: %w", v, err)
	}
	return nil
}
