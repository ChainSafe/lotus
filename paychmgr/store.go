package paychmgr

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/filecoin-project/lotus/chain/types"

	"github.com/filecoin-project/specs-actors/actors/builtin/paych"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	"github.com/ipfs/go-datastore/namespace"
	dsq "github.com/ipfs/go-datastore/query"

	"github.com/filecoin-project/go-address"
	cborrpc "github.com/filecoin-project/go-cbor-util"

	"github.com/filecoin-project/lotus/node/modules/dtypes"
)

var ErrChannelNotTracked = errors.New("channel not tracked")

type Store struct {
	ds datastore.Batching
}

func NewStore(ds dtypes.MetadataDS) *Store {
	ds = namespace.Wrap(ds, datastore.NewKey("/paych/"))
	return &Store{
		ds: ds,
	}
}

const (
	DirInbound  = 1
	DirOutbound = 2
)

type VoucherInfo struct {
	Voucher *paych.SignedVoucher
	Proof   []byte
}

// ChannelInfo keeps track of information about a channel
type ChannelInfo struct {
	// Channel address - may be nil if the channel hasn't been created yet
	Channel *address.Address
	// Control is the address of the account that created the channel
	Control address.Address
	// Target is the address of the account on the other end of the channel
	Target address.Address
	// Sequence distinguishes channels with the same Control / Target
	Sequence uint64
	// Direction indicates if the channel is inbound (this node is the Target)
	// or outbound (this node is the Control)
	Direction uint64
	// Vouchers is a list of all vouchers sent on the channel
	Vouchers []*VoucherInfo
	// NextLane is the number of the next lane that should be used when the
	// client requests a new lane (eg to create a voucher for a new deal)
	NextLane uint64
	// Amount added to the channel.
	// Note: This amount is only used by GetPaych to keep track of how much
	// has locally been added to the channel. It should reflect the channel's
	// Balance on chain as long as all operations occur on the same datastore.
	Amount types.BigInt
	// Pending amount that we're awaiting confirmation of
	PendingAmount types.BigInt
	// CID of a pending create message (while waiting for confirmation)
	CreateMsg *cid.Cid
	// CID of a pending add funds message (while waiting for confirmation)
	AddFundsMsg *cid.Cid
}

// TrackChannel stores a channel, returning an error if the channel was already
// being tracked
func (ps *Store) TrackChannel(ci *ChannelInfo) error {
	_, err := ps.ByAddress(*ci.Channel)
	switch err {
	default:
		return err
	case nil:
		return fmt.Errorf("already tracking channel: %s", ci.Channel)
	case ErrChannelNotTracked:
		return ps.putChannelInfo(ci)
	}
}

func (ps *Store) ListChannels() ([]address.Address, error) {
	cis, err := ps.findChans(func(ci *ChannelInfo) bool {
		return ci.Channel != nil
	}, 0)
	if err != nil {
		return nil, err
	}

	addrs := make([]address.Address, 0, len(cis))
	for _, ci := range cis {
		addrs = append(addrs, *ci.Channel)
	}

	return addrs, nil

	//res, err := ps.ds.Query(dsq.Query{KeysOnly: true})
	//if err != nil {
	//	return nil, err
	//}
	//defer res.Close() //nolint:errcheck
	//
	//var out []address.Address
	//for {
	//	res, ok := res.NextSync()
	//	if !ok {
	//		break
	//	}
	//
	//	if res.Error != nil {
	//		return nil, err
	//	}
	//
	//	addr, err := address.NewFromString(strings.TrimPrefix(res.Key, "/"))
	//	if err != nil {
	//		return nil, xerrors.Errorf("failed reading paych key (%q) from datastore: %w", res.Key, err)
	//	}
	//
	//	out = append(out, addr)
	//}
	//
	//return out, nil
}

func (ps *Store) findChans(filter func(*ChannelInfo) bool, max int) ([]ChannelInfo, error) {
	res, err := ps.ds.Query(dsq.Query{})
	if err != nil {
		return nil, err
	}
	defer res.Close() //nolint:errcheck

	var stored ChannelInfoStorable
	var matches []ChannelInfo

	for {
		res, ok := res.NextSync()
		if !ok {
			break
		}

		if res.Error != nil {
			return nil, err
		}

		ci, err := unmarshallChannelInfo(&stored, res)
		if err != nil {
			return nil, err
		}

		if !filter(ci) {
			continue
		}

		//addr, err := address.NewFromString(strings.TrimPrefix(res.Key, "/"))
		//if err != nil {
		//	return nil, xerrors.Errorf("failed reading paych key (%q) from datastore: %w", res.Key, err)
		//}

		matches = append(matches, *ci)
		if len(matches) == max {
			return matches, nil
		}
	}

	//return addrs, nil
	return matches, nil
}

func (ps *Store) AllocateLane(ch address.Address) (uint64, error) {
	ci, err := ps.ByAddress(ch)
	if err != nil {
		return 0, err
	}

	out := ci.NextLane
	ci.NextLane++

	return out, ps.putChannelInfo(ci)
}

func (ps *Store) VouchersForPaych(ch address.Address) ([]*VoucherInfo, error) {
	ci, err := ps.ByAddress(ch)
	if err != nil {
		return nil, err
	}

	return ci.Vouchers, nil
}

func (ps *Store) ByAddress(addr address.Address) (*ChannelInfo, error) {
	// TODO: cache
	cis, err := ps.findChans(func(ci *ChannelInfo) bool {
		return ci.Channel != nil && *ci.Channel == addr
	}, 1)
	if err != nil {
		return nil, err
	}

	if len(cis) == 0 {
		return nil, ErrChannelNotTracked
	}

	return &cis[0], nil
}

// OutboundByFromTo collects the outbound channel with the given from / to
// addresses and returns the one with the highest sequence number
func (ps *Store) OutboundByFromTo(from address.Address, to address.Address) (*ChannelInfo, error) {
	cis, err := ps.findChans(func(ci *ChannelInfo) bool {
		if ci.Direction != DirOutbound {
			return false
		}
		return ci.Control == from && ci.Target == to
	}, 0)
	if err != nil {
		return nil, err
	}

	return highestSequence(cis)
}

func highestSequence(cis []ChannelInfo) (*ChannelInfo, error) {
	if len(cis) == 0 {
		return nil, ErrChannelNotTracked
	}

	highestIndex := 0
	highest := cis[0].Sequence
	for i := 1; i < len(cis); i++ {
		if cis[i].Sequence > highest {
			highest = cis[i].Sequence
			highestIndex = i
		}
	}
	return &cis[highestIndex], nil
}

// WithPendingAddFunds is used on startup to find channels for which a
// create channel or add funds message has been sent, but lotus shut down
// before the response was received.
func (ps *Store) WithPendingAddFunds() ([]ChannelInfo, error) {
	return ps.findChans(func(ci *ChannelInfo) bool {
		if ci.Direction != DirOutbound {
			return false
		}
		return ci.CreateMsg != nil || ci.AddFundsMsg != nil
	}, 0)
}

// The datastore key used to identify the channel info
func dskeyForChannel(ci *ChannelInfo) datastore.Key {
	return datastore.NewKey(fmt.Sprintf("%s->%s:%d", ci.Control.String(), ci.Target.String(), ci.Sequence))
}

func (ps *Store) putChannelInfo(ci *ChannelInfo) error {
	// TODO: When a channel is settled, the next call to putChannelInfo should
	// create a new channel with a higher Sequence number
	k := dskeyForChannel(ci)

	b, err := marshallChannelInfo(ci)
	if err != nil {
		return err
	}

	return ps.ds.Put(k, b)
}

// ChannelInfoStorable is used to store information about a channel in the data store.
// TODO: Only need this because we can't currently marshall a nil address.Address for
// Channel so we can't marshall ChannelInfo directly
type ChannelInfoStorable struct {
	Channel       string
	Control       address.Address
	Target        address.Address
	Sequence      uint64
	Direction     uint64
	Vouchers      []*VoucherInfo
	NextLane      uint64
	Amount        types.BigInt
	PendingAmount types.BigInt
	AddFundsMsg   *cid.Cid
	CreateMsg     *cid.Cid
}

func marshallChannelInfo(ci *ChannelInfo) ([]byte, error) {
	ch := ""
	if ci.Channel != nil {
		ch = ci.Channel.String()
	}
	toStore := ChannelInfoStorable{
		Channel:       ch,
		Control:       ci.Control,
		Target:        ci.Target,
		Sequence:      ci.Sequence,
		Direction:     ci.Direction,
		Vouchers:      ci.Vouchers,
		NextLane:      ci.NextLane,
		Amount:        ci.Amount,
		PendingAmount: ci.PendingAmount,
		CreateMsg:     ci.CreateMsg,
		AddFundsMsg:   ci.AddFundsMsg,
	}

	return cborrpc.Dump(&toStore)
}

func unmarshallChannelInfo(stored *ChannelInfoStorable, res dsq.Result) (*ChannelInfo, error) {
	if err := stored.UnmarshalCBOR(bytes.NewReader(res.Value)); err != nil {
		return nil, err
	}

	var ch *address.Address
	if len(stored.Channel) > 0 {
		addr, err := address.NewFromString(stored.Channel)
		if err != nil {
			return nil, err
		}
		ch = &addr
	}
	ci := ChannelInfo{
		Channel:       ch,
		Control:       stored.Control,
		Target:        stored.Target,
		Sequence:      stored.Sequence,
		Direction:     stored.Direction,
		Vouchers:      stored.Vouchers,
		NextLane:      stored.NextLane,
		Amount:        stored.Amount,
		PendingAmount: stored.PendingAmount,
		CreateMsg:     stored.CreateMsg,
		AddFundsMsg:   stored.AddFundsMsg,
	}
	return &ci, nil
}
