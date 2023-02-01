package cmd

import (
	"bytes"
	"fmt"
	"github.com/rickiey/sector_penalty/pkg"
	"github.com/spf13/cobra"
	"strconv"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/rickiey/loggo"

	"github.com/filecoin-project/go-state-types/builtin"
	"github.com/filecoin-project/go-state-types/builtin/v9/miner"
	"github.com/filecoin-project/go-state-types/builtin/v9/util/math"
	"github.com/filecoin-project/go-state-types/builtin/v9/util/smoothing"

	"github.com/filecoin-project/go-state-types/builtin/v9/power"
	"github.com/filecoin-project/go-state-types/builtin/v9/reward"
)

func init() {
	rootCmd.AddCommand(sectorTerminatePostCmd)
}

var sectorTerminatePostCmd = &cobra.Command{
	Use:   "sector terminate [miner] [sectorNumber]",
	Short: "Calculates the penalty for terminating a sector",
	Run: func(cmd *cobra.Command, args []string) {
		mineraddr, err := address.NewFromString(args[1])
		if err != nil {
			loggo.Error(err)
			return
		}
		sectorNum, err := strconv.ParseUint(args[2], 10, 64)
		if err != nil {
			loggo.Error(err)
			return
		}
		clacTerminationPenalty(mineraddr, abi.SectorNumber(sectorNum))
	},
}

func clacTerminationPenalty(m address.Address, sectorID abi.SectorNumber) {
	head, err := lapi.ChainHead(ctx)
	if err != nil {
		loggo.Error(err.Error())
		return
	}

	tsk := types.NewTipSetKey(head.Cids()...)

	sectorinfo, err := lapi.StateSectorGetInfo(ctx, m, sectorID, tsk)
	if err != nil {
		loggo.Error(err)
		return
	}

	act, err := lapi.StateGetActor(ctx, builtin.RewardActorAddr, tsk)
	if err != nil {
		loggo.Error(err)
		return
	}
	// resume
	actorHead, err := lapi.ChainReadObj(ctx, act.Head)
	if err != nil {
		loggo.Error(err)
		return
	}

	var rewardActorState reward.State

	if err := rewardActorState.UnmarshalCBOR(bytes.NewReader(actorHead)); err != nil {
		loggo.Error(err)
		return
	}

	actst, err := lapi.StateGetActor(ctx, builtin.StoragePowerActorAddr, tsk)
	if err != nil {
		loggo.Error(err)
		return
	}

	stactorHead, err := lapi.ChainReadObj(ctx, actst.Head)
	if err != nil {
		loggo.Error(err)
		return
	}

	var powerActorState power.State

	if err := powerActorState.UnmarshalCBOR(bytes.NewReader(stactorHead)); err != nil {
		loggo.Error(err)
		return
	}

	fmt.Printf("miner: %s sector ID: %v , start height: %v  expiration height: %v,  ExpectedDayReward: %v \n", m.String(),
		sectorinfo.SectorNumber, sectorinfo.Activation, sectorinfo.Expiration, sectorinfo.ExpectedDayReward)
	penalty := terminationPenalty(ss64GiB, head.Height(), rewardActorState.ThisEpochRewardSmoothed, powerActorState.ThisEpochQAPowerSmoothed, []*miner.SectorOnChainInfo{sectorinfo})

	fmt.Printf("penalty for terminating sector : %v attoFIL about %.10f FIL\n", penalty, pkg.ToFloat64(penalty))

}

func terminationPenalty(sectorSize abi.SectorSize, currEpoch abi.ChainEpoch,
	rewardEstimate, networkQAPowerEstimate smoothing.FilterEstimate, sectors []*miner.SectorOnChainInfo) abi.TokenAmount {
	totalFee := big.Zero()
	for _, s := range sectors {
		sectorPower := QAPowerForSector(sectorSize, s)
		fee := PledgePenaltyForTermination(s.ExpectedDayReward, currEpoch-s.Activation, s.ExpectedStoragePledge,
			networkQAPowerEstimate, sectorPower, rewardEstimate, s.ReplacedDayReward, s.ReplacedSectorAge)
		totalFee = big.Add(fee, totalFee)
	}
	return totalFee
}

// QAPowerForSector The quality-adjusted power for a sector.
func QAPowerForSector(size abi.SectorSize, sector *miner.SectorOnChainInfo) abi.StoragePower {
	duration := sector.Expiration - sector.Activation
	return QAPowerForWeight(size, duration, sector.DealWeight, sector.VerifiedDealWeight)
}

// QAPowerForWeight The power for a sector size, committed duration, and weight.
func QAPowerForWeight(size abi.SectorSize, duration abi.ChainEpoch, dealWeight, verifiedWeight abi.DealWeight) abi.StoragePower {
	quality := QualityForWeight(size, duration, dealWeight, verifiedWeight)
	return big.Rsh(big.Mul(big.NewIntUnsigned(uint64(size)), quality), builtin.SectorQualityPrecision)
}

func QualityForWeight(size abi.SectorSize, duration abi.ChainEpoch, dealWeight, verifiedWeight abi.DealWeight) abi.SectorQuality {
	// sectorSpaceTime = size * duration
	sectorSpaceTime := big.Mul(big.NewIntUnsigned(uint64(size)), big.NewInt(int64(duration)))
	// totalDealSpaceTime = dealWeight + verifiedWeight
	totalDealSpaceTime := big.Add(dealWeight, verifiedWeight)

	// Base - all size * duration of non-deals
	// weightedBaseSpaceTime = (sectorSpaceTime - totalDealSpaceTime) * QualityBaseMultiplier
	weightedBaseSpaceTime := big.Mul(big.Sub(sectorSpaceTime, totalDealSpaceTime), builtin.QualityBaseMultiplier)
	// Deal - all deal size * deal duration * 10
	// weightedDealSpaceTime = dealWeight * DealWeightMultiplier
	weightedDealSpaceTime := big.Mul(dealWeight, builtin.DealWeightMultiplier)
	// Verified - all verified deal size * verified deal duration * 100
	// weightedVerifiedSpaceTime = verifiedWeight * VerifiedDealWeightMultiplier
	weightedVerifiedSpaceTime := big.Mul(verifiedWeight, builtin.VerifiedDealWeightMultiplier)
	// Sum - sum of all spacetime
	// weightedSumSpaceTime = weightedBaseSpaceTime + weightedDealSpaceTime + weightedVerifiedSpaceTime
	weightedSumSpaceTime := big.Sum(weightedBaseSpaceTime, weightedDealSpaceTime, weightedVerifiedSpaceTime)
	// scaledUpWeightedSumSpaceTime = weightedSumSpaceTime * 2^20
	scaledUpWeightedSumSpaceTime := big.Lsh(weightedSumSpaceTime, builtin.SectorQualityPrecision)

	// Average of weighted space time: (scaledUpWeightedSumSpaceTime / sectorSpaceTime * 10)
	return big.Div(big.Div(scaledUpWeightedSumSpaceTime, sectorSpaceTime), builtin.QualityBaseMultiplier)
}

const TerminationLifetimeCap = 140 // PARAM_SPEC
func minEpoch(a, b abi.ChainEpoch) abi.ChainEpoch {
	if a < b {
		return a
	}
	return b
}

var TerminationRewardFactor = builtin.BigFrac{ // PARAM_SPEC
	Numerator:   big.NewInt(1),
	Denominator: big.NewInt(2),
}

func PledgePenaltyForTermination(dayReward abi.TokenAmount, sectorAge abi.ChainEpoch,
	twentyDayRewardAtActivation abi.TokenAmount, networkQAPowerEstimate smoothing.FilterEstimate,
	qaSectorPower abi.StoragePower, rewardEstimate smoothing.FilterEstimate, replacedDayReward abi.TokenAmount,
	replacedSectorAge abi.ChainEpoch) abi.TokenAmount {
	// max(SP(t), BR(StartEpoch, 20d) + BR(StartEpoch, 1d) * terminationRewardFactor * min(SectorAgeInDays, 140))
	// and sectorAgeInDays = sectorAge / EpochsInDay
	lifetimeCap := abi.ChainEpoch(TerminationLifetimeCap) * builtin.EpochsInDay
	cappedSectorAge := minEpoch(sectorAge, lifetimeCap)
	// expected reward for lifetime of new sector (epochs*AttoFIL/day)
	expectedReward := big.Mul(dayReward, big.NewInt(int64(cappedSectorAge)))
	// if lifetime under cap and this sector replaced capacity, add expected reward for old sector's lifetime up to cap
	relevantReplacedAge := minEpoch(replacedSectorAge, lifetimeCap-cappedSectorAge)
	expectedReward = big.Add(expectedReward, big.Mul(replacedDayReward, big.NewInt(int64(relevantReplacedAge))))

	penalizedReward := big.Mul(expectedReward, TerminationRewardFactor.Numerator)

	return big.Max(
		PledgePenaltyForTerminationLowerBound(rewardEstimate, networkQAPowerEstimate, qaSectorPower),
		big.Add(
			twentyDayRewardAtActivation,
			big.Div(
				penalizedReward,
				big.Mul(big.NewInt(builtin.EpochsInDay), TerminationRewardFactor.Denominator)))) // (epochs*AttoFIL/day -> AttoFIL)
}

func PledgePenaltyForTerminationLowerBound(rewardEstimate, networkQAPowerEstimate smoothing.FilterEstimate, qaSectorPower abi.StoragePower) abi.TokenAmount {
	return ExpectedRewardForPower(rewardEstimate, networkQAPowerEstimate, qaSectorPower, TerminationPenaltyLowerBoundProjectionPeriod)
}

var TerminationPenaltyLowerBoundProjectionPeriod = abi.ChainEpoch((builtin.EpochsInDay * 35) / 10) // PARAM_SPEC

func ExpectedRewardForPower(rewardEstimate, networkQAPowerEstimate smoothing.FilterEstimate, qaSectorPower abi.StoragePower, projectionDuration abi.ChainEpoch) abi.TokenAmount {
	networkQAPowerSmoothed := smoothing.Estimate(&networkQAPowerEstimate)
	if networkQAPowerSmoothed.IsZero() {
		return smoothing.Estimate(&rewardEstimate)
	}
	expectedRewardForProvingPeriod := smoothing.ExtrapolatedCumSumOfRatio(projectionDuration, 0, rewardEstimate, networkQAPowerEstimate)
	br128 := big.Mul(qaSectorPower, expectedRewardForProvingPeriod) // Q.0 * Q.128 => Q.128
	br := big.Rsh(br128, math.Precision128)

	return big.Max(br, big.Zero())
}
