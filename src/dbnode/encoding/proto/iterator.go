// Copyright (c) 2019 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	dpb "github.com/golang/protobuf/protoc-gen-go/descriptor"
	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/dynamic"
	"github.com/m3db/m3/src/dbnode/encoding"
	"github.com/m3db/m3/src/dbnode/encoding/m3tsz"
)

var (
	errIteratorReaderIsRequired = errors.New("proto iterator: reader is required")
	errIteratorSchemaIsRequired = errors.New("proto iterator: schema is required")
)

// TODO: Need to add support for the iterator detecting the end of the stream.
type iterator struct {
	err                  error
	schema               *desc.MessageDescriptor
	stream               encoding.IStream
	consumedFirstMessage bool
	lastIterated         *dynamic.Message
	tszFields            []tszFieldState

	// Fields that are reused between function calls to
	// avoid allocations.
	varIntBuf    [8]byte
	bitsetValues []int
}

// NewIterator creates a new iterator.
func NewIterator(
	reader io.Reader,
	schema *desc.MessageDescriptor,
) (*iterator, error) {
	if reader == nil {
		return nil, errIteratorReaderIsRequired
	}
	if schema == nil {
		return nil, errIteratorSchemaIsRequired
	}

	iter := &iterator{
		schema: schema,
		stream: encoding.NewIStream(reader),
		// TODO: These need to be possibly updated as we traverse a stream
		tszFields: tszFields(nil, schema),
	}

	return iter, nil
}

func (it *iterator) Next() bool {
	if !it.hasNext() {
		return false
	}

	if err := it.readTSZValues(); err != nil {
		it.err = err
		return false
	}

	if err := it.readProtoValues(); err != nil {
		it.err = err
		return false
	}

	it.consumedFirstMessage = true
	return it.hasNext()
}

func (it *iterator) Current() *dynamic.Message {
	return it.lastIterated
}

func (it *iterator) Err() error {
	return it.err
}

func (it *iterator) readTSZValues() error {
	var err error

	if !it.consumedFirstMessage {
		err = it.readFirstTSZValues()
	} else {
		err = it.readNextTSZValues()
	}

	return err
}

func (it *iterator) readProtoValues() error {
	bit, err := it.stream.ReadBit()
	if err != nil {
		return err
	}

	if bit == 0 {
		// No changes since previous message.
		return nil
	}

	err = it.readBitset()
	if err != nil {
		return fmt.Errorf(
			"error readining changed proto field numbers bitset: %v", err)
	}

	marshalLen, err := it.readVarInt()
	if err != nil {
		return err
	}

	// TODO(rartoul): Probably want to use a bytes pool for this.
	buf := make([]byte, 0, marshalLen)
	for i := uint64(0); i < marshalLen; i++ {
		b, err := it.stream.ReadByte()
		if err != nil {
			return fmt.Errorf("error reading marshaled proto bytes: %v", err)
		}
		buf = append(buf, b)
	}

	if it.lastIterated == nil {
		it.lastIterated = dynamic.NewMessage(it.schema)
	}

	currMessage := dynamic.NewMessage(it.schema)
	err = currMessage.Unmarshal(buf)
	if err != nil {
		return fmt.Errorf("error unmarshaling protobuf: %v", err)
	}

	if !it.consumedFirstMessage {
		// If this is the first message in the stream then the list of changed
		// fields will be empty (even though some of the fields may have data)
		// so we need to do a full merge and ignore the empty list of changed
		// fields.
		err := it.lastIterated.MergeFrom(currMessage)
		if err != nil {
			return fmt.Errorf(
				"error merging current message into previous, err: %v, message: %s",
				err, currMessage.String())
		}
	}

	for _, fieldNum := range it.bitsetValues {
		it.lastIterated.SetFieldByNumber(fieldNum, currMessage.GetFieldByNumber(fieldNum))
	}

	return nil
}

func (it *iterator) readFirstTSZValues() error {
	for i := range it.tszFields {
		fb, xor, err := it.readFullFloatVal()
		if err != nil {
			return err
		}

		it.tszFields[i].prevFloatBits = fb
		it.tszFields[i].prevXOR = xor
		if err := it.updateLastIteratedWithTSZValues(i); err != nil {
			return err
		}
	}

	return nil
}

func (it *iterator) readNextTSZValues() error {
	for i := range it.tszFields {
		fb, xor, err := it.readFloatXOR(i)
		if err != nil {
			return err
		}

		it.tszFields[i].prevFloatBits = fb
		it.tszFields[i].prevXOR = xor
		if err := it.updateLastIteratedWithTSZValues(i); err != nil {
			return err
		}
	}

	return nil
}

// updateLastIteratedWithTSZValues updates lastIterated with the current
// value of the TSZ field in it.tszFields at index i. This ensures that
// when we return it.lastIterated in the call to Current() that all the
// most recent values are present.
func (it *iterator) updateLastIteratedWithTSZValues(i int) error {
	if it.lastIterated == nil {
		it.lastIterated = dynamic.NewMessage(it.schema)
	}

	var (
		fieldNum = it.tszFields[i].fieldNum
		val      = math.Float64frombits(it.tszFields[i].prevFloatBits)
		err      error
	)
	if it.schema.FindFieldByNumber(int32(fieldNum)).GetType() == dpb.FieldDescriptorProto_TYPE_DOUBLE {
		err = it.lastIterated.TrySetFieldByNumber(fieldNum, val)
	} else {
		err = it.lastIterated.TrySetFieldByNumber(fieldNum, float32(val))
	}
	return err
}

func (it *iterator) readFloatXOR(i int) (floatBits, xor uint64, err error) {
	xor, err = it.readXOR(i)
	if err != nil {
		return 0, 0, err
	}
	prevFloatBits := it.tszFields[i].prevFloatBits
	return prevFloatBits ^ xor, xor, nil
}

func (it *iterator) readXOR(i int) (uint64, error) {
	cb, err := it.readBits(1)
	if err != nil {
		return 0, err
	}
	if cb == m3tsz.OpcodeZeroValueXOR {
		return 0, nil
	}

	cb2, err := it.readBits(1)
	if err != nil {
		return 0, err
	}

	cb = (cb << 1) | cb2
	if cb == m3tsz.OpcodeContainedValueXOR {
		var (
			previousXOR                       = it.tszFields[i].prevXOR
			previousLeading, previousTrailing = encoding.LeadingAndTrailingZeros(previousXOR)
			numMeaningfulBits                 = 64 - previousLeading - previousTrailing
		)
		meaningfulBits, err := it.readBits(numMeaningfulBits)
		if err != nil {
			return 0, err
		}

		return meaningfulBits << uint(previousTrailing), nil
	}

	numLeadingZerosBits, err := it.readBits(6)
	if err != nil {
		return 0, err
	}
	numMeaningfulBitsBits, err := it.readBits(6)
	if err != nil {
		return 0, err
	}

	var (
		numLeadingZeros   = int(numLeadingZerosBits)
		numMeaningfulBits = int(numMeaningfulBitsBits) + 1
		numTrailingZeros  = 64 - numLeadingZeros - numMeaningfulBits
	)
	meaningfulBits, err := it.readBits(numMeaningfulBits)
	if err != nil {
		return 0, err
	}

	return meaningfulBits << uint(numTrailingZeros), nil
}

func (it *iterator) readFullFloatVal() (floatBits uint64, xor uint64, err error) {
	floatBits, err = it.readBits(64)
	if err != nil {
		return 0, 0, err
	}

	return floatBits, floatBits, nil
}

// readBitset does the inverse of encodeBitset on the encoder struct.
func (it *iterator) readBitset() error {
	it.bitsetValues = it.bitsetValues[:0]
	bitsetLengthBits, err := it.readVarInt()
	if err != nil {
		return err
	}

	for i := uint64(0); i < bitsetLengthBits; i++ {
		bit, err := it.stream.ReadBit()
		if err != nil {
			return fmt.Errorf("error reading bitset: %v", err)
		}

		if bit == 1 {
			// Add 1 because protobuf fields are 1-indexed not 0-indexed.
			it.bitsetValues = append(it.bitsetValues, int(i)+1)
		}
	}

	return nil
}

func (it *iterator) readVarInt() (uint64, error) {
	var (
		// Convert array to slice and reset size to zero so
		// we can reuse the buffer.
		buf      = it.varIntBuf[:0]
		numBytes = 0
	)
	for {
		b, err := it.stream.ReadByte()
		if err != nil {
			return 0, fmt.Errorf("error reading var int: %v", err)
		}

		buf = append(buf, b)
		numBytes++

		if b>>7 == 0 {
			break
		}
	}

	buf = buf[:numBytes]
	varInt, _ := binary.Uvarint(buf)
	return varInt, nil
}

func (it *iterator) readBits(numBits int) (uint64, error) {
	res, err := it.stream.ReadBits(numBits)
	if err != nil {
		return 0, err
	}

	return res, nil
}

func (it *iterator) hasNext() bool {
	// TODO(rartoul): Do I care about closed? Maybe for cleanup
	return !it.hasError() && !it.isDone() && !it.isClosed()
}

func (it *iterator) hasError() bool {
	return it.err != nil
}

func (i *iterator) isDone() bool {
	// TODO: Fix me
	return false
}

func (i *iterator) isClosed() bool {
	// TODO: Fix me
	return false
}
