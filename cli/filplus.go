package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	cbor "github.com/ipfs/go-ipld-cbor"
	"github.com/urfave/cli/v2"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	actorstypes "github.com/filecoin-project/go-state-types/actors"
	"github.com/filecoin-project/go-state-types/big"
	verifregtypes8 "github.com/filecoin-project/go-state-types/builtin/v8/verifreg"
	verifregtypes9 "github.com/filecoin-project/go-state-types/builtin/v9/verifreg"
	"github.com/filecoin-project/go-state-types/network"

	"github.com/filecoin-project/lotus/api/v0api"
	"github.com/filecoin-project/lotus/blockstore"
	"github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/chain/actors"
	"github.com/filecoin-project/lotus/chain/actors/adt"
	"github.com/filecoin-project/lotus/chain/actors/builtin/datacap"
	"github.com/filecoin-project/lotus/chain/actors/builtin/verifreg"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/lib/tablewriter"
)

var filplusCmd = &cli.Command{
	Name:  "filplus",
	Usage: "Interact with the verified registry actor used by Filplus",
	Flags: []cli.Flag{},
	Subcommands: []*cli.Command{
		filplusVerifyClientCmd,
		filplusListNotariesCmd,
		filplusListClientsCmd,
		filplusCheckClientCmd,
		filplusCheckNotaryCmd,
		filplusSignRemoveDataCapProposal,
		filplusListAllocationsCmd,
		filplusListClaimsCmd,
		filplusRemoveExpiredAllocationsCmd,
		filplusRemoveExpiredClaimsCmd,
	},
}

var filplusVerifyClientCmd = &cli.Command{
	Name:      "grant-datacap",
	Usage:     "give allowance to the specified verified client address",
	ArgsUsage: "[clientAddress datacap]",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:     "from",
			Usage:    "specify your notary address to send the message from",
			Required: true,
		},
	},
	Action: func(cctx *cli.Context) error {
		froms := cctx.String("from")
		if froms == "" {
			return fmt.Errorf("must specify from address with --from")
		}

		fromk, err := address.NewFromString(froms)
		if err != nil {
			return err
		}

		if cctx.NArg() != 2 {
			return IncorrectNumArgs(cctx)
		}

		target, err := address.NewFromString(cctx.Args().Get(0))
		if err != nil {
			return err
		}

		allowance, err := types.BigFromString(cctx.Args().Get(1))
		if err != nil {
			return err
		}

		api, closer, err := GetFullNodeAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()
		ctx := ReqContext(cctx)

		found, dcap, err := checkNotary(ctx, api, fromk)
		if err != nil {
			return err
		}

		if !found {
			return xerrors.New("sender address must be a notary")
		}

		if dcap.Cmp(allowance.Int) < 0 {
			return xerrors.Errorf("cannot allot more allowance than notary data cap: %s < %s", dcap, allowance)
		}

		// TODO: This should be abstracted over actor versions
		params, err := actors.SerializeParams(&verifregtypes9.AddVerifiedClientParams{Address: target, Allowance: allowance})
		if err != nil {
			return err
		}

		msg := &types.Message{
			To:     verifreg.Address,
			From:   fromk,
			Method: verifreg.Methods.AddVerifiedClient,
			Params: params,
		}

		smsg, err := api.MpoolPushMessage(ctx, msg, nil)
		if err != nil {
			return err
		}

		fmt.Printf("message sent, now waiting on cid: %s\n", smsg.Cid())

		mwait, err := api.StateWaitMsg(ctx, smsg.Cid(), build.MessageConfidence)
		if err != nil {
			return err
		}

		if mwait.Receipt.ExitCode.IsError() {
			return fmt.Errorf("failed to add verified client: %d", mwait.Receipt.ExitCode)
		}

		return nil
	},
}

var filplusListNotariesCmd = &cli.Command{
	Name:  "list-notaries",
	Usage: "list all notaries",
	Action: func(cctx *cli.Context) error {
		if cctx.NArg() != 0 {
			return IncorrectNumArgs(cctx)
		}

		api, closer, err := GetFullNodeAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()
		ctx := ReqContext(cctx)

		act, err := api.StateGetActor(ctx, verifreg.Address, types.EmptyTSK)
		if err != nil {
			return err
		}

		apibs := blockstore.NewAPIBlockstore(api)
		store := adt.WrapStore(ctx, cbor.NewCborStore(apibs))

		st, err := verifreg.Load(store, act)
		if err != nil {
			return err
		}
		return st.ForEachVerifier(func(addr address.Address, dcap abi.StoragePower) error {
			_, err := fmt.Printf("%s: %s\n", addr, dcap)
			return err
		})
	},
}

var filplusListClientsCmd = &cli.Command{
	Name:  "list-clients",
	Usage: "list all verified clients",
	Action: func(cctx *cli.Context) error {
		if cctx.NArg() != 0 {
			return IncorrectNumArgs(cctx)
		}

		api, closer, err := GetFullNodeAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()
		ctx := ReqContext(cctx)

		apibs := blockstore.NewAPIBlockstore(api)
		store := adt.WrapStore(ctx, cbor.NewCborStore(apibs))

		nv, err := api.StateNetworkVersion(ctx, types.EmptyTSK)
		if err != nil {
			return err
		}

		av, err := actorstypes.VersionForNetwork(nv)
		if err != nil {
			return err
		}

		if av <= 8 {
			act, err := api.StateGetActor(ctx, verifreg.Address, types.EmptyTSK)
			if err != nil {
				return err
			}

			st, err := verifreg.Load(store, act)
			if err != nil {
				return err
			}
			return st.ForEachClient(func(addr address.Address, dcap abi.StoragePower) error {
				_, err := fmt.Printf("%s: %s\n", addr, dcap)
				return err
			})
		}
		act, err := api.StateGetActor(ctx, datacap.Address, types.EmptyTSK)
		if err != nil {
			return err
		}

		st, err := datacap.Load(store, act)
		if err != nil {
			return err
		}
		return st.ForEachClient(func(addr address.Address, dcap abi.StoragePower) error {
			_, err := fmt.Printf("%s: %s\n", addr, dcap)
			return err
		})
	},
}

var filplusListAllocationsCmd = &cli.Command{
	Name:      "list-allocations",
	Usage:     "List allocations available in verified registry actor or made by a client if specified",
	ArgsUsage: "clientAddress",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:  "expired",
			Usage: "list only expired allocations",
		},
		&cli.BoolFlag{
			Name:  "json",
			Usage: "output results in json format",
			Value: false,
		},
	},
	Action: func(cctx *cli.Context) error {
		if cctx.NArg() > 1 {
			return IncorrectNumArgs(cctx)
		}

		api, closer, err := GetFullNodeAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()
		ctx := ReqContext(cctx)

		writeOut := func(tsHeight abi.ChainEpoch, allocations map[verifreg.AllocationId]verifreg.Allocation, json, expired bool) error {
			// Map Keys. Corresponds to the standard tablewriter output
			allocationID := "AllocationID"
			client := "Client"
			provider := "Miner"
			pieceCid := "PieceCid"
			pieceSize := "PieceSize"
			tMin := "TermMin"
			tMax := "TermMax"
			expr := "Expiration"

			// One-to-one mapping between tablewriter keys and JSON keys
			tableKeysToJsonKeys := map[string]string{
				allocationID: strings.ToLower(allocationID),
				client:       strings.ToLower(client),
				provider:     strings.ToLower(provider),
				pieceCid:     strings.ToLower(pieceCid),
				pieceSize:    strings.ToLower(pieceSize),
				tMin:         strings.ToLower(tMin),
				tMax:         strings.ToLower(tMax),
				expr:         strings.ToLower(expr),
			}

			var allocs []map[string]interface{}

			for key, val := range allocations {
				if tsHeight > val.Expiration || !expired {
					alloc := map[string]interface{}{
						allocationID: key,
						client:       val.Client,
						provider:     val.Provider,
						pieceCid:     val.Data,
						pieceSize:    val.Size,
						tMin:         val.TermMin,
						tMax:         val.TermMax,
						expr:         val.Expiration,
					}
					allocs = append(allocs, alloc)
				}
			}

			if json {
				// get a new list of allocations with json keys instead of tablewriter keys
				var jsonAllocs []map[string]interface{}
				for _, alloc := range allocs {
					jsonAlloc := make(map[string]interface{})
					for k, v := range alloc {
						jsonAlloc[tableKeysToJsonKeys[k]] = v
					}
					jsonAllocs = append(jsonAllocs, jsonAlloc)
				}
				// then return this!
				return PrintJson(jsonAllocs)
			}
			// Init the tablewriter's columns
			tw := tablewriter.New(
				tablewriter.Col(allocationID),
				tablewriter.Col(client),
				tablewriter.Col(provider),
				tablewriter.Col(pieceCid),
				tablewriter.Col(pieceSize),
				tablewriter.Col(tMin),
				tablewriter.Col(tMax),
				tablewriter.NewLineCol(expr))
			// populate it with content
			for _, alloc := range allocs {
				tw.Write(alloc)
			}
			// return the corresponding string
			return tw.Flush(os.Stdout)
		}

		store := adt.WrapStore(ctx, cbor.NewCborStore(blockstore.NewAPIBlockstore(api)))

		verifregActor, err := api.StateGetActor(ctx, verifreg.Address, types.EmptyTSK)
		if err != nil {
			return err
		}

		verifregState, err := verifreg.Load(store, verifregActor)
		if err != nil {
			return err
		}

		ts, err := api.ChainHead(ctx)
		if err != nil {
			return err
		}

		if cctx.NArg() == 1 {
			clientAddr, err := address.NewFromString(cctx.Args().Get(0))
			if err != nil {
				return err
			}

			clientIdAddr, err := api.StateLookupID(ctx, clientAddr, types.EmptyTSK)
			if err != nil {
				return err
			}

			allocationsMap, err := verifregState.GetAllocations(clientIdAddr)
			if err != nil {
				return err
			}

			return writeOut(ts.Height(), allocationsMap, cctx.Bool("json"), cctx.Bool("expired"))
		}

		allocationsMap, err := verifregState.GetAllAllocations()
		if err != nil {
			return err
		}

		return writeOut(ts.Height(), allocationsMap, cctx.Bool("json"), cctx.Bool("expired"))

	},
}

var filplusListClaimsCmd = &cli.Command{
	Name:      "list-claims",
	Usage:     "List claims available in verified registry actor or made by provider if specified",
	ArgsUsage: "providerAddress",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:  "expired",
			Usage: "list only expired claims",
		},
	},
	Action: func(cctx *cli.Context) error {
		if cctx.NArg() > 1 {
			return IncorrectNumArgs(cctx)
		}

		api, closer, err := GetFullNodeAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()
		ctx := ReqContext(cctx)

		writeOut := func(tsHeight abi.ChainEpoch, claims map[verifreg.ClaimId]verifreg.Claim, json, expired bool) error {
			// Map Keys. Corresponds to the standard tablewriter output
			claimID := "ClaimID"
			provider := "Provider"
			client := "Client"
			data := "Data"
			size := "Size"
			tMin := "TermMin"
			tMax := "TermMax"
			tStart := "TermStart"
			sector := "Sector"

			// One-to-one mapping between tablewriter keys and JSON keys
			tableKeysToJsonKeys := map[string]string{
				claimID:  strings.ToLower(claimID),
				provider: strings.ToLower(provider),
				client:   strings.ToLower(client),
				data:     strings.ToLower(data),
				size:     strings.ToLower(size),
				tMin:     strings.ToLower(tMin),
				tMax:     strings.ToLower(tMax),
				tStart:   strings.ToLower(tStart),
				sector:   strings.ToLower(sector),
			}

			var claimList []map[string]interface{}

			for key, val := range claims {
				if tsHeight > val.TermStart+val.TermMax || !expired {
					claim := map[string]interface{}{
						claimID:  key,
						provider: val.Provider,
						client:   val.Client,
						data:     val.Data,
						size:     val.Size,
						tMin:     val.TermMin,
						tMax:     val.TermMax,
						tStart:   val.TermStart,
						sector:   val.Sector,
					}
					claimList = append(claimList, claim)
				}
			}

			if json {
				// get a new list of claims with json keys instead of tablewriter keys
				var jsonClaims []map[string]interface{}
				for _, claim := range claimList {
					jsonClaim := make(map[string]interface{})
					for k, v := range claim {
						jsonClaim[tableKeysToJsonKeys[k]] = v
					}
					jsonClaims = append(jsonClaims, jsonClaim)
				}
				// then return this!
				return PrintJson(jsonClaims)
			}
			// Init the tablewriter's columns
			tw := tablewriter.New(
				tablewriter.Col(claimID),
				tablewriter.Col(client),
				tablewriter.Col(provider),
				tablewriter.Col(data),
				tablewriter.Col(size),
				tablewriter.Col(tMin),
				tablewriter.Col(tMax),
				tablewriter.Col(tStart),
				tablewriter.NewLineCol(sector))
			// populate it with content
			for _, alloc := range claimList {

				tw.Write(alloc)
			}
			// return the corresponding string
			return tw.Flush(os.Stdout)
		}

		store := adt.WrapStore(ctx, cbor.NewCborStore(blockstore.NewAPIBlockstore(api)))

		verifregActor, err := api.StateGetActor(ctx, verifreg.Address, types.EmptyTSK)
		if err != nil {
			return err
		}

		verifregState, err := verifreg.Load(store, verifregActor)
		if err != nil {
			return err
		}

		ts, err := api.ChainHead(ctx)
		if err != nil {
			return err
		}

		if cctx.NArg() == 1 {
			providerAddr, err := address.NewFromString(cctx.Args().Get(0))
			if err != nil {
				return err
			}

			providerIdAddr, err := api.StateLookupID(ctx, providerAddr, types.EmptyTSK)
			if err != nil {
				return err
			}

			claimsMap, err := verifregState.GetClaims(providerIdAddr)
			if err != nil {
				return err
			}

			return writeOut(ts.Height(), claimsMap, cctx.Bool("json"), cctx.Bool("expired"))
		}

		claimsMap, err := verifregState.GetAllClaims()
		if err != nil {
			return err
		}

		return writeOut(ts.Height(), claimsMap, cctx.Bool("json"), cctx.Bool("expired"))
	},
}

var filplusRemoveExpiredAllocationsCmd = &cli.Command{
	Name:      "remove-expired-allocations",
	Usage:     "remove expired allocations (if no allocations are specified all eligible allocations are removed)",
	ArgsUsage: "clientAddress Optional[...allocationId]",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "from",
			Usage: "optionally specify the account to send the message from",
		},
	},
	Action: func(cctx *cli.Context) error {
		if cctx.NArg() < 1 {
			return IncorrectNumArgs(cctx)
		}

		api, closer, err := GetFullNodeAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()
		ctx := ReqContext(cctx)

		args := cctx.Args().Slice()

		clientAddr, err := address.NewFromString(args[0])
		if err != nil {
			return err
		}

		clientIdAddr, err := api.StateLookupID(ctx, clientAddr, types.EmptyTSK)
		if err != nil {
			return err
		}

		clientId, err := address.IDFromAddress(clientIdAddr)
		if err != nil {
			return err
		}

		fromAddr := clientIdAddr
		if from := cctx.String("from"); from != "" {
			addr, err := address.NewFromString(from)
			if err != nil {
				return err
			}

			fromAddr = addr
		}

		allocationIDs := make([]verifregtypes9.AllocationId, len(args)-1)
		for i, allocationString := range args[1:] {
			id, err := strconv.ParseUint(allocationString, 10, 64)
			if err != nil {
				return err
			}
			allocationIDs[i] = verifregtypes9.AllocationId(id)
		}

		params, err := actors.SerializeParams(&verifregtypes9.RemoveExpiredAllocationsParams{
			Client:        abi.ActorID(clientId),
			AllocationIds: allocationIDs,
		})
		if err != nil {
			return err
		}

		msg := &types.Message{
			To:     verifreg.Address,
			From:   fromAddr,
			Method: verifreg.Methods.RemoveExpiredAllocations,
			Params: params,
		}

		smsg, err := api.MpoolPushMessage(ctx, msg, nil)
		if err != nil {
			return err
		}

		fmt.Printf("message sent, now waiting on cid: %s\n", smsg.Cid())

		mwait, err := api.StateWaitMsg(ctx, smsg.Cid(), build.MessageConfidence)
		if err != nil {
			return err
		}

		if mwait.Receipt.ExitCode.IsError() {
			return fmt.Errorf("failed to remove expired allocations: %d", mwait.Receipt.ExitCode)
		}

		return nil
	},
}

var filplusRemoveExpiredClaimsCmd = &cli.Command{
	Name:      "remove-expired-claims",
	Usage:     "remove expired claims (if no claims are specified all eligible claims are removed)",
	ArgsUsage: "providerAddress Optional[...claimId]",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "from",
			Usage: "optionally specify the account to send the message from",
		},
	},
	Action: func(cctx *cli.Context) error {
		if cctx.NArg() < 1 {
			return IncorrectNumArgs(cctx)
		}

		api, closer, err := GetFullNodeAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()
		ctx := ReqContext(cctx)

		args := cctx.Args().Slice()

		providerAddr, err := address.NewFromString(args[0])
		if err != nil {
			return err
		}

		providerIdAddr, err := api.StateLookupID(ctx, providerAddr, types.EmptyTSK)
		if err != nil {
			return err
		}

		providerId, err := address.IDFromAddress(providerIdAddr)
		if err != nil {
			return err
		}

		fromAddr := providerIdAddr
		if from := cctx.String("from"); from != "" {
			addr, err := address.NewFromString(from)
			if err != nil {
				return err
			}

			fromAddr = addr
		}

		claimIDs := make([]verifregtypes9.ClaimId, len(args)-1)
		for i, claimStr := range args[1:] {
			id, err := strconv.ParseUint(claimStr, 10, 64)
			if err != nil {
				return err
			}
			claimIDs[i] = verifregtypes9.ClaimId(id)
		}

		params, err := actors.SerializeParams(&verifregtypes9.RemoveExpiredClaimsParams{
			Provider: abi.ActorID(providerId),
			ClaimIds: claimIDs,
		})
		if err != nil {
			return err
		}

		msg := &types.Message{
			To:     verifreg.Address,
			From:   fromAddr,
			Method: verifreg.Methods.RemoveExpiredClaims,
			Params: params,
		}

		smsg, err := api.MpoolPushMessage(ctx, msg, nil)
		if err != nil {
			return err
		}

		fmt.Printf("message sent, now waiting on cid: %s\n", smsg.Cid())

		mwait, err := api.StateWaitMsg(ctx, smsg.Cid(), build.MessageConfidence)
		if err != nil {
			return err
		}

		if mwait.Receipt.ExitCode.IsError() {
			return fmt.Errorf("failed to remove expired allocations: %d", mwait.Receipt.ExitCode)
		}

		return nil
	},
}

var filplusCheckClientCmd = &cli.Command{
	Name:      "check-client-datacap",
	Usage:     "check verified client remaining bytes",
	ArgsUsage: "clientAddress",
	Action: func(cctx *cli.Context) error {
		if cctx.NArg() != 1 {
			return fmt.Errorf("must specify client address to check")
		}

		caddr, err := address.NewFromString(cctx.Args().First())
		if err != nil {
			return err
		}

		api, closer, err := GetFullNodeAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()
		ctx := ReqContext(cctx)

		dcap, err := api.StateVerifiedClientStatus(ctx, caddr, types.EmptyTSK)
		if err != nil {
			return err
		}
		if dcap == nil {
			return xerrors.Errorf("client %s is not a verified client", caddr)
		}

		fmt.Println(*dcap)

		return nil
	},
}

var filplusCheckNotaryCmd = &cli.Command{
	Name:      "check-notary-datacap",
	Usage:     "check a notary's remaining bytes",
	ArgsUsage: "notaryAddress",
	Action: func(cctx *cli.Context) error {
		if cctx.NArg() != 1 {
			return fmt.Errorf("must specify notary address to check")
		}

		vaddr, err := address.NewFromString(cctx.Args().First())
		if err != nil {
			return err
		}

		api, closer, err := GetFullNodeAPI(cctx)
		if err != nil {
			return err
		}
		defer closer()
		ctx := ReqContext(cctx)

		found, dcap, err := checkNotary(ctx, api, vaddr)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("not found")
		}

		fmt.Println(dcap)

		return nil
	},
}

func checkNotary(ctx context.Context, api v0api.FullNode, vaddr address.Address) (bool, abi.StoragePower, error) {
	vid, err := api.StateLookupID(ctx, vaddr, types.EmptyTSK)
	if err != nil {
		return false, big.Zero(), err
	}

	act, err := api.StateGetActor(ctx, verifreg.Address, types.EmptyTSK)
	if err != nil {
		return false, big.Zero(), err
	}

	apibs := blockstore.NewAPIBlockstore(api)
	store := adt.WrapStore(ctx, cbor.NewCborStore(apibs))

	st, err := verifreg.Load(store, act)
	if err != nil {
		return false, big.Zero(), err
	}

	return st.VerifierDataCap(vid)
}

var filplusSignRemoveDataCapProposal = &cli.Command{
	Name:      "sign-remove-data-cap-proposal",
	Usage:     "allows a notary to sign a Remove Data Cap Proposal",
	ArgsUsage: "[verifierAddress clientAddress allowanceToRemove]",
	Flags: []cli.Flag{
		&cli.Int64Flag{
			Name:     "id",
			Usage:    "specify the RemoveDataCapProposal ID (will look up on chain if unspecified)",
			Required: false,
		},
	},
	Action: func(cctx *cli.Context) error {
		if cctx.NArg() != 3 {
			return IncorrectNumArgs(cctx)
		}

		api, closer, err := GetFullNodeAPI(cctx)
		if err != nil {
			return xerrors.Errorf("failed to get full node api: %w", err)
		}
		defer closer()
		ctx := ReqContext(cctx)

		act, err := api.StateGetActor(ctx, verifreg.Address, types.EmptyTSK)
		if err != nil {
			return xerrors.Errorf("failed to get verifreg actor: %w", err)
		}

		apibs := blockstore.NewAPIBlockstore(api)
		store := adt.WrapStore(ctx, cbor.NewCborStore(apibs))

		st, err := verifreg.Load(store, act)
		if err != nil {
			return xerrors.Errorf("failed to load verified registry state: %w", err)
		}

		verifier, err := address.NewFromString(cctx.Args().Get(0))
		if err != nil {
			return err
		}
		verifierIdAddr, err := api.StateLookupID(ctx, verifier, types.EmptyTSK)
		if err != nil {
			return err
		}

		client, err := address.NewFromString(cctx.Args().Get(1))
		if err != nil {
			return err
		}
		clientIdAddr, err := api.StateLookupID(ctx, client, types.EmptyTSK)
		if err != nil {
			return err
		}

		allowanceToRemove, err := types.BigFromString(cctx.Args().Get(2))
		if err != nil {
			return err
		}

		dataCap, err := api.StateVerifiedClientStatus(ctx, clientIdAddr, types.EmptyTSK)
		if err != nil {
			return xerrors.Errorf("failed to find verified client data cap: %w", err)
		}
		if dataCap.LessThanEqual(big.Zero()) {
			return xerrors.Errorf("client data cap %s is less than amount requested to be removed %s", dataCap.String(), allowanceToRemove.String())
		}

		found, _, err := checkNotary(ctx, api, verifier)
		if err != nil {
			return xerrors.Errorf("failed to check notary status: %w", err)
		}

		if !found {
			return xerrors.New("verifier address must be a notary")
		}

		id := cctx.Uint64("id")
		if id == 0 {
			_, id, err = st.RemoveDataCapProposalID(verifierIdAddr, clientIdAddr)
			if err != nil {
				return xerrors.Errorf("failed find remove data cap proposal id: %w", err)
			}
		}

		nv, err := api.StateNetworkVersion(ctx, types.EmptyTSK)
		if err != nil {
			return xerrors.Errorf("failed to get network version: %w", err)
		}

		paramBuf := new(bytes.Buffer)
		paramBuf.WriteString(verifregtypes9.SignatureDomainSeparation_RemoveDataCap)
		if nv <= network.Version16 {
			params := verifregtypes8.RemoveDataCapProposal{
				RemovalProposalID: id,
				DataCapAmount:     allowanceToRemove,
				VerifiedClient:    clientIdAddr,
			}

			err = params.MarshalCBOR(paramBuf)
		} else {
			params := verifregtypes9.RemoveDataCapProposal{
				RemovalProposalID: verifregtypes9.RmDcProposalID{ProposalID: id},
				DataCapAmount:     allowanceToRemove,
				VerifiedClient:    clientIdAddr,
			}

			err = params.MarshalCBOR(paramBuf)
		}
		if err != nil {
			return xerrors.Errorf("failed to marshall paramBuf: %w", err)
		}

		sig, err := api.WalletSign(ctx, verifier, paramBuf.Bytes())
		if err != nil {
			return xerrors.Errorf("failed to sign message: %w", err)
		}

		sigBytes := append([]byte{byte(sig.Type)}, sig.Data...)

		fmt.Println(hex.EncodeToString(sigBytes))

		return nil
	},
}
