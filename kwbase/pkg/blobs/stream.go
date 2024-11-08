// Copyright 2019 The Cockroach Authors.
// Copyright (c) 2022-present, Shanghai Yunxi Technology Co, Ltd. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// This software (KWDB) is licensed under Mulan PSL v2.
// You can use this software according to the terms and conditions of the Mulan PSL v2.
// You may obtain a copy of Mulan PSL v2 at:
//          http://license.coscl.org.cn/MulanPSL2
// THIS SOFTWARE IS PROVIDED ON AN "AS IS" BASIS, WITHOUT WARRANTIES OF ANY KIND,
// EITHER EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO NON-INFRINGEMENT,
// MERCHANTABILITY OR FIT FOR A PARTICULAR PURPOSE.
// See the Mulan PSL v2 for more details.

package blobs

import (
	"io"

	"gitee.com/kwbasedb/kwbase/pkg/blobs/blobspb"
)

// Within the blob service, streaming is used in two functions:
//   - GetStream, streaming from server to client
//   - PutStream, streaming from client to server
// These functions are used to read or write files on a remote node.
// The io.ReadCloser we implement here are used on the _receiver's_
// side, to read from either Blob_GetStreamClient or Blob_PutStreamServer.
// The function streamContent() is used on the _sender's_ side to split
// the content and send it using Blob_GetStreamServer or Blob_PutStreamClient.

// chunkSize was decided to be 128K after running an experiment benchmarking
// ReadFile and WriteFile. It seems like the benefits of streaming do not appear
// until files of 1 MB or larger, and for those files, 128K chunks are optimal.
// For ReadFile, larger chunks are more efficient but the gains are not as significant
// past 128K. For WriteFile, 128K chunks perform best, and past that, performance
// starts decreasing.
var chunkSize = 128 * 1 << 10

// blobStreamReader implements a ReadCloser which receives
// gRPC streaming messages.
var _ io.ReadCloser = &blobStreamReader{}

type streamReceiver interface {
	SendAndClose(*blobspb.StreamResponse) error
	Recv() (*blobspb.StreamChunk, error)
}

// nopSendAndClose creates a GetStreamClient that has a nop SendAndClose function.
// This is needed as Blob_GetStreamClient does not have a Close() function, whereas
// the other sender, Blob_PutStreamServer, does.
type nopSendAndClose struct {
	blobspb.Blob_GetStreamClient
}

func (*nopSendAndClose) SendAndClose(*blobspb.StreamResponse) error {
	return nil
}

// newGetStreamReader creates an io.ReadCloser that uses gRPC's streaming API
// to read chunks of data.
func newGetStreamReader(client blobspb.Blob_GetStreamClient) io.ReadCloser {
	return &blobStreamReader{
		stream: &nopSendAndClose{client},
	}
}

// newPutStreamReader creates an io.ReadCloser that uses gRPC's streaming API
// to read chunks of data.
func newPutStreamReader(client blobspb.Blob_PutStreamServer) io.ReadCloser {
	return &blobStreamReader{stream: client}
}

type blobStreamReader struct {
	lastPayload []byte
	lastOffset  int
	stream      streamReceiver
	EOFReached  bool
}

func (r *blobStreamReader) Read(out []byte) (int, error) {
	if r.EOFReached {
		return 0, io.EOF
	}

	offset := 0
	// Use the last payload.
	if r.lastPayload != nil {
		offset = len(r.lastPayload) - r.lastOffset
		if len(out) < offset {
			copy(out, r.lastPayload[r.lastOffset:])
			r.lastOffset += len(out)
			return len(out), nil
		}
		copy(out[:offset], r.lastPayload[r.lastOffset:])
		r.lastPayload = nil
	}
	for offset < len(out) {
		chunk, err := r.stream.Recv()
		if err == io.EOF {
			r.EOFReached = true
			break
		}
		if err != nil {
			return offset, err
		}
		var lenToWrite int
		if len(out)-offset >= len(chunk.Payload) {
			lenToWrite = len(chunk.Payload)
		} else {
			lenToWrite = len(out) - offset
			// Need to cache payload.
			r.lastPayload = chunk.Payload
			r.lastOffset = lenToWrite
		}
		copy(out[offset:offset+lenToWrite], chunk.Payload[:lenToWrite])
		offset += lenToWrite
	}
	return offset, nil
}

func (r *blobStreamReader) Close() error {
	return r.stream.SendAndClose(&blobspb.StreamResponse{})
}

type streamSender interface {
	Send(*blobspb.StreamChunk) error
}

// streamContent splits the content into chunks, of size `chunkSize`,
// and streams those chunks to sender.
// Note: This does not close the stream.
func streamContent(sender streamSender, content io.Reader) error {
	payload := make([]byte, chunkSize)
	var chunk blobspb.StreamChunk
	for {
		n, err := content.Read(payload)
		if n > 0 {
			chunk.Payload = payload[:n]
			err = sender.Send(&chunk)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
