package sealing

import (
	"bytes"
	"context"
	"sort"
	"sync"
	"time"

	"github.com/filecoin-project/go-state-types/network"

	"github.com/filecoin-project/lotus/chain/actors"

	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-bitfield"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	miner5 "github.com/filecoin-project/specs-actors/v5/actors/builtin/miner"
	proof5 "github.com/filecoin-project/specs-actors/v5/actors/runtime/proof"

	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/chain/actors/builtin/miner"
	"github.com/filecoin-project/lotus/chain/actors/policy"
	"github.com/filecoin-project/lotus/extern/sector-storage/ffiwrapper"
	"github.com/filecoin-project/lotus/extern/storage-sealing/sealiface"
	"github.com/filecoin-project/lotus/node/config"
)

const arp = abi.RegisteredAggregationProof_SnarkPackV1

type CommitBatcherApi interface {
	SendMsg(ctx context.Context, from, to address.Address, method abi.MethodNum, value, maxFee abi.TokenAmount, params []byte) (cid.Cid, error)
	StateMinerInfo(context.Context, address.Address, TipSetToken) (miner.MinerInfo, error)
	ChainHead(ctx context.Context) (TipSetToken, abi.ChainEpoch, error)
	ChainBaseFee(context.Context, TipSetToken) (abi.TokenAmount, error)

	StateSectorPreCommitInfo(ctx context.Context, maddr address.Address, sectorNumber abi.SectorNumber, tok TipSetToken) (*miner.SectorPreCommitOnChainInfo, error)
	StateMinerInitialPledgeCollateral(context.Context, address.Address, miner.SectorPreCommitInfo, TipSetToken) (big.Int, error)
	StateNetworkVersion(ctx context.Context, tok TipSetToken) (network.Version, error)
}

type AggregateInput struct {
	spt   abi.RegisteredSealProof
	info  proof5.AggregateSealVerifyInfo
	proof []byte
}

type CommitBatcher struct {
	api       CommitBatcherApi
	maddr     address.Address
	mctx      context.Context
	addrSel   AddrSel
	feeCfg    config.MinerFeeConfig
	getConfig GetSealingConfigFunc
	prover    ffiwrapper.Prover

	cutoffs map[abi.SectorNumber]time.Time
	todo    map[abi.SectorNumber]AggregateInput
	waiting map[abi.SectorNumber][]chan sealiface.CommitBatchRes

	notify, stop, stopped chan struct{}
	force                 chan chan []sealiface.CommitBatchRes
	lk                    sync.Mutex
}

func NewCommitBatcher(mctx context.Context, maddr address.Address, api CommitBatcherApi, addrSel AddrSel, feeCfg config.MinerFeeConfig, getConfig GetSealingConfigFunc, prov ffiwrapper.Prover) *CommitBatcher {
	b := &CommitBatcher{
		api:       api,
		maddr:     maddr,
		mctx:      mctx,
		addrSel:   addrSel,
		feeCfg:    feeCfg,
		getConfig: getConfig,
		prover:    prov,

		cutoffs: map[abi.SectorNumber]time.Time{},
		todo:    map[abi.SectorNumber]AggregateInput{},
		waiting: map[abi.SectorNumber][]chan sealiface.CommitBatchRes{},

		notify:  make(chan struct{}, 1),
		force:   make(chan chan []sealiface.CommitBatchRes),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}

	go b.run()

	return b
}

func (b *CommitBatcher) run() {
	var forceRes chan []sealiface.CommitBatchRes
	var lastMsg []sealiface.CommitBatchRes

	cfg, err := b.getConfig()
	if err != nil {
		panic(err)
	}

	for {
		if forceRes != nil {
			forceRes <- lastMsg
			forceRes = nil
		}
		lastMsg = nil

		var sendAboveMax, sendAboveMin bool
		select {
		case <-b.stop:
			close(b.stopped)
			return
		case <-b.notify:
			sendAboveMax = true
		case <-b.batchWait(cfg.CommitBatchWait, cfg.CommitBatchSlack):
			sendAboveMin = true
		case fr := <-b.force: // user triggered
			forceRes = fr
		}

		var err error
		lastMsg, err = b.maybeStartBatch(sendAboveMax, sendAboveMin)
		if err != nil {
			log.Warnw("CommitBatcher processBatch error", "error", err)
		}
	}
}

func (b *CommitBatcher) batchWait(maxWait, slack time.Duration) <-chan time.Time {
	now := time.Now()

	b.lk.Lock()
	defer b.lk.Unlock()

	if len(b.todo) == 0 {
		return nil
	}

	var cutoff time.Time
	for sn := range b.todo {
		sectorCutoff := b.cutoffs[sn]
		if cutoff.IsZero() || (!sectorCutoff.IsZero() && sectorCutoff.Before(cutoff)) {
			cutoff = sectorCutoff
		}
	}
	for sn := range b.waiting {
		sectorCutoff := b.cutoffs[sn]
		if cutoff.IsZero() || (!sectorCutoff.IsZero() && sectorCutoff.Before(cutoff)) {
			cutoff = sectorCutoff
		}
	}

	if cutoff.IsZero() {
		return time.After(maxWait)
	}

	cutoff = cutoff.Add(-slack)
	if cutoff.Before(now) {
		return time.After(time.Nanosecond) // can't return 0
	}

	wait := cutoff.Sub(now)
	if wait > maxWait {
		wait = maxWait
	}

	return time.After(wait)
}

func (b *CommitBatcher) maybeStartBatch(notif, after bool) ([]sealiface.CommitBatchRes, error) {
	b.lk.Lock()
	defer b.lk.Unlock()

	total := len(b.todo)
	if total == 0 {
		return nil, nil // nothing to do
	}

	cfg, err := b.getConfig()
	if err != nil {
		return nil, xerrors.Errorf("getting config: %w", err)
	}

	if notif && total < cfg.MaxCommitBatch {
		return nil, nil
	}

	if after && total < cfg.MinCommitBatch {
		return nil, nil
	}

	var res []sealiface.CommitBatchRes

	if total < cfg.MinCommitBatch || total < miner5.MinAggregatedSectors {
		res, err = b.processIndividually()
	} else {
		res, err = b.processBatch(cfg)
	}
	if err != nil && len(res) == 0 {
		return nil, err
	}

	for _, r := range res {
		if err != nil {
			r.Error = err.Error()
		}

		for _, sn := range r.Sectors {
			for _, ch := range b.waiting[sn] {
				ch <- r // buffered
			}

			delete(b.waiting, sn)
			delete(b.todo, sn)
			delete(b.cutoffs, sn)
		}
	}

	return res, nil
}

func (b *CommitBatcher) processBatch(cfg sealiface.Config) ([]sealiface.CommitBatchRes, error) {
	tok, _, err := b.api.ChainHead(b.mctx)
	if err != nil {
		return nil, err
	}

	total := len(b.todo)

	var res sealiface.CommitBatchRes

	params := miner5.ProveCommitAggregateParams{
		SectorNumbers: bitfield.New(),
	}

	proofs := make([][]byte, 0, total)
	infos := make([]proof5.AggregateSealVerifyInfo, 0, total)
	collateral := big.Zero()

	for id, p := range b.todo {
		if len(infos) >= cfg.MaxCommitBatch {
			log.Infow("commit batch full")
			break
		}

		res.Sectors = append(res.Sectors, id)

		sc, err := b.getSectorCollateral(id, tok)
		if err != nil {
			res.FailedSectors[id] = err.Error()
			continue
		}

		collateral = big.Add(collateral, sc)

		params.SectorNumbers.Set(uint64(id))
		infos = append(infos, p.info)
	}

	sort.Slice(infos, func(i, j int) bool {
		return infos[i].Number < infos[j].Number
	})

	for _, info := range infos {
		proofs = append(proofs, b.todo[info.Number].proof)
	}

	mid, err := address.IDFromAddress(b.maddr)
	if err != nil {
		return []sealiface.CommitBatchRes{res}, xerrors.Errorf("getting miner id: %w", err)
	}

	params.AggregateProof, err = b.prover.AggregateSealProofs(proof5.AggregateSealVerifyProofAndInfos{
		Miner:          abi.ActorID(mid),
		SealProof:      b.todo[infos[0].Number].spt,
		AggregateProof: arp,
		Infos:          infos,
	}, proofs)
	if err != nil {
		return []sealiface.CommitBatchRes{res}, xerrors.Errorf("aggregating proofs: %w", err)
	}

	enc := new(bytes.Buffer)
	if err := params.MarshalCBOR(enc); err != nil {
		return []sealiface.CommitBatchRes{res}, xerrors.Errorf("couldn't serialize ProveCommitAggregateParams: %w", err)
	}

	mi, err := b.api.StateMinerInfo(b.mctx, b.maddr, nil)
	if err != nil {
		return []sealiface.CommitBatchRes{res}, xerrors.Errorf("couldn't get miner info: %w", err)
	}

	maxFee := b.feeCfg.MaxCommitBatchGasFee.FeeForSectors(len(infos))

	bf, err := b.api.ChainBaseFee(b.mctx, tok)
	if err != nil {
		return []sealiface.CommitBatchRes{res}, xerrors.Errorf("couldn't get base fee: %w", err)
	}

	nv, err := b.api.StateNetworkVersion(b.mctx, tok)
	if err != nil {
		log.Errorf("getting network version: %s", err)
		return []sealiface.CommitBatchRes{res}, xerrors.Errorf("getting network version: %s", err)
	}

	aggFee := policy.AggregateNetworkFee(nv, len(infos), bf)

	goodFunds := big.Add(maxFee, big.Add(collateral, aggFee))

	from, _, err := b.addrSel(b.mctx, mi, api.CommitAddr, goodFunds, collateral)
	if err != nil {
		return []sealiface.CommitBatchRes{res}, xerrors.Errorf("no good address found: %w", err)
	}

	mcid, err := b.api.SendMsg(b.mctx, from, b.maddr, miner.Methods.ProveCommitAggregate, collateral, maxFee, enc.Bytes())
	if err != nil {
		return []sealiface.CommitBatchRes{res}, xerrors.Errorf("sending message failed: %w", err)
	}

	res.Msg = &mcid

	log.Infow("Sent ProveCommitAggregate message", "cid", mcid, "from", from, "todo", total, "sectors", len(infos))

	return []sealiface.CommitBatchRes{res}, nil
}

func (b *CommitBatcher) processIndividually() ([]sealiface.CommitBatchRes, error) {
	mi, err := b.api.StateMinerInfo(b.mctx, b.maddr, nil)
	if err != nil {
		return nil, xerrors.Errorf("couldn't get miner info: %w", err)
	}

	tok, _, err := b.api.ChainHead(b.mctx)
	if err != nil {
		return nil, err
	}

	var res []sealiface.CommitBatchRes

	for sn, info := range b.todo {
		r := sealiface.CommitBatchRes{
			Sectors: []abi.SectorNumber{sn},
		}

		mcid, err := b.processSingle(mi, sn, info, tok)
		if err != nil {
			log.Errorf("process single error: %+v", err) // todo: return to user
			r.FailedSectors[sn] = err.Error()
		} else {
			r.Msg = &mcid
		}

		res = append(res, r)
	}

	return res, nil
}

func (b *CommitBatcher) processSingle(mi miner.MinerInfo, sn abi.SectorNumber, info AggregateInput, tok TipSetToken) (cid.Cid, error) {
	enc := new(bytes.Buffer)
	params := &miner.ProveCommitSectorParams{
		SectorNumber: sn,
		Proof:        info.proof,
	}

	if err := params.MarshalCBOR(enc); err != nil {
		return cid.Undef, xerrors.Errorf("marshaling commit params: %w", err)
	}

	collateral, err := b.getSectorCollateral(sn, tok)
	if err != nil {
		return cid.Undef, err
	}

	goodFunds := big.Add(collateral, big.Int(b.feeCfg.MaxCommitGasFee))

	from, _, err := b.addrSel(b.mctx, mi, api.CommitAddr, goodFunds, collateral)
	if err != nil {
		return cid.Undef, xerrors.Errorf("no good address to send commit message from: %w", err)
	}

	mcid, err := b.api.SendMsg(b.mctx, from, b.maddr, miner.Methods.ProveCommitSector, collateral, big.Int(b.feeCfg.MaxCommitGasFee), enc.Bytes())
	if err != nil {
		return cid.Undef, xerrors.Errorf("pushing message to mpool: %w", err)
	}

	return mcid, nil
}

// register commit, wait for batch message, return message CID
func (b *CommitBatcher) AddCommit(ctx context.Context, s SectorInfo, in AggregateInput) (res sealiface.CommitBatchRes, err error) {
	sn := s.SectorNumber

	cu, err := b.getCommitCutoff(s)
	if err != nil {
		return sealiface.CommitBatchRes{}, err
	}

	b.lk.Lock()
	b.cutoffs[sn] = cu
	b.todo[sn] = in

	sent := make(chan sealiface.CommitBatchRes, 1)
	b.waiting[sn] = append(b.waiting[sn], sent)

	select {
	case b.notify <- struct{}{}:
	default: // already have a pending notification, don't need more
	}
	b.lk.Unlock()

	select {
	case r := <-sent:
		return r, nil
	case <-ctx.Done():
		return sealiface.CommitBatchRes{}, ctx.Err()
	}
}

func (b *CommitBatcher) Flush(ctx context.Context) ([]sealiface.CommitBatchRes, error) {
	resCh := make(chan []sealiface.CommitBatchRes, 1)
	select {
	case b.force <- resCh:
		select {
		case res := <-resCh:
			return res, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *CommitBatcher) Pending(ctx context.Context) ([]abi.SectorID, error) {
	b.lk.Lock()
	defer b.lk.Unlock()

	mid, err := address.IDFromAddress(b.maddr)
	if err != nil {
		return nil, err
	}

	res := make([]abi.SectorID, 0)
	for _, s := range b.todo {
		res = append(res, abi.SectorID{
			Miner:  abi.ActorID(mid),
			Number: s.info.Number,
		})
	}

	sort.Slice(res, func(i, j int) bool {
		if res[i].Miner != res[j].Miner {
			return res[i].Miner < res[j].Miner
		}

		return res[i].Number < res[j].Number
	})

	return res, nil
}

func (b *CommitBatcher) Stop(ctx context.Context) error {
	close(b.stop)

	select {
	case <-b.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TODO: If this returned epochs, it would make testing much easier
func (b *CommitBatcher) getCommitCutoff(si SectorInfo) (time.Time, error) {
	tok, curEpoch, err := b.api.ChainHead(b.mctx)
	if err != nil {
		return time.Now(), xerrors.Errorf("getting chain head: %s", err)
	}

	nv, err := b.api.StateNetworkVersion(b.mctx, tok)
	if err != nil {
		log.Errorf("getting network version: %s", err)
		return time.Now(), xerrors.Errorf("getting network version: %s", err)
	}

	pci, err := b.api.StateSectorPreCommitInfo(b.mctx, b.maddr, si.SectorNumber, tok)
	if err != nil {
		log.Errorf("getting precommit info: %s", err)
		return time.Now(), err
	}

	cutoffEpoch := pci.PreCommitEpoch + policy.GetMaxProveCommitDuration(actors.VersionForNetwork(nv), si.SectorType)

	for _, p := range si.Pieces {
		if p.DealInfo == nil {
			continue
		}

		startEpoch := p.DealInfo.DealSchedule.StartEpoch
		if startEpoch < cutoffEpoch {
			cutoffEpoch = startEpoch
		}
	}

	if cutoffEpoch <= curEpoch {
		return time.Now(), nil
	}

	return time.Now().Add(time.Duration(cutoffEpoch-curEpoch) * time.Duration(build.BlockDelaySecs) * time.Second), nil
}

func (b *CommitBatcher) getSectorCollateral(sn abi.SectorNumber, tok TipSetToken) (abi.TokenAmount, error) {
	pci, err := b.api.StateSectorPreCommitInfo(b.mctx, b.maddr, sn, tok)
	if err != nil {
		return big.Zero(), xerrors.Errorf("getting precommit info: %w", err)
	}
	if pci == nil {
		return big.Zero(), xerrors.Errorf("precommit info not found on chain")
	}

	collateral, err := b.api.StateMinerInitialPledgeCollateral(b.mctx, b.maddr, pci.Info, tok)
	if err != nil {
		return big.Zero(), xerrors.Errorf("getting initial pledge collateral: %w", err)
	}

	collateral = big.Sub(collateral, pci.PreCommitDeposit)
	if collateral.LessThan(big.Zero()) {
		collateral = big.Zero()
	}

	return collateral, nil
}
