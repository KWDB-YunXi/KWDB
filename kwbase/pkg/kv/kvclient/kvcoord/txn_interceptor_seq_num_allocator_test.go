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

package kvcoord

import (
	"context"
	"fmt"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/storage/enginepb"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
	"github.com/stretchr/testify/require"
)

func makeMockTxnSeqNumAllocator() (txnSeqNumAllocator, *mockLockedSender) {
	mockSender := &mockLockedSender{}
	return txnSeqNumAllocator{
		wrapped: mockSender,
	}, mockSender
}

// TestSequenceNumberAllocation tests the basics of sequence number allocation.
// It verifies that read-only requests are assigned the current largest sequence
// number and that write requests are assigned a sequence number larger than any
// previously allocated.
func TestSequenceNumberAllocation(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	s, mockSender := makeMockTxnSeqNumAllocator()

	txn := makeTxnProto()
	keyA, keyB := roachpb.Key("a"), roachpb.Key("b")

	// Read-only requests are not given unique sequence numbers.
	var ba roachpb.BatchRequest
	ba.Header = roachpb.Header{Txn: &txn}
	ba.Add(&roachpb.GetRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
	ba.Add(&roachpb.ScanRequest{RequestHeader: roachpb.RequestHeader{Key: keyA, EndKey: keyB}})

	mockSender.MockSend(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		require.Len(t, ba.Requests, 2)
		require.Equal(t, enginepb.TxnSeq(0), ba.Requests[0].GetInner().Header().Sequence)
		require.Equal(t, enginepb.TxnSeq(0), ba.Requests[1].GetInner().Header().Sequence)

		br := ba.CreateReply()
		br.Txn = ba.Txn
		return br, nil
	})

	br, pErr := s.SendLocked(ctx, ba)
	require.Nil(t, pErr)
	require.NotNil(t, br)

	// Write requests each get a unique sequence number.
	ba.Requests = nil
	ba.Add(&roachpb.ConditionalPutRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
	ba.Add(&roachpb.GetRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
	ba.Add(&roachpb.InitPutRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
	ba.Add(&roachpb.ScanRequest{RequestHeader: roachpb.RequestHeader{Key: keyA, EndKey: keyB}})

	mockSender.MockSend(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		require.Len(t, ba.Requests, 4)
		require.Equal(t, enginepb.TxnSeq(1), ba.Requests[0].GetInner().Header().Sequence)
		require.Equal(t, enginepb.TxnSeq(1), ba.Requests[1].GetInner().Header().Sequence)
		require.Equal(t, enginepb.TxnSeq(2), ba.Requests[2].GetInner().Header().Sequence)
		require.Equal(t, enginepb.TxnSeq(2), ba.Requests[3].GetInner().Header().Sequence)

		br = ba.CreateReply()
		br.Txn = ba.Txn
		return br, nil
	})

	br, pErr = s.SendLocked(ctx, ba)
	require.Nil(t, pErr)
	require.NotNil(t, br)

	// EndTxn requests also get a unique sequence number.
	ba.Requests = nil
	ba.Add(&roachpb.PutRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
	ba.Add(&roachpb.ScanRequest{RequestHeader: roachpb.RequestHeader{Key: keyA, EndKey: keyB}})
	ba.Add(&roachpb.EndTxnRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})

	mockSender.MockSend(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		require.Len(t, ba.Requests, 3)
		require.Equal(t, enginepb.TxnSeq(3), ba.Requests[0].GetInner().Header().Sequence)
		require.Equal(t, enginepb.TxnSeq(3), ba.Requests[1].GetInner().Header().Sequence)
		require.Equal(t, enginepb.TxnSeq(4), ba.Requests[2].GetInner().Header().Sequence)

		br = ba.CreateReply()
		br.Txn = ba.Txn
		return br, nil
	})

	br, pErr = s.SendLocked(ctx, ba)
	require.Nil(t, pErr)
	require.NotNil(t, br)
}

// TestSequenceNumberAllocationWithStep tests the basics of sequence number allocation.
// It verifies that read-only requests are assigned the last step sequence number
// and that write requests are assigned a sequence number larger than any
// previously allocated.
func TestSequenceNumberAllocationWithStep(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	s, mockSender := makeMockTxnSeqNumAllocator()

	txn := makeTxnProto()
	keyA, keyB := roachpb.Key("a"), roachpb.Key("b")

	s.configureSteppingLocked(true /* enabled */)

	for i := 1; i <= 3; i++ {
		if err := s.stepLocked(ctx); err != nil {
			t.Fatal(err)
		}
		if s.writeSeq != s.readSeq {
			t.Fatalf("mismatched read seqnum: got %d, expected %d", s.readSeq, s.writeSeq)
		}

		t.Run(fmt.Sprintf("step %d", i), func(t *testing.T) {
			currentStepSeqNum := s.writeSeq

			var ba roachpb.BatchRequest
			ba.Header = roachpb.Header{Txn: &txn}
			ba.Add(&roachpb.GetRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
			ba.Add(&roachpb.ScanRequest{RequestHeader: roachpb.RequestHeader{Key: keyA, EndKey: keyB}})

			mockSender.MockSend(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
				require.Len(t, ba.Requests, 2)
				require.Equal(t, currentStepSeqNum, ba.Requests[0].GetInner().Header().Sequence)
				require.Equal(t, currentStepSeqNum, ba.Requests[1].GetInner().Header().Sequence)

				br := ba.CreateReply()
				br.Txn = ba.Txn
				return br, nil
			})

			br, pErr := s.SendLocked(ctx, ba)
			require.Nil(t, pErr)
			require.NotNil(t, br)

			// Write requests each get a unique sequence number. The read-only requests
			// remain at the last step seqnum.
			ba.Requests = nil
			ba.Add(&roachpb.ConditionalPutRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
			ba.Add(&roachpb.GetRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
			ba.Add(&roachpb.InitPutRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
			ba.Add(&roachpb.ScanRequest{RequestHeader: roachpb.RequestHeader{Key: keyA, EndKey: keyB}})

			mockSender.MockSend(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
				require.Len(t, ba.Requests, 4)
				require.Equal(t, currentStepSeqNum+1, ba.Requests[0].GetInner().Header().Sequence)
				require.Equal(t, currentStepSeqNum, ba.Requests[1].GetInner().Header().Sequence)
				require.Equal(t, currentStepSeqNum+2, ba.Requests[2].GetInner().Header().Sequence)
				require.Equal(t, currentStepSeqNum, ba.Requests[3].GetInner().Header().Sequence)

				br = ba.CreateReply()
				br.Txn = ba.Txn
				return br, nil
			})

			br, pErr = s.SendLocked(ctx, ba)
			require.Nil(t, pErr)
			require.NotNil(t, br)

			// EndTxn requests also get a unique sequence number. Meanwhile read-only
			// requests remain at the last step.
			ba.Requests = nil
			ba.Add(&roachpb.PutRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
			ba.Add(&roachpb.ScanRequest{RequestHeader: roachpb.RequestHeader{Key: keyA, EndKey: keyB}})
			ba.Add(&roachpb.EndTxnRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})

			mockSender.MockSend(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
				require.Len(t, ba.Requests, 3)
				require.Equal(t, currentStepSeqNum+3, ba.Requests[0].GetInner().Header().Sequence)
				require.Equal(t, currentStepSeqNum, ba.Requests[1].GetInner().Header().Sequence)
				require.Equal(t, currentStepSeqNum+4, ba.Requests[2].GetInner().Header().Sequence)

				br = ba.CreateReply()
				br.Txn = ba.Txn
				return br, nil
			})

			br, pErr = s.SendLocked(ctx, ba)
			require.Nil(t, pErr)
			require.NotNil(t, br)
		})
	}

	// Check that step-wise execution is disabled by ConfigureStepping(SteppingDisabled).
	s.configureSteppingLocked(false /* enabled */)
	currentStepSeqNum := s.writeSeq

	var ba roachpb.BatchRequest
	ba.Requests = nil
	ba.Add(&roachpb.ConditionalPutRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
	ba.Add(&roachpb.GetRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
	ba.Add(&roachpb.InitPutRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
	ba.Add(&roachpb.ScanRequest{RequestHeader: roachpb.RequestHeader{Key: keyA, EndKey: keyB}})

	mockSender.MockSend(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		require.Len(t, ba.Requests, 4)
		require.Equal(t, currentStepSeqNum+1, ba.Requests[0].GetInner().Header().Sequence)
		require.Equal(t, currentStepSeqNum+1, ba.Requests[1].GetInner().Header().Sequence)
		require.Equal(t, currentStepSeqNum+2, ba.Requests[2].GetInner().Header().Sequence)
		require.Equal(t, currentStepSeqNum+2, ba.Requests[3].GetInner().Header().Sequence)

		br := ba.CreateReply()
		br.Txn = ba.Txn
		return br, nil
	})

	br, pErr := s.SendLocked(ctx, ba)
	require.Nil(t, pErr)
	require.NotNil(t, br)
}

// TestSequenceNumberAllocationTxnRequests tests sequence number allocation's
// interaction with transaction state requests (HeartbeatTxn and EndTxn). Only
// EndTxn requests should be assigned unique sequence numbers.
func TestSequenceNumberAllocationTxnRequests(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	s, mockSender := makeMockTxnSeqNumAllocator()

	txn := makeTxnProto()
	keyA := roachpb.Key("a")

	var ba roachpb.BatchRequest
	ba.Header = roachpb.Header{Txn: &txn}
	ba.Add(&roachpb.HeartbeatTxnRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
	ba.Add(&roachpb.EndTxnRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})

	mockSender.MockSend(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		require.Len(t, ba.Requests, 2)
		require.Equal(t, enginepb.TxnSeq(0), ba.Requests[0].GetInner().Header().Sequence)
		require.Equal(t, enginepb.TxnSeq(1), ba.Requests[1].GetInner().Header().Sequence)

		br := ba.CreateReply()
		br.Txn = ba.Txn
		return br, nil
	})

	br, pErr := s.SendLocked(ctx, ba)
	require.Nil(t, pErr)
	require.NotNil(t, br)
}

// TestSequenceNumberAllocationAfterEpochBump tests that sequence number
// allocation resets to zero after an transaction epoch bump.
func TestSequenceNumberAllocationAfterEpochBump(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	s, mockSender := makeMockTxnSeqNumAllocator()

	txn := makeTxnProto()
	keyA, keyB := roachpb.Key("a"), roachpb.Key("b")

	// Perform a few writes to increase the sequence number counter.
	var ba roachpb.BatchRequest
	ba.Header = roachpb.Header{Txn: &txn}
	ba.Add(&roachpb.ConditionalPutRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
	ba.Add(&roachpb.GetRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
	ba.Add(&roachpb.InitPutRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})

	mockSender.MockSend(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		require.Len(t, ba.Requests, 3)
		require.Equal(t, enginepb.TxnSeq(1), ba.Requests[0].GetInner().Header().Sequence)
		require.Equal(t, enginepb.TxnSeq(1), ba.Requests[1].GetInner().Header().Sequence)
		require.Equal(t, enginepb.TxnSeq(2), ba.Requests[2].GetInner().Header().Sequence)

		br := ba.CreateReply()
		br.Txn = ba.Txn
		return br, nil
	})

	br, pErr := s.SendLocked(ctx, ba)
	require.Nil(t, pErr)
	require.NotNil(t, br)

	// Bump the transaction's epoch.
	s.epochBumpedLocked()

	// Perform a few more writes. The sequence numbers assigned to requests
	// should have started back at zero again.
	ba.Requests = nil
	ba.Add(&roachpb.GetRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
	ba.Add(&roachpb.ConditionalPutRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
	ba.Add(&roachpb.ScanRequest{RequestHeader: roachpb.RequestHeader{Key: keyA, EndKey: keyB}})
	ba.Add(&roachpb.InitPutRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})

	mockSender.MockSend(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		require.Len(t, ba.Requests, 4)
		require.Equal(t, enginepb.TxnSeq(0), ba.Requests[0].GetInner().Header().Sequence)
		require.Equal(t, enginepb.TxnSeq(1), ba.Requests[1].GetInner().Header().Sequence)
		require.Equal(t, enginepb.TxnSeq(1), ba.Requests[2].GetInner().Header().Sequence)
		require.Equal(t, enginepb.TxnSeq(2), ba.Requests[3].GetInner().Header().Sequence)

		br = ba.CreateReply()
		br.Txn = ba.Txn
		return br, nil
	})

	br, pErr = s.SendLocked(ctx, ba)
	require.Nil(t, pErr)
	require.NotNil(t, br)
}

// TestSequenceNumberAllocationAfterLeafInitialization tests that the sequence number
// allocator updates its sequence counter based on the provided LeafTxnInitialState
func TestSequenceNumberAllocationAfterLeafInitialization(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	s, mockSender := makeMockTxnSeqNumAllocator()

	txn := makeTxnProto()
	keyA := roachpb.Key("a")

	tis := roachpb.LeafTxnInputState{
		SteppingModeEnabled: true,
		ReadSeqNum:          4,
	}
	s.initializeLeaf(&tis)

	// Perform a few reads and writes. The sequence numbers assigned should
	// start at the sequence number provided in the LeafTxnInputState.
	var ba roachpb.BatchRequest
	ba.Header = roachpb.Header{Txn: &txn}
	ba.Add(&roachpb.GetRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
	ba.Add(&roachpb.GetRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})

	mockSender.MockSend(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		require.Len(t, ba.Requests, 2)
		require.Equal(t, enginepb.TxnSeq(4), ba.Requests[0].GetInner().Header().Sequence)
		require.Equal(t, enginepb.TxnSeq(4), ba.Requests[1].GetInner().Header().Sequence)

		br := ba.CreateReply()
		br.Txn = ba.Txn
		return br, nil
	})

	br, pErr := s.SendLocked(ctx, ba)
	require.Nil(t, pErr)
	require.NotNil(t, br)
}

// TestSequenceNumberAllocationSavepoint tests that the allocator populates a
// savepoint with the cur seq num.
func TestSequenceNumberAllocationSavepoint(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	s, mockSender := makeMockTxnSeqNumAllocator()
	txn := makeTxnProto()
	keyA, keyB := roachpb.Key("a"), roachpb.Key("b")

	// Perform a few writes to increase the sequence number counter.
	var ba roachpb.BatchRequest
	ba.Header = roachpb.Header{Txn: &txn}
	ba.Add(&roachpb.PutRequest{RequestHeader: roachpb.RequestHeader{Key: keyA}})
	ba.Add(&roachpb.PutRequest{RequestHeader: roachpb.RequestHeader{Key: keyB}})

	mockSender.MockSend(func(ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		br := ba.CreateReply()
		br.Txn = ba.Txn
		return br, nil
	})
	br, pErr := s.SendLocked(ctx, ba)
	require.Nil(t, pErr)
	require.NotNil(t, br)
	require.Equal(t, enginepb.TxnSeq(2), s.writeSeq)

	sp := &savepoint{}
	s.createSavepointLocked(ctx, sp)
	require.Equal(t, enginepb.TxnSeq(2), sp.seqNum)
}
