package simulation

import (
	"context"
	"math"
	"time"

	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/crypto"

	proof5 "github.com/filecoin-project/specs-actors/v5/actors/runtime/proof"

	"github.com/filecoin-project/lotus/chain/actors"
	"github.com/filecoin-project/lotus/chain/actors/aerrors"
	"github.com/filecoin-project/lotus/chain/actors/builtin/miner"
	"github.com/filecoin-project/lotus/chain/types"
)

// postChainCommitInfo returns th
func (sim *Simulation) postChainCommitInfo(ctx context.Context, epoch abi.ChainEpoch) (abi.Randomness, error) {
	commitRand, err := sim.Chainstore.GetChainRandomness(
		ctx, sim.head.Cids(), crypto.DomainSeparationTag_PoStChainCommit, epoch, nil, true)
	return commitRand, err
}

// packWindowPoSts packs window posts until either the block is full or all healty sectors
// have been proven. It does not recover sectors.
func (ss *simulationState) packWindowPoSts(ctx context.Context, cb packFunc) (_err error) {
	// Push any new window posts into the queue.
	if err := ss.queueWindowPoSts(ctx); err != nil {
		return err
	}
	done := 0
	failed := 0
	defer func() {
		if _err != nil {
			return
		}

		log.Debugw("packed window posts",
			"epoch", ss.nextEpoch(),
			"done", done,
			"failed", failed,
			"remaining", len(ss.pendingWposts),
		)
	}()
	// Then pack as many as we can.
	for len(ss.pendingWposts) > 0 {
		next := ss.pendingWposts[0]
		if _, err := cb(next); err != nil {
			if aerr, ok := err.(aerrors.ActorError); !ok || aerr.IsFatal() {
				return err
			}
			log.Errorw("failed to submit windowed post",
				"error", err,
				"miner", next.To,
				"epoch", ss.nextEpoch(),
			)
			failed++
		} else {
			done++
		}

		ss.pendingWposts = ss.pendingWposts[1:]
	}
	ss.pendingWposts = nil
	return nil
}

// stepWindowPoStsMiner enqueues all missing window posts for the current epoch for the given miner.
func (ss *simulationState) stepWindowPoStsMiner(
	ctx context.Context,
	addr address.Address, minerState miner.State,
	commitEpoch abi.ChainEpoch, commitRand abi.Randomness,
) error {

	if active, err := minerState.DeadlineCronActive(); err != nil {
		return err
	} else if !active {
		return nil
	}

	minerInfo, err := ss.getMinerInfo(ctx, addr)
	if err != nil {
		return err
	}

	di, err := minerState.DeadlineInfo(ss.nextEpoch())
	if err != nil {
		return err
	}
	di = di.NextNotElapsed()

	dl, err := minerState.LoadDeadline(di.Index)
	if err != nil {
		return err
	}

	provenBf, err := dl.PartitionsPoSted()
	if err != nil {
		return err
	}
	proven, err := provenBf.AllMap(math.MaxUint64)
	if err != nil {
		return err
	}

	var (
		partitions      []miner.PoStPartition
		partitionGroups [][]miner.PoStPartition
	)
	// Only prove partitions with live sectors.
	err = dl.ForEachPartition(func(idx uint64, part miner.Partition) error {
		if proven[idx] {
			return nil
		}
		// TODO: set this to the actual limit from specs-actors.
		// NOTE: We're mimicing the behavior of wdpost_run.go here.
		if len(partitions) > 0 && idx%4 == 0 {
			partitionGroups = append(partitionGroups, partitions)
			partitions = nil

		}
		live, err := part.LiveSectors()
		if err != nil {
			return err
		}
		liveCount, err := live.Count()
		if err != nil {
			return err
		}
		faulty, err := part.FaultySectors()
		if err != nil {
			return err
		}
		faultyCount, err := faulty.Count()
		if err != nil {
			return err
		}
		if liveCount-faultyCount > 0 {
			partitions = append(partitions, miner.PoStPartition{Index: idx})
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(partitions) > 0 {
		partitionGroups = append(partitionGroups, partitions)
		partitions = nil
	}

	proof, err := mockWpostProof(minerInfo.WindowPoStProofType, addr)
	if err != nil {
		return err
	}
	for _, group := range partitionGroups {
		params := miner.SubmitWindowedPoStParams{
			Deadline:   di.Index,
			Partitions: group,
			Proofs: []proof5.PoStProof{{
				PoStProof:  minerInfo.WindowPoStProofType,
				ProofBytes: proof,
			}},
			ChainCommitEpoch: commitEpoch,
			ChainCommitRand:  commitRand,
		}
		enc, aerr := actors.SerializeParams(&params)
		if aerr != nil {
			return xerrors.Errorf("could not serialize submit window post parameters: %w", aerr)
		}
		msg := &types.Message{
			To:     addr,
			From:   minerInfo.Worker,
			Method: miner.Methods.SubmitWindowedPoSt,
			Params: enc,
			Value:  types.NewInt(0),
		}
		ss.pendingWposts = append(ss.pendingWposts, msg)
	}
	return nil
}

// queueWindowPoSts enqueues missing window posts for all miners with deadlines opening between the
// last epoch in which this function was called and the current epoch (head+1).
func (ss *simulationState) queueWindowPoSts(ctx context.Context) error {
	targetHeight := ss.nextEpoch()

	now := time.Now()
	was := len(ss.pendingWposts)
	count := 0
	defer func() {
		log.Debugw("computed window posts",
			"miners", count,
			"count", len(ss.pendingWposts)-was,
			"duration", time.Since(now),
		)
	}()

	// Perform a bit of catch up. This lets us do things like skip blocks at upgrades then catch
	// up to make the simualtion easier.
	for ; ss.nextWpostEpoch <= targetHeight; ss.nextWpostEpoch++ {
		if ss.nextWpostEpoch+miner.WPoStChallengeWindow < targetHeight {
			log.Warnw("skipping old window post", "epoch", ss.nextWpostEpoch)
			continue
		}
		commitEpoch := ss.nextWpostEpoch - 1
		commitRand, err := ss.postChainCommitInfo(ctx, commitEpoch)
		if err != nil {
			return err
		}

		for _, addr := range ss.wpostPeriods[int(ss.nextWpostEpoch%miner.WPoStChallengeWindow)] {
			_, minerState, err := ss.getMinerState(ctx, addr)
			if err != nil {
				return err
			}
			if err := ss.stepWindowPoStsMiner(ctx, addr, minerState, commitEpoch, commitRand); err != nil {
				return err
			}
			count++
		}

	}
	return nil
}