package paychmgr

import (
	"context"
	"sync"
	"testing"
	"time"

	cborrpc "github.com/filecoin-project/go-cbor-util"

	init_ "github.com/filecoin-project/specs-actors/actors/builtin/init"

	"github.com/filecoin-project/specs-actors/actors/builtin"

	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/types"

	"github.com/filecoin-project/go-address"

	"github.com/filecoin-project/specs-actors/actors/abi/big"
	tutils "github.com/filecoin-project/specs-actors/support/testing"
	"github.com/ipfs/go-cid"
	ds "github.com/ipfs/go-datastore"
	ds_sync "github.com/ipfs/go-datastore/sync"

	"github.com/stretchr/testify/require"
)

type waitingCall struct {
	response chan types.MessageReceipt
}

type mockPaychAPI struct {
	lk           sync.Mutex
	messages     map[cid.Cid]*types.SignedMessage
	waitingCalls []*waitingCall
}

func newMockPaychAPI() *mockPaychAPI {
	return &mockPaychAPI{
		messages: make(map[cid.Cid]*types.SignedMessage),
	}
}

func (pchapi *mockPaychAPI) StateWaitMsg(ctx context.Context, msg cid.Cid, confidence uint64) (*api.MsgLookup, error) {
	response := make(chan types.MessageReceipt)

	pchapi.lk.Lock()
	pchapi.waitingCalls = append(pchapi.waitingCalls, &waitingCall{response: response})
	pchapi.lk.Unlock()

	receipt := <-response

	return &api.MsgLookup{Receipt: receipt}, nil
}

func (pchapi *mockPaychAPI) MpoolPushMessage(ctx context.Context, msg *types.Message) (*types.SignedMessage, error) {
	pchapi.lk.Lock()
	defer pchapi.lk.Unlock()

	smsg := &types.SignedMessage{Message: *msg}
	pchapi.messages[smsg.Cid()] = smsg
	return smsg, nil
}

func (pchapi *mockPaychAPI) pushedMessages(c cid.Cid) *types.SignedMessage {
	pchapi.lk.Lock()
	defer pchapi.lk.Unlock()

	return pchapi.messages[c]
}

func (pchapi *mockPaychAPI) pushedMessageCount() int {
	pchapi.lk.Lock()
	defer pchapi.lk.Unlock()

	return len(pchapi.messages)
}

func (pchapi *mockPaychAPI) finishWaitingCalls(receipt types.MessageReceipt) {
	pchapi.lk.Lock()
	defer pchapi.lk.Unlock()

	for _, call := range pchapi.waitingCalls {
		call.response <- receipt
	}
	pchapi.waitingCalls = nil
}

func (pchapi *mockPaychAPI) close() {
	pchapi.finishWaitingCalls(types.MessageReceipt{
		ExitCode: 0,
		Return:   []byte{},
	})
}

func TestPaychGetCreateChannelMsg(t *testing.T) {
	ctx := context.Background()
	store := NewStore(ds_sync.MutexWrap(ds.NewMapDatastore()))

	from := tutils.NewIDAddr(t, 101)
	to := tutils.NewIDAddr(t, 102)

	sm := newMockStateManager()
	pchapi := newMockPaychAPI()
	defer pchapi.close()

	mgr, err := newManager(sm, store, pchapi)
	require.NoError(t, err)

	ensureFree := big.NewInt(10)
	ch, mcid, err := mgr.GetPaych(ctx, from, to, ensureFree)
	require.NoError(t, err)
	require.Equal(t, address.Undef, ch)

	pushedMsg := pchapi.pushedMessages(mcid)
	require.Equal(t, from, pushedMsg.Message.From)
	require.Equal(t, builtin.InitActorAddr, pushedMsg.Message.To)
	require.Equal(t, ensureFree, pushedMsg.Message.Value)
}

func TestPaychGetAddFundsSameValue(t *testing.T) {
	ctx := context.Background()
	store := NewStore(ds_sync.MutexWrap(ds.NewMapDatastore()))

	from := tutils.NewIDAddr(t, 101)
	to := tutils.NewIDAddr(t, 102)

	sm := newMockStateManager()
	pchapi := newMockPaychAPI()
	defer pchapi.close()

	mgr, err := newManager(sm, store, pchapi)
	require.NoError(t, err)

	// Send create message for a channel with value 10
	ensureFree := big.NewInt(10)
	_, mcid, err := mgr.GetPaych(ctx, from, to, ensureFree)
	require.NoError(t, err)

	// Requesting the same or a lesser amount should just return the same message
	// CID as for the create channel message
	ensureFree2 := big.NewInt(10)
	ch2, mcid2, err := mgr.GetPaych(ctx, from, to, ensureFree2)
	require.NoError(t, err)
	require.Equal(t, address.Undef, ch2)
	require.Equal(t, mcid, mcid2)

	// Should not have sent a second message (because the amount was already
	// covered by the create channel message)
	msgCount := pchapi.pushedMessageCount()
	require.Equal(t, 1, msgCount)
}

func TestPaychGetCreateChannelThenAddFunds(t *testing.T) {
	ctx := context.Background()
	store := NewStore(ds_sync.MutexWrap(ds.NewMapDatastore()))

	ch := tutils.NewIDAddr(t, 100)
	from := tutils.NewIDAddr(t, 101)
	to := tutils.NewIDAddr(t, 102)

	sm := newMockStateManager()
	pchapi := newMockPaychAPI()
	defer pchapi.close()

	mgr, err := newManager(sm, store, pchapi)
	require.NoError(t, err)

	// Send create message for a channel with value 10
	ensureFree := big.NewInt(10)
	_, createMsgCid, err := mgr.GetPaych(ctx, from, to, ensureFree)
	require.NoError(t, err)

	// Should have no channels yet (message sent but channel not created)
	cis, err := mgr.ListChannels()
	require.NoError(t, err)
	require.Len(t, cis, 0)

	// 1. Set up create channel response (sent in response to WaitForMsg())
	createChannelRet := init_.ExecReturn{
		IDAddress:     ch,
		RobustAddress: ch,
	}
	createChannelRetBytes, err := cborrpc.Dump(&createChannelRet)
	require.NoError(t, err)
	createChannelResponse := types.MessageReceipt{
		ExitCode: 0,
		Return:   createChannelRetBytes,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)

		// 2. Request add funds - should block until create channel has completed
		ensureFree2 := big.NewInt(15)
		ch2, addFundsMsgCid, err := mgr.GetPaych(ctx, from, to, ensureFree2)

		// 4. This GetPaych should return after create channel from first
		//    GetPaych completes
		require.NoError(t, err)

		// Expect the channel to have been created
		require.Equal(t, ch, ch2)
		// Expect add funds message CID to be different to create message cid
		require.NotEqual(t, createMsgCid, addFundsMsgCid)

		// Should have one channel, whose address is the channel that was created
		cis, err := mgr.ListChannels()
		require.NoError(t, err)
		require.Len(t, cis, 1)
		require.Equal(t, ch, cis[0])

		// Amount should be amount sent to first GetPaych (to create
		// channel).
		// PendingAmount should be amount sent in second GetPaych
		// (second GetPaych triggered add funds, which has not yet been confirmed)
		ci, err := mgr.GetChannelInfo(ch)
		require.NoError(t, err)
		require.EqualValues(t, 10, ci.Amount.Int64())
		require.EqualValues(t, 15, ci.PendingAmount.Int64())
		require.Nil(t, ci.CreateMsg)

		// Trigger add funds confirmation
		pchapi.finishWaitingCalls(types.MessageReceipt{ExitCode: 0})

		time.Sleep(time.Millisecond * 10)

		// Should still have one channel
		cis, err = mgr.ListChannels()
		require.NoError(t, err)
		require.Len(t, cis, 1)
		require.Equal(t, ch, cis[0])

		// Channel amount should include last amount sent to GetPaych
		ci, err = mgr.GetChannelInfo(ch)
		require.NoError(t, err)
		require.EqualValues(t, 15, ci.Amount.Int64())
		require.EqualValues(t, 0, ci.PendingAmount.Int64())
		require.Nil(t, ci.AddFundsMsg)
	}()

	// Give the go routine above a moment to run
	time.Sleep(time.Millisecond * 10)

	// 3. Send create channel response
	pchapi.finishWaitingCalls(createChannelResponse)

	<-done
}

func TestPaychGetCreateChannelWithErrorThenAddFunds(t *testing.T) {
	ctx := context.Background()
	store := NewStore(ds_sync.MutexWrap(ds.NewMapDatastore()))

	from := tutils.NewIDAddr(t, 101)
	to := tutils.NewIDAddr(t, 102)

	sm := newMockStateManager()
	pchapi := newMockPaychAPI()
	defer pchapi.close()

	mgr, err := newManager(sm, store, pchapi)
	require.NoError(t, err)

	// Send create message for a channel
	ensureFree := big.NewInt(10)
	_, _, err = mgr.GetPaych(ctx, from, to, ensureFree)
	require.NoError(t, err)

	// 1. Set up create channel response (sent in response to WaitForMsg())
	//    This response indicates an error.
	createChannelResponse := types.MessageReceipt{
		ExitCode: 1, // error
		Return:   []byte{},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)

		// 2. Request add funds - should block until create channel has completed
		ensureFree2 := big.NewInt(15)
		_, _, err := mgr.GetPaych(ctx, from, to, ensureFree2)

		// 4. This GetPaych should complete after create channel from first
		//    GetPaych completes, and it should error out because the create
		//    channel was unsuccessful
		require.Error(t, err)
	}()

	// Give the go routine above a moment to run
	time.Sleep(time.Millisecond * 10)

	// 3. Send create channel response
	pchapi.finishWaitingCalls(createChannelResponse)

	<-done
}

func TestPaychGetRecoverAfterError(t *testing.T) {
	ctx := context.Background()
	store := NewStore(ds_sync.MutexWrap(ds.NewMapDatastore()))

	ch := tutils.NewIDAddr(t, 100)
	from := tutils.NewIDAddr(t, 101)
	to := tutils.NewIDAddr(t, 102)

	sm := newMockStateManager()
	pchapi := newMockPaychAPI()
	defer pchapi.close()

	mgr, err := newManager(sm, store, pchapi)
	require.NoError(t, err)

	// Send create message for a channel
	ensureFree := big.NewInt(10)
	_, _, err = mgr.GetPaych(ctx, from, to, ensureFree)
	require.NoError(t, err)

	time.Sleep(time.Millisecond * 10)

	// Send error create channel response
	pchapi.finishWaitingCalls(types.MessageReceipt{
		ExitCode: 1, // error
		Return:   []byte{},
	})

	time.Sleep(time.Millisecond * 10)

	// Send create message for a channel again
	ensureFree2 := big.NewInt(7)
	_, _, err = mgr.GetPaych(ctx, from, to, ensureFree2)
	require.NoError(t, err)

	time.Sleep(time.Millisecond * 10)

	// Send success create channel response
	createChannelRet := init_.ExecReturn{
		IDAddress:     ch,
		RobustAddress: ch,
	}
	createChannelRetBytes, err := cborrpc.Dump(&createChannelRet)
	require.NoError(t, err)
	createChannelResponse := types.MessageReceipt{
		ExitCode: 0,
		Return:   createChannelRetBytes,
	}
	pchapi.finishWaitingCalls(createChannelResponse)

	time.Sleep(time.Millisecond * 10)

	// Should have one channel, whose address is the channel that was created
	cis, err := mgr.ListChannels()
	require.NoError(t, err)
	require.Len(t, cis, 1)
	require.Equal(t, ch, cis[0])

	ci, err := mgr.GetChannelInfo(ch)
	require.NoError(t, err)
	require.Equal(t, ensureFree2, ci.Amount)
	require.EqualValues(t, 0, ci.PendingAmount.Int64())
	require.Nil(t, ci.CreateMsg)
}

func TestPaychGetRecoverAfterAddFundsError(t *testing.T) {
	ctx := context.Background()
	store := NewStore(ds_sync.MutexWrap(ds.NewMapDatastore()))

	ch := tutils.NewIDAddr(t, 100)
	from := tutils.NewIDAddr(t, 101)
	to := tutils.NewIDAddr(t, 102)

	sm := newMockStateManager()
	pchapi := newMockPaychAPI()
	defer pchapi.close()

	mgr, err := newManager(sm, store, pchapi)
	require.NoError(t, err)

	// Send create message for a channel
	ensureFree := big.NewInt(10)
	_, _, err = mgr.GetPaych(ctx, from, to, ensureFree)
	require.NoError(t, err)

	time.Sleep(time.Millisecond * 10)

	// Send success create channel response
	createChannelRet := init_.ExecReturn{
		IDAddress:     ch,
		RobustAddress: ch,
	}
	createChannelRetBytes, err := cborrpc.Dump(&createChannelRet)
	require.NoError(t, err)
	createChannelResponse := types.MessageReceipt{
		ExitCode: 0,
		Return:   createChannelRetBytes,
	}
	pchapi.finishWaitingCalls(createChannelResponse)

	time.Sleep(time.Millisecond * 10)

	// Send add funds message for channel
	ensureFree2 := big.NewInt(15)
	_, _, err = mgr.GetPaych(ctx, from, to, ensureFree2)
	require.NoError(t, err)

	time.Sleep(time.Millisecond * 10)

	// Send error add funds response
	pchapi.finishWaitingCalls(types.MessageReceipt{
		ExitCode: 1, // error
		Return:   []byte{},
	})

	time.Sleep(time.Millisecond * 10)

	// Should have one channel, whose address is the channel that was created
	cis, err := mgr.ListChannels()
	require.NoError(t, err)
	require.Len(t, cis, 1)
	require.Equal(t, ch, cis[0])

	ci, err := mgr.GetChannelInfo(ch)
	require.NoError(t, err)
	require.Equal(t, ensureFree, ci.Amount)
	require.EqualValues(t, 0, ci.PendingAmount.Int64())
	require.Nil(t, ci.CreateMsg)
	require.Nil(t, ci.AddFundsMsg)

	// Send add funds message for channel again
	ensureFree3 := big.NewInt(12)
	_, _, err = mgr.GetPaych(ctx, from, to, ensureFree3)
	require.NoError(t, err)

	time.Sleep(time.Millisecond * 10)

	// Send success add funds response
	pchapi.finishWaitingCalls(types.MessageReceipt{
		ExitCode: 0,
		Return:   []byte{},
	})

	time.Sleep(time.Millisecond * 10)

	// Should have one channel, whose address is the channel that was created
	cis, err = mgr.ListChannels()
	require.NoError(t, err)
	require.Len(t, cis, 1)
	require.Equal(t, ch, cis[0])

	// Amount should be equal to ensure free for successful add funds msg
	ci, err = mgr.GetChannelInfo(ch)
	require.NoError(t, err)
	require.Equal(t, ensureFree3, ci.Amount)
	require.EqualValues(t, 0, ci.PendingAmount.Int64())
	require.Nil(t, ci.CreateMsg)
	require.Nil(t, ci.AddFundsMsg)
}

func TestPaychGetRestartAfterCreateChannelMsg(t *testing.T) {
	ctx := context.Background()
	store := NewStore(ds_sync.MutexWrap(ds.NewMapDatastore()))

	ch := tutils.NewIDAddr(t, 100)
	from := tutils.NewIDAddr(t, 101)
	to := tutils.NewIDAddr(t, 102)

	sm := newMockStateManager()
	pchapi := newMockPaychAPI()

	mgr, err := newManager(sm, store, pchapi)
	require.NoError(t, err)

	// Send create message for a channel with value 10
	ensureFree := big.NewInt(10)
	_, createMsgCid, err := mgr.GetPaych(ctx, from, to, ensureFree)
	require.NoError(t, err)

	// Simulate shutting down lotus
	pchapi.close()

	// Create a new manager with the same datastore
	sm2 := newMockStateManager()
	pchapi2 := newMockPaychAPI()
	defer pchapi2.close()

	mgr2, err := newManager(sm2, store, pchapi2)
	require.NoError(t, err)

	// Should have no channels yet (message sent but channel not created)
	cis, err := mgr2.ListChannels()
	require.NoError(t, err)
	require.Len(t, cis, 0)

	// 1. Set up create channel response (sent in response to WaitForMsg())
	createChannelRet := init_.ExecReturn{
		IDAddress:     ch,
		RobustAddress: ch,
	}
	createChannelRetBytes, err := cborrpc.Dump(&createChannelRet)
	require.NoError(t, err)
	createChannelResponse := types.MessageReceipt{
		ExitCode: 0,
		Return:   createChannelRetBytes,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)

		// 2. Request add funds - should block until create channel has completed
		ensureFree2 := big.NewInt(15)
		ch2, addFundsMsgCid, err := mgr2.GetPaych(ctx, from, to, ensureFree2)

		// 4. This GetPaych should return after create channel from first
		//    GetPaych completes
		require.NoError(t, err)

		// Expect the channel to have been created
		require.Equal(t, ch, ch2)
		// Expect add funds message CID to be different to create message cid
		require.NotEqual(t, createMsgCid, addFundsMsgCid)

		// Should have one channel, whose address is the channel that was created
		cis, err := mgr2.ListChannels()
		require.NoError(t, err)
		require.Len(t, cis, 1)
		require.Equal(t, ch, cis[0])

		// Amount should be amount sent to first GetPaych (to create
		// channel).
		// PendingAmount should be amount sent in second GetPaych
		// (second GetPaych triggered add funds, which has not yet been confirmed)
		ci, err := mgr2.GetChannelInfo(ch)
		require.NoError(t, err)
		require.EqualValues(t, 10, ci.Amount.Int64())
		require.EqualValues(t, 15, ci.PendingAmount.Int64())
		require.Nil(t, ci.CreateMsg)
	}()

	// Give the go routine above a moment to run
	time.Sleep(time.Millisecond * 10)

	// 3. Send create channel response
	pchapi2.finishWaitingCalls(createChannelResponse)

	<-done
}

func TestPaychGetRestartAfterAddFundsMsg(t *testing.T) {
	ctx := context.Background()
	store := NewStore(ds_sync.MutexWrap(ds.NewMapDatastore()))

	ch := tutils.NewIDAddr(t, 100)
	from := tutils.NewIDAddr(t, 101)
	to := tutils.NewIDAddr(t, 102)

	sm := newMockStateManager()
	pchapi := newMockPaychAPI()

	mgr, err := newManager(sm, store, pchapi)
	require.NoError(t, err)

	// Send create message for a channel
	ensureFree := big.NewInt(10)
	_, _, err = mgr.GetPaych(ctx, from, to, ensureFree)
	require.NoError(t, err)

	time.Sleep(time.Millisecond * 10)

	// Send success create channel response
	createChannelRet := init_.ExecReturn{
		IDAddress:     ch,
		RobustAddress: ch,
	}
	createChannelRetBytes, err := cborrpc.Dump(&createChannelRet)
	require.NoError(t, err)
	createChannelResponse := types.MessageReceipt{
		ExitCode: 0,
		Return:   createChannelRetBytes,
	}
	pchapi.finishWaitingCalls(createChannelResponse)

	time.Sleep(time.Millisecond * 10)

	// Send add funds message for channel
	ensureFree2 := big.NewInt(15)
	_, _, err = mgr.GetPaych(ctx, from, to, ensureFree2)
	require.NoError(t, err)

	time.Sleep(time.Millisecond * 10)

	// Simulate shutting down lotus
	pchapi.close()

	// Create a new manager with the same datastore
	sm2 := newMockStateManager()
	pchapi2 := newMockPaychAPI()
	defer pchapi2.close()

	time.Sleep(time.Millisecond * 10)

	mgr2, err := newManager(sm2, store, pchapi2)
	require.NoError(t, err)

	// Send success add funds response
	pchapi2.finishWaitingCalls(types.MessageReceipt{
		ExitCode: 0,
		Return:   []byte{},
	})

	time.Sleep(time.Millisecond * 10)

	// Should have one channel, whose address is the channel that was created
	cis, err := mgr2.ListChannels()
	require.NoError(t, err)
	require.Len(t, cis, 1)
	require.Equal(t, ch, cis[0])

	// Amount should be equal to ensure free for successful add funds msg
	ci, err := mgr2.GetChannelInfo(ch)
	require.NoError(t, err)
	require.Equal(t, ensureFree2, ci.Amount)
	require.EqualValues(t, 0, ci.PendingAmount.Int64())
	require.Nil(t, ci.CreateMsg)
	require.Nil(t, ci.AddFundsMsg)
}
