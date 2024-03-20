package cliutil

import (
	"context"
	"github.com/filecoin-project/go-address"
	actorstypes "github.com/filecoin-project/go-state-types/actors"
	"github.com/filecoin-project/go-state-types/manifest"
	"github.com/filecoin-project/lotus/chain/actors"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/chain/types/ethtypes"
	"golang.org/x/xerrors"
	"strings"
)

type parseAPI interface {
	StateGetActor(ctx context.Context, actor address.Address, tsk types.TipSetKey) (*types.Actor, error)
	StateLookupID(context.Context, address.Address, types.TipSetKey) (address.Address, error)
}

func ParseAddress(ctx context.Context, addr string, lapi parseAPI) (ethtypes.EthAddress, error) {
	if strings.HasPrefix(addr, "0x") {
		return ethtypes.ParseEthAddress(addr)
	}
	// user passed f1, f2, f3, or f4
	filAddr, err := address.NewFromString(addr)

	if err != nil {
		return ethtypes.EthAddress{}, err
	}

	if filAddr.Protocol() == address.ID {
		actor, err := lapi.StateGetActor(ctx, filAddr, types.EmptyTSK)
		if err != nil {
			return ethtypes.EthAddress{}, err
		}
		if actor.Address != nil {
			return ethtypes.EthAddressFromFilecoinAddress(*actor.Address)
		}

		actorCodeEvm, success := actors.GetActorCodeID(actorstypes.Version(actors.LatestVersion), manifest.EvmKey)
		if !success {
			return ethtypes.EthAddress{}, xerrors.New("actor code not found")
		}
		if actor.Code.Equals(actorCodeEvm) {
			return ethtypes.EthAddress{}, xerrors.New("Cant pass an ID address of an EVM actor")
		}

		actorCodeEthAccount, success := actors.GetActorCodeID(actorstypes.Version(actors.LatestVersion), manifest.EthAccountKey)
		if !success {
			return ethtypes.EthAddress{}, xerrors.New("actor code not found")
		}
		if actor.Code.Equals(actorCodeEthAccount) {
			return ethtypes.EthAddress{}, xerrors.New("Cant pass an ID address of an Eth Account")
		}
	}

	if filAddr.Protocol() != address.ID && filAddr.Protocol() != address.Delegated {
		filAddr, err = lapi.StateLookupID(ctx, filAddr, types.EmptyTSK)
		if err != nil {
			return ethtypes.EthAddress{}, err
		}
	}

	return ethtypes.EthAddressFromFilecoinAddress(filAddr)
}
