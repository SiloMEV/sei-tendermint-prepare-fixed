package consensus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace"
	otrace "go.opentelemetry.io/otel/trace"

	"github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/crypto"
	cstypes "github.com/tendermint/tendermint/internal/consensus/types"
	"github.com/tendermint/tendermint/internal/eventbus"
	"github.com/tendermint/tendermint/internal/jsontypes"
	"github.com/tendermint/tendermint/internal/libs/autofile"
	sm "github.com/tendermint/tendermint/internal/state"
	tmevents "github.com/tendermint/tendermint/libs/events"
	"github.com/tendermint/tendermint/libs/log"
	tmmath "github.com/tendermint/tendermint/libs/math"
	tmos "github.com/tendermint/tendermint/libs/os"
	"github.com/tendermint/tendermint/libs/service"
	tmtime "github.com/tendermint/tendermint/libs/time"
	"github.com/tendermint/tendermint/privval"
	tmgrpc "github.com/tendermint/tendermint/privval/grpc"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
	"github.com/tendermint/tendermint/types"
)

// Consensus sentinel errors
var (
	ErrInvalidProposalSignature   = errors.New("error invalid proposal signature")
	ErrInvalidProposalPOLRound    = errors.New("error invalid proposal POL round")
	ErrAddingVote                 = errors.New("error adding vote")
	ErrSignatureFoundInPastBlocks = errors.New("found signature from the same key")

	errPubKeyIsNotSet = errors.New("pubkey is not set. Look for \"Can't get private validator pubkey\" errors")
)

var msgQueueSize = 1000
var heartbeatIntervalInSecs = 10

// msgs from the reactor which may update the state
type msgInfo struct {
	Msg         Message
	PeerID      types.NodeID
	ReceiveTime time.Time
}

func (msgInfo) TypeTag() string { return "tendermint/wal/MsgInfo" }

type msgInfoJSON struct {
	Msg         json.RawMessage `json:"msg"`
	PeerID      types.NodeID    `json:"peer_key"`
	ReceiveTime time.Time       `json:"receive_time"`
}

func (m msgInfo) MarshalJSON() ([]byte, error) {
	msg, err := jsontypes.Marshal(m.Msg)
	if err != nil {
		return nil, err
	}
	return json.Marshal(msgInfoJSON{Msg: msg, PeerID: m.PeerID, ReceiveTime: m.ReceiveTime})
}

func (m *msgInfo) UnmarshalJSON(data []byte) error {
	var msg msgInfoJSON
	if err := json.Unmarshal(data, &msg); err != nil {
		return err
	}
	if err := jsontypes.Unmarshal(msg.Msg, &m.Msg); err != nil {
		return err
	}
	m.PeerID = msg.PeerID
	return nil
}

// internally generated messages which may update the state
type timeoutInfo struct {
	Duration time.Duration         `json:"duration,string"`
	Height   int64                 `json:"height,string"`
	Round    int32                 `json:"round"`
	Step     cstypes.RoundStepType `json:"step"`
}

func (timeoutInfo) TypeTag() string { return "tendermint/wal/TimeoutInfo" }

func (ti *timeoutInfo) String() string {
	return fmt.Sprintf("%v ; %d/%d %v", ti.Duration, ti.Height, ti.Round, ti.Step)
}

// interface to the mempool
type txNotifier interface {
	TxsAvailable() <-chan struct{}
}

// interface to the evidence pool
type evidencePool interface {
	// reports conflicting votes to the evidence pool to be processed into evidence
	ReportConflictingVotes(voteA, voteB *types.Vote)
}

// State handles execution of the consensus algorithm.
// It processes votes and proposals, and upon reaching agreement,
// commits blocks to the chain and executes them against the application.
// The internal state machine receives input from peers, the internal validator, and from a timer.
type State struct {
	service.BaseService
	logger log.Logger

	// config details
	config            *config.ConsensusConfig
	mempoolConfig     *config.MempoolConfig
	privValidator     types.PrivValidator // for signing votes
	privValidatorType types.PrivValidatorType

	// store blocks and commits
	blockStore sm.BlockStore

	stateStore        sm.Store
	skipBootstrapping bool

	// create and execute blocks
	blockExec *sm.BlockExecutor

	// notify us if txs are available
	txNotifier txNotifier

	// add evidence to the pool
	// when it's detected
	evpool evidencePool

	// internal state
	mtx        sync.RWMutex
	roundState cstypes.SafeRoundState
	state      sm.State // State until height-1.
	// privValidator pubkey, memoized for the duration of one block
	// to avoid extra requests to HSM
	privValidatorPubKey crypto.PubKey

	// state changes may be triggered by: msgs from peers,
	// msgs from ourself, or by timeouts
	peerMsgQueue     chan msgInfo
	internalMsgQueue chan msgInfo
	timeoutTicker    TimeoutTicker

	// information about about added votes and block parts are written on this channel
	// so statistics can be computed by reactor
	statsMsgQueue chan msgInfo

	// we use eventBus to trigger msg broadcasts in the reactor,
	// and to notify external subscribers, eg. through a websocket
	eventBus *eventbus.EventBus

	// a Write-Ahead Log ensures we can recover from any kind of crash
	// and helps us avoid signing conflicting votes
	wal          WAL
	replayMode   bool // so we don't log signing errors during replay
	doWALCatchup bool // determines if we even try to do the catchup

	// for tests where we want to limit the number of transitions the state makes
	nSteps int

	// some functions can be overwritten for testing
	decideProposal func(ctx context.Context, height int64, round int32)
	doPrevote      func(ctx context.Context, height int64, round int32)
	setProposal    func(proposal *types.Proposal, t time.Time) error

	// synchronous pubsub between consensus state and reactor.
	// state only emits EventNewRoundStep, EventValidBlock, and EventVote
	evsw tmevents.EventSwitch

	// for reporting metrics
	metrics *Metrics

	// wait the channel event happening for shutting down the state gracefully
	onStopCh chan *cstypes.RoundState

	tracer                otrace.Tracer
	tracerProviderOptions []trace.TracerProviderOption
	heightSpan            otrace.Span
	heightBeingTraced     int64
	tracingCtx            context.Context
}

// StateOption sets an optional parameter on the State.
type StateOption func(*State)

// SkipStateStoreBootstrap is a state option forces the constructor to
// skip state bootstrapping during construction.
func SkipStateStoreBootstrap(sm *State) {
	sm.skipBootstrapping = true
}

// NewState returns a new State.
func NewState(
	logger log.Logger,
	cfg *config.ConsensusConfig,
	store sm.Store,
	blockExec *sm.BlockExecutor,
	blockStore sm.BlockStore,
	txNotifier txNotifier,
	evpool evidencePool,
	eventBus *eventbus.EventBus,
	traceProviderOps []trace.TracerProviderOption,
	options ...StateOption,
) (*State, error) {
	cs := &State{
		eventBus:         eventBus,
		logger:           logger,
		config:           cfg,
		blockExec:        blockExec,
		blockStore:       blockStore,
		stateStore:       store,
		txNotifier:       txNotifier,
		peerMsgQueue:     make(chan msgInfo, msgQueueSize),
		internalMsgQueue: make(chan msgInfo, msgQueueSize),
		timeoutTicker:    NewTimeoutTicker(logger),
		statsMsgQueue:    make(chan msgInfo, msgQueueSize),
		doWALCatchup:     true,
		wal:              nilWAL{},
		evpool:           evpool,
		evsw:             tmevents.NewEventSwitch(),
		metrics:          NopMetrics(),
		onStopCh:         make(chan *cstypes.RoundState),
	}

	// set function defaults (may be overwritten before calling Start)
	cs.decideProposal = cs.defaultDecideProposal
	cs.doPrevote = cs.defaultDoPrevote
	cs.setProposal = cs.defaultSetProposal

	// NOTE: we do not call scheduleRound0 yet, we do that upon Start()
	cs.BaseService = *service.NewBaseService(logger, "State", cs)
	for _, option := range options {
		option(cs)
	}

	// this is not ideal, but it lets the consensus tests start
	// node-fragments gracefully while letting the nodes
	// themselves avoid this.
	if !cs.skipBootstrapping {
		if err := cs.updateStateFromStore(); err != nil {
			return nil, err
		}
	}

	tp := trace.NewTracerProvider(traceProviderOps...)
	cs.tracer = tp.Tracer("tm-consensus-state")
	cs.tracerProviderOptions = traceProviderOps

	return cs, nil
}

func (cs *State) updateStateFromStore() error {
	state, err := cs.stateStore.Load()
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}
	if state.IsEmpty() {
		return nil
	}

	eq, err := state.Equals(cs.state)
	if err != nil {
		return fmt.Errorf("comparing state: %w", err)
	}
	// if the new state is equivalent to the old state, we should not trigger a state update.
	if eq {
		return nil
	}

	// We have no votes, so reconstruct LastCommit from SeenCommit.
	if state.LastBlockHeight > 0 {
		cs.reconstructLastCommit(state)
	}

	cs.updateToState(state)

	return nil
}

// StateMetrics sets the metrics.
func StateMetrics(metrics *Metrics) StateOption {
	return func(cs *State) { cs.metrics = metrics }
}

// String returns a string.
func (cs *State) String() string {
	// better not to access shared variables
	return "ConsensusState"
}

// GetState returns a copy of the chain state.
func (cs *State) GetState() sm.State {
	cs.mtx.RLock()
	defer cs.mtx.RUnlock()
	return cs.state.Copy()
}

// GetLastHeight returns the last height committed.
// If there were no blocks, returns 0.
func (cs *State) GetLastHeight() int64 {
	return cs.roundState.Height() - 1
}

// GetRoundState returns a shallow copy of the internal consensus state.
func (cs *State) GetRoundState() *cstypes.RoundState {
	rs := cs.roundState.CopyInternal()
	return rs
}

// GetRoundStateJSON returns a json of RoundState.
func (cs *State) GetRoundStateJSON() ([]byte, error) {
	return json.Marshal(*cs.roundState.CopyInternal())
}

// GetRoundStateSimpleJSON returns a json of RoundStateSimple
func (cs *State) GetRoundStateSimpleJSON() ([]byte, error) {
	copy := cs.roundState.CopyInternal()
	return json.Marshal(copy.RoundStateSimple())
}

// GetValidators returns a copy of the current validators.
func (cs *State) GetValidators() (int64, []*types.Validator) {
	cs.mtx.RLock()
	defer cs.mtx.RUnlock()
	return cs.state.LastBlockHeight, cs.state.Validators.Copy().Validators
}

// SetPrivValidator sets the private validator account for signing votes. It
// immediately requests pubkey and caches it.
func (cs *State) SetPrivValidator(ctx context.Context, priv types.PrivValidator) {
	cs.mtx.Lock()
	defer cs.mtx.Unlock()

	cs.privValidator = priv

	if priv != nil {
		switch t := priv.(type) {
		case *privval.RetrySignerClient:
			cs.privValidatorType = types.RetrySignerClient
		case *privval.FilePV:
			cs.privValidatorType = types.FileSignerClient
		case *privval.SignerClient:
			cs.privValidatorType = types.SignerSocketClient
		case *tmgrpc.SignerClient:
			cs.privValidatorType = types.SignerGRPCClient
		case types.MockPV:
			cs.privValidatorType = types.MockSignerClient
		case *types.ErroringMockPV:
			cs.privValidatorType = types.ErrorMockSignerClient
		default:
			cs.logger.Error("unsupported priv validator type", "err",
				fmt.Errorf("error privValidatorType %s", t))
		}
	}

	if err := cs.updatePrivValidatorPubKey(ctx); err != nil {
		cs.logger.Error("failed to get private validator pubkey", "err", err)
	}
}

// SetTimeoutTicker sets the local timer. It may be useful to overwrite for
// testing.
func (cs *State) SetTimeoutTicker(timeoutTicker TimeoutTicker) {
	cs.mtx.Lock()
	cs.timeoutTicker = timeoutTicker
	cs.mtx.Unlock()
}

// LoadCommit loads the commit for a given height.
func (cs *State) LoadCommit(height int64) *types.Commit {
	cs.mtx.RLock()
	defer cs.mtx.RUnlock()

	if height == cs.blockStore.Height() {
		commit := cs.blockStore.LoadSeenCommit()
		// NOTE: Retrieving the height of the most recent block and retrieving
		// the most recent commit does not currently occur as an atomic
		// operation. We check the height and commit here in case a more recent
		// commit has arrived since retrieving the latest height.
		if commit != nil && commit.Height == height {
			return commit
		}
	}

	return cs.blockStore.LoadBlockCommit(height)
}

// OnStart loads the latest state via the WAL, and starts the timeout and
// receive routines.
func (cs *State) OnStart(ctx context.Context) error {
	if err := cs.updateStateFromStore(); err != nil {
		return err
	}

	// We may set the WAL in testing before calling Start, so only OpenWAL if its
	// still the nilWAL.
	if _, ok := cs.wal.(nilWAL); ok {
		if err := cs.loadWalFile(ctx); err != nil {
			return err
		}
	}

	// we need the timeoutRoutine for replay so
	// we don't block on the tick chan.
	// NOTE: we will get a build up of garbage go routines
	// firing on the tockChan until the receiveRoutine is started
	// to deal with them (by that point, at most one will be valid)
	if err := cs.timeoutTicker.Start(ctx); err != nil {
		return err
	}

	// We may have lost some votes if the process crashed reload from consensus
	// log to catchup.
	if cs.doWALCatchup {
		repairAttempted := false

	LOOP:
		for {
			err := cs.catchupReplay(ctx, cs.roundState.Height())
			switch {
			case err == nil:
				break LOOP

			case !IsDataCorruptionError(err):
				cs.logger.Error("error on catchup replay; proceeding to start state anyway", "err", err)
				break LOOP

			case repairAttempted:
				return err
			}

			cs.logger.Error("the WAL file is corrupted; attempting repair", "err", err)

			// 1) prep work
			cs.wal.Stop()

			repairAttempted = true

			// 2) backup original WAL file
			corruptedFile := fmt.Sprintf("%s.CORRUPTED", cs.config.WalFile())
			if err := tmos.CopyFile(cs.config.WalFile(), corruptedFile); err != nil {
				return err
			}

			cs.logger.Debug("backed up WAL file", "src", cs.config.WalFile(), "dst", corruptedFile)

			// 3) try to repair (WAL file will be overwritten!)
			if err := repairWalFile(corruptedFile, cs.config.WalFile()); err != nil {
				cs.logger.Error("the WAL repair failed", "err", err)
				return err
			}

			cs.logger.Info("successful WAL repair")

			// reload WAL file
			if err := cs.loadWalFile(ctx); err != nil {
				return err
			}
		}
	}

	// Double Signing Risk Reduction
	if err := cs.checkDoubleSigningRisk(cs.roundState.Height()); err != nil {
		return err
	}

	// now start the receiveRoutine
	go cs.receiveRoutine(ctx, 0)
	// start heartbeater
	go cs.heartbeater(ctx)

	// schedule the first round!
	// use GetRoundState so we don't race the receiveRoutine for access
	cs.scheduleRound0(cs.GetRoundState())

	return nil
}

// timeoutRoutine: receive requests for timeouts on tickChan and fire timeouts on tockChan
// receiveRoutine: serializes processing of proposoals, block parts, votes; coordinates state transitions
//
// this is only used in tests.
func (cs *State) startRoutines(ctx context.Context, maxSteps int) {
	err := cs.timeoutTicker.Start(ctx)
	if err != nil {
		cs.logger.Error("failed to start timeout ticker", "err", err)
		return
	}

	go cs.receiveRoutine(ctx, maxSteps)
}

// loadWalFile loads WAL data from file. It overwrites cs.wal.
func (cs *State) loadWalFile(ctx context.Context) error {
	wal, err := cs.OpenWAL(ctx, cs.config.WalFile())
	if err != nil {
		cs.logger.Error("failed to load state WAL", "err", err)
		return err
	}

	cs.wal = wal
	return nil
}

func (cs *State) getOnStopCh() chan *cstypes.RoundState {
	cs.mtx.RLock()
	defer cs.mtx.RUnlock()

	return cs.onStopCh
}

// OnStop implements service.Service.
func (cs *State) OnStop() {
	// If the node is committing a new block, wait until it is finished!
	if cs.GetRoundState().Step == cstypes.RoundStepCommit {
		cs.mtx.RLock()
		commitTimeout := cs.state.ConsensusParams.Timeout.Commit
		cs.mtx.RUnlock()
		select {
		case <-cs.getOnStopCh():
		case <-time.After(commitTimeout):
			cs.logger.Error("OnStop: timeout waiting for commit to finish", "time", commitTimeout)
		}
	}

	if cs.timeoutTicker.IsRunning() {
		cs.timeoutTicker.Stop()
	}
	// WAL is stopped in receiveRoutine.
}

// OpenWAL opens a file to log all consensus messages and timeouts for
// deterministic accountability.
func (cs *State) OpenWAL(ctx context.Context, walFile string) (WAL, error) {
	wal, err := NewWAL(ctx, cs.logger.With("wal", walFile), walFile)
	if err != nil {
		cs.logger.Error("failed to open WAL", "file", walFile, "err", err)
		return nil, err
	}

	if err := wal.Start(ctx); err != nil {
		cs.logger.Error("failed to start WAL", "err", err)
		return nil, err
	}

	return wal, nil
}

//------------------------------------------------------------
// Public interface for passing messages into the consensus state, possibly causing a state transition.
// If peerID == "", the msg is considered internal.
// Messages are added to the appropriate queue (peer or internal).
// If the queue is full, the function may block.
// TODO: should these return anything or let callers just use events?

// AddVote inputs a vote.
func (cs *State) AddVote(ctx context.Context, vote *types.Vote, peerID types.NodeID) error {
	if peerID == "" {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case cs.internalMsgQueue <- msgInfo{&VoteMessage{vote}, "", tmtime.Now()}:
			return nil
		}
	} else {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case cs.peerMsgQueue <- msgInfo{&VoteMessage{vote}, peerID, tmtime.Now()}:
			return nil
		}
	}

	// TODO: wait for event?!
}

// SetProposal inputs a proposal.
func (cs *State) SetProposal(ctx context.Context, proposal *types.Proposal, peerID types.NodeID) error {

	if peerID == "" {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case cs.internalMsgQueue <- msgInfo{&ProposalMessage{proposal}, "", tmtime.Now()}:
			return nil
		}
	} else {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case cs.peerMsgQueue <- msgInfo{&ProposalMessage{proposal}, peerID, tmtime.Now()}:
			return nil
		}
	}

	// TODO: wait for event?!
}

// AddProposalBlockPart inputs a part of the proposal block.
func (cs *State) AddProposalBlockPart(ctx context.Context, height int64, round int32, part *types.Part, peerID types.NodeID) error {
	if peerID == "" {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case cs.internalMsgQueue <- msgInfo{&BlockPartMessage{height, round, part}, "", tmtime.Now()}:
			return nil
		}
	} else {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case cs.peerMsgQueue <- msgInfo{&BlockPartMessage{height, round, part}, peerID, tmtime.Now()}:
			return nil
		}
	}

	// TODO: wait for event?!
}

// SetProposalAndBlock inputs the proposal and all block parts.
func (cs *State) SetProposalAndBlock(
	ctx context.Context,
	proposal *types.Proposal,
	block *types.Block,
	parts *types.PartSet,
	peerID types.NodeID,
) error {

	if err := cs.SetProposal(ctx, proposal, peerID); err != nil {
		return err
	}

	for i := 0; i < int(parts.Total()); i++ {
		part := parts.GetPart(i)
		if err := cs.AddProposalBlockPart(ctx, proposal.Height, proposal.Round, part, peerID); err != nil {
			return err
		}
	}

	return nil
}

//------------------------------------------------------------
// internal functions for managing the state

func (cs *State) updateHeight(height int64) {
	cs.metrics.Height.Set(float64(height))
	cs.metrics.ClearStepMetrics()
	cs.roundState.SetHeight(height)
}

func (cs *State) updateRoundStep(round int32, step cstypes.RoundStepType) {
	if !cs.replayMode {
		if round != cs.roundState.Round() || round == 0 && step == cstypes.RoundStepNewRound {
			cs.metrics.MarkRound(cs.roundState.Round(), cs.roundState.StartTime())
		}
		if cs.roundState.Step() != step {
			cs.metrics.MarkStep(cs.roundState.Step())
		}
	}
	cs.roundState.SetRound(round)
	cs.roundState.SetStep(step)
}

// enterNewRound(height, 0) at cs.StartTime.
func (cs *State) scheduleRound0(rs *cstypes.RoundState) {
	// cs.logger.Info("scheduleRound0", "now", tmtime.Now(), "startTime", cs.StartTime)
	sleepDuration := rs.StartTime.Sub(tmtime.Now())
	cs.scheduleTimeout(sleepDuration, rs.Height, 0, cstypes.RoundStepNewHeight)
}

// Attempt to schedule a timeout (by sending timeoutInfo on the tickChan)
func (cs *State) scheduleTimeout(duration time.Duration, height int64, round int32, step cstypes.RoundStepType) {
	cs.timeoutTicker.ScheduleTimeout(timeoutInfo{duration, height, round, step})
}

// send a msg into the receiveRoutine regarding our own proposal, block part, or vote
func (cs *State) sendInternalMessage(ctx context.Context, mi msgInfo) {
	select {
	case <-ctx.Done():
	case cs.internalMsgQueue <- mi:
	default:
		// NOTE: using the go-routine means our votes can
		// be processed out of order.
		// TODO: use CList here for strict determinism and
		// attempt push to internalMsgQueue in receiveRoutine
		cs.logger.Debug("internal msg queue is full; using a go-routine")
		go func() {
			select {
			case <-ctx.Done():
			case cs.internalMsgQueue <- mi:
			}
		}()
	}
}

// Reconstruct the LastCommit from either SeenCommit or the ExtendedCommit. SeenCommit
// and ExtendedCommit are saved along with the block. If VoteExtensions are required
// the method will panic on an absent ExtendedCommit or an ExtendedCommit without
// extension data.
func (cs *State) reconstructLastCommit(state sm.State) {
	extensionsEnabled := cs.state.ConsensusParams.ABCI.VoteExtensionsEnabled(state.LastBlockHeight)
	if !extensionsEnabled {
		votes, err := cs.votesFromSeenCommit(state)
		if err != nil {
			panic(fmt.Sprintf("failed to reconstruct last commit; %s", err))
		}
		cs.roundState.SetLastCommit(votes)
		return
	}

	votes, err := cs.votesFromExtendedCommit(state)
	if err != nil {
		panic(fmt.Sprintf("failed to reconstruct last extended commit; %s", err))
	}
	cs.roundState.SetLastCommit(votes)
}

func (cs *State) votesFromExtendedCommit(state sm.State) (*types.VoteSet, error) {
	ec := cs.blockStore.LoadBlockExtendedCommit(state.LastBlockHeight)
	if ec == nil {
		return nil, fmt.Errorf("extended commit for height %v not found", state.LastBlockHeight)
	}
	vs := ec.ToExtendedVoteSet(state.ChainID, state.LastValidators)
	if !vs.HasTwoThirdsMajority() {
		return nil, errors.New("extended commit does not have +2/3 majority")
	}
	return vs, nil
}

func (cs *State) votesFromSeenCommit(state sm.State) (*types.VoteSet, error) {
	commit := cs.blockStore.LoadSeenCommit()
	if commit == nil || commit.Height != state.LastBlockHeight {
		commit = cs.blockStore.LoadBlockCommit(state.LastBlockHeight)
	}
	if commit == nil {
		return nil, fmt.Errorf("commit for height %v not found", state.LastBlockHeight)
	}

	vs := commit.ToVoteSet(state.ChainID, state.LastValidators)
	if !vs.HasTwoThirdsMajority() {
		return nil, errors.New("commit does not have +2/3 majority")
	}
	return vs, nil
}

// Updates State and increments height to match that of state.
// The round becomes 0 and cs.Step becomes cstypes.RoundStepNewHeight.
func (cs *State) updateToState(state sm.State) {
	if cs.roundState.CommitRound() > -1 && 0 < cs.roundState.Height() && cs.roundState.Height() != state.LastBlockHeight {
		panic(fmt.Sprintf(
			"updateToState() expected state height of %v but found %v",
			cs.roundState.Height(), state.LastBlockHeight,
		))
	}

	if !cs.state.IsEmpty() {
		if cs.state.LastBlockHeight > 0 && cs.state.LastBlockHeight+1 != cs.roundState.Height() {
			// This might happen when someone else is mutating cs.state.
			// Someone forgot to pass in state.Copy() somewhere?!
			panic(fmt.Sprintf(
				"inconsistent cs.state.LastBlockHeight+1 %v vs cs.Height %v",
				cs.state.LastBlockHeight+1, cs.roundState.Height(),
			))
		}
		if cs.state.LastBlockHeight > 0 && cs.roundState.Height() == cs.state.InitialHeight {
			panic(fmt.Sprintf(
				"inconsistent cs.state.LastBlockHeight %v, expected 0 for initial height %v",
				cs.state.LastBlockHeight, cs.state.InitialHeight,
			))
		}

		// If state isn't further out than cs.state, just ignore.
		// This happens when SwitchToConsensus() is called in the reactor.
		// We don't want to reset e.g. the Votes, but we still want to
		// signal the new round step, because other services (eg. txNotifier)
		// depend on having an up-to-date peer state!
		if state.LastBlockHeight <= cs.state.LastBlockHeight {
			cs.logger.Debug(
				"ignoring updateToState()",
				"new_height", state.LastBlockHeight+1,
				"old_height", cs.state.LastBlockHeight+1,
			)
			cs.newStep()
			return
		}
	}

	// Reset fields based on state.
	validators := state.Validators

	switch {
	case state.LastBlockHeight == 0: // Very first commit should be empty.
		cs.roundState.SetLastCommit((*types.VoteSet)(nil))
	case cs.roundState.CommitRound() > -1 && cs.roundState.Votes() != nil: // Otherwise, use cs.Votes
		if !cs.roundState.Votes().Precommits(cs.roundState.CommitRound()).HasTwoThirdsMajority() {
			panic(fmt.Sprintf(
				"wanted to form a commit, but precommits (H/R: %d/%d) didn't have 2/3+: %v",
				state.LastBlockHeight, cs.roundState.CommitRound(), cs.roundState.Votes().Precommits(cs.roundState.CommitRound()),
			))
		}

		cs.roundState.SetLastCommit(cs.roundState.Votes().Precommits(cs.roundState.CommitRound()))

	case cs.roundState.LastCommit() == nil:
		// NOTE: when Tendermint starts, it has no votes. reconstructLastCommit
		// must be called to reconstruct LastCommit from SeenCommit.
		panic(fmt.Sprintf(
			"last commit cannot be empty after initial block (H:%d)",
			state.LastBlockHeight+1,
		))
	}

	// Next desired block height
	height := state.LastBlockHeight + 1
	if height == 1 {
		height = state.InitialHeight
	}

	// RoundState fields
	cs.updateHeight(height)
	cs.updateRoundStep(0, cstypes.RoundStepNewHeight)

	if cs.roundState.CommitTime().IsZero() {
		// "Now" makes it easier to sync up dev nodes.
		// We add timeoutCommit to allow transactions
		// to be gathered for the first block.
		// And alternative solution that relies on clocks:
		// cs.StartTime = state.LastBlockTime.Add(timeoutCommit)
		cs.roundState.SetStartTime(cs.commitTime(tmtime.Now()))
	} else {
		cs.roundState.SetStartTime(cs.commitTime(cs.roundState.CommitTime()))
	}

	cs.roundState.SetValidators(validators)
	cs.roundState.SetProposal(nil)
	cs.roundState.SetProposalReceiveTime(time.Time{})
	cs.roundState.SetProposalBlock(nil)
	cs.roundState.SetProposalBlockParts(nil)
	cs.roundState.SetLockedRound(-1)
	cs.roundState.SetLockedBlock(nil)
	cs.roundState.SetLockedBlockParts(nil)
	cs.roundState.SetValidRound(-1)
	cs.roundState.SetValidBlock(nil)
	cs.roundState.SetValidBlockParts(nil)
	if state.ConsensusParams.ABCI.VoteExtensionsEnabled(height) {
		cs.roundState.SetVotes(cstypes.NewExtendedHeightVoteSet(state.ChainID, height, validators))
	} else {
		cs.roundState.SetVotes(cstypes.NewHeightVoteSet(state.ChainID, height, validators))
	}
	cs.roundState.SetCommitRound(-1)
	cs.roundState.SetLastValidators(state.LastValidators)
	cs.roundState.SetTriggeredTimeoutPrecommit(false)

	cs.state = state

	// Finally, broadcast RoundState
	cs.newStep()
}

func (cs *State) newStep() {
	rs := cs.roundState.RoundStateEvent()
	if err := cs.wal.Write(rs); err != nil {
		cs.logger.Error("failed writing to WAL", "err", err)
	}

	cs.nSteps++

	// newStep is called by updateToState in NewState before the eventBus is set!
	if cs.eventBus != nil {
		if err := cs.eventBus.PublishEventNewRoundStep(rs); err != nil {
			cs.logger.Error("failed publishing new round step", "err", err)
		}

		roundState := cs.roundState.CopyInternal()
		cs.evsw.FireEvent(types.EventNewRoundStepValue, roundState)
	}
}

func (cs *State) heartbeater(ctx context.Context) {
	for {
		select {
		case <-time.After(time.Duration(heartbeatIntervalInSecs) * time.Second):
			cs.fireHeartbeatEvent()
		case <-ctx.Done():
			return
		}
	}
}

func (cs *State) fireHeartbeatEvent() {
	roundState := cs.roundState.CopyInternal()
	cs.evsw.FireEvent(types.EventNewRoundStepValue, roundState)
}

//-----------------------------------------
// the main go routines

// receiveRoutine handles messages which may cause state transitions.
// it's argument (n) is the number of messages to process before exiting - use 0 to run forever
// It keeps the RoundState and is the only thing that updates it.
// Updates (state transitions) happen on timeouts, complete proposals, and 2/3 majorities.
// State must be locked before any internal state is updated.
func (cs *State) receiveRoutine(ctx context.Context, maxSteps int) {
	onExit := func(cs *State) {
		// NOTE: the internalMsgQueue may have signed messages from our
		// priv_val that haven't hit the WAL, but its ok because
		// priv_val tracks LastSig

		// close wal now that we're done writing to it
		cs.wal.Stop()
		cs.wal.Wait()
	}

	defer func() {
		if r := recover(); r != nil {
			cs.logger.Error("CONSENSUS FAILURE!!!", "err", r, "stack", string(debug.Stack()))

			// Make a best-effort attempt to close the WAL, but otherwise do not
			// attempt to gracefully terminate. Once consensus has irrecoverably
			// failed, any additional progress we permit the node to make may
			// complicate diagnosing and recovering from the failure.
			onExit(cs)

			// There are a couple of cases where the we
			// panic with an error from deeper within the
			// state machine and in these cases, typically
			// during a normal shutdown, we can continue
			// with normal shutdown with safety. These
			// cases are:
			if err, ok := r.(error); ok {
				// TODO(creachadair): In ordinary operation, the WAL autofile should
				// never be closed. This only happens during shutdown and production
				// nodes usually halt by panicking. Many existing tests, however,
				// assume a clean shutdown is possible. Prior to #8111, we were
				// swallowing the panic in receiveRoutine, making that appear to
				// work. Filtering this specific error is slightly risky, but should
				// affect only unit tests. In any case, not re-panicking here only
				// preserves the pre-existing behavior for this one error type.
				if errors.Is(err, autofile.ErrAutoFileClosed) {
					return
				}

				// don't re-panic if the panic is just an
				// error and we're already trying to shut down
				if ctx.Err() != nil {
					return

				}
			}

			// Re-panic to ensure the node terminates.
			//
			panic(r)
		}
	}()

	for {
		if maxSteps > 0 {
			if cs.nSteps >= maxSteps {
				cs.logger.Debug("reached max steps; exiting receive routine")
				cs.nSteps = 0
				return
			}
		}

		select {
		case <-cs.txNotifier.TxsAvailable():
			cs.handleTxsAvailable(ctx)

		case mi := <-cs.peerMsgQueue:
			if err := cs.wal.Write(mi); err != nil {
				cs.logger.Error("failed writing to WAL", "err", err)
			}
			// handles proposals, block parts, votes
			// may generate internal events (votes, complete proposals, 2/3 majorities)
			cs.handleMsg(ctx, mi, false)

		case mi := <-cs.internalMsgQueue:
			err := cs.wal.Write(mi)
			if err != nil {
				panic(fmt.Errorf(
					"failed to write %v msg to consensus WAL due to %w; check your file system and restart the node",
					mi, err,
				))
			}

			// handles proposals, block parts, votes
			cs.handleMsg(ctx, mi, true)

		case ti := <-cs.timeoutTicker.Chan(): // tockChan:
			if err := cs.wal.Write(ti); err != nil {
				cs.logger.Error("failed writing to WAL", "err", err)
			}

			// if the timeout is relevant to the rs
			// go to the next step
			cs.handleTimeout(ctx, ti, *cs.roundState.CopyInternal())

		case <-ctx.Done():
			onExit(cs)
			return

		}
		// TODO should we handle context cancels here?
	}
}
func (cs *State) fsyncAndCompleteProposal(ctx context.Context, fsyncUponCompletion bool, height int64, span otrace.Span, onPropose bool) {
	cs.metrics.ProposalBlockCreatedOnPropose.With("success", strconv.FormatBool(onPropose)).Add(1)
	if fsyncUponCompletion {
		if err := cs.wal.FlushAndSync(); err != nil { // fsync
			cs.logger.Error("Error flushing wal after receiving all block parts", "error", err)
		}
	}
	cs.metrics.CompleteProposalTime.Observe(float64(time.Since(cs.roundState.ProposalReceiveTime())))
	cs.handleCompleteProposal(ctx, height, span)
}

// We only used tx key based dissemination if configured to do so and we are a validator
func (cs *State) gossipTransactionKeyOnly() bool {
	return cs.config.GossipTransactionKeyOnly && cs.privValidatorPubKey != nil
}

// state transitions on complete-proposal, 2/3-any, 2/3-one
func (cs *State) handleMsg(ctx context.Context, mi msgInfo, fsyncUponCompletion bool) {
	cs.mtx.Lock()
	defer cs.mtx.Unlock()
	var (
		added bool
		err   error
	)

	cs.metrics.MarkStepLatency(cs.roundState.Step())

	msg, peerID := mi.Msg, mi.PeerID

	switch msg := msg.(type) {
	case *ProposalMessage:
		spanCtx, span := cs.tracer.Start(cs.getTracingCtx(ctx), "cs.state.handleProposalMsg")
		span.SetAttributes(attribute.Int("round", int(msg.Proposal.Round)))
		defer span.End()

		// will not cause transition.
		// once proposal is set, we can receive block parts
		if err = cs.setProposal(msg.Proposal, mi.ReceiveTime); err == nil {
			if cs.gossipTransactionKeyOnly() {
				isProposer := cs.isProposer(cs.privValidatorPubKey.Address())
				if !isProposer && cs.roundState.ProposalBlock() == nil {
					created := cs.tryCreateProposalBlock(spanCtx, msg.Proposal.Height, msg.Proposal.Round, msg.Proposal.Header, msg.Proposal.LastCommit, msg.Proposal.Evidence, msg.Proposal.ProposerAddress)
					if created {
						cs.fsyncAndCompleteProposal(ctx, fsyncUponCompletion, msg.Proposal.Height, span, true)
					}
				}
			}
		}

	case *BlockPartMessage:
		// If we have already created block parts, we can exit early if block part matches
		if cs.config.GossipTransactionKeyOnly && cs.roundState.Proposal() != nil && cs.roundState.ProposalBlockParts() != nil {
			// Check hash proof matches. If so, we can return
			if msg.Part.Proof.Verify(cs.roundState.ProposalBlockParts().Hash(), msg.Part.Bytes) != nil {
				return
			}
		}
		_, span := cs.tracer.Start(cs.getTracingCtx(ctx), "cs.state.handleBlockPartMsg")
		span.SetAttributes(attribute.Int("round", int(msg.Round)))
		defer span.End()

		// if the proposal is complete, we'll enterPrevote or tryFinalizeCommit
		added, err = cs.addProposalBlockPart(msg, peerID)
		// We unlock here to yield to any routines that need to read the the RoundState.
		// Previously, this code held the lock from the point at which the final block
		// part was received until the block executed against the application.
		// This prevented the reactor from being able to retrieve the most updated
		// version of the RoundState. The reactor needs the updated RoundState to
		// gossip the now completed block.
		//
		// This code can be further improved by either always operating on a copy
		// of RoundState and only locking when switching out State's copy of
		// RoundState with the updated copy or by emitting RoundState events in
		// more places for routines depending on it to listen for.
		cs.mtx.Unlock()

		cs.mtx.Lock()
		if added && cs.roundState.ProposalBlockParts().IsComplete() {
			cs.fsyncAndCompleteProposal(ctx, fsyncUponCompletion, msg.Height, span, false)
		}
		if added {
			select {
			case cs.statsMsgQueue <- mi:
			case <-ctx.Done():
				return
			}
		}

		if err != nil && msg.Round != cs.roundState.Round() {
			cs.logger.Debug(
				"received block part from wrong round",
				"height", cs.roundState.Height(),
				"cs_round", cs.roundState.Round(),
				"block_round", msg.Round,
			)
			err = nil
		} else if err != nil {
			cs.logger.Debug("added block part but received error", "error", err, "height", cs.roundState.Height(), "cs_round", cs.roundState.Round(), "block_round", msg.Round)
		}

	case *VoteMessage:
		_, span := cs.tracer.Start(cs.getTracingCtx(ctx), "cs.state.handleVoteMsg")
		span.SetAttributes(attribute.Int("round", int(msg.Vote.Round)))
		defer span.End()

		// attempt to add the vote and dupeout the validator if its a duplicate signature
		// if the vote gives us a 2/3-any or 2/3-one, we transition
		added, err = cs.tryAddVote(ctx, msg.Vote, peerID, span)
		if added {
			select {
			case cs.statsMsgQueue <- mi:
			case <-ctx.Done():
				return
			}
		}

		// TODO: punish peer
		// We probably don't want to stop the peer here. The vote does not
		// necessarily comes from a malicious peer but can be just broadcasted by
		// a typical peer.
		// https://github.com/tendermint/tendermint/issues/1281

		// NOTE: the vote is broadcast to peers by the reactor listening
		// for vote events

		// TODO: If rs.Height == vote.Height && rs.Round < vote.Round,
		// the peer is sending us CatchupCommit precommits.
		// We could make note of this and help filter in broadcastHasVoteMessage().

	default:
		cs.logger.Error("unknown msg type", "type", fmt.Sprintf("%T", msg))
		return
	}

	if err != nil {
		cs.logger.Error(
			"failed to process message",
			"height", cs.roundState.Height(),
			"round", cs.roundState.Round(),
			"peer", peerID,
			"msg_type", fmt.Sprintf("%T", msg),
			"err", err,
		)
	}
	return
}

func (cs *State) handleTimeout(
	ctx context.Context,
	ti timeoutInfo,
	rs cstypes.RoundState,
) {
	cs.logger.Debug("received tock", "timeout", ti.Duration, "height", ti.Height, "round", ti.Round, "step", ti.Step)

	// timeouts must be for current height, round, step
	if ti.Height != rs.Height || ti.Round < rs.Round || (ti.Round == rs.Round && ti.Step < rs.Step) {
		cs.logger.Debug("ignoring tock because we are ahead", "height", rs.Height, "round", rs.Round, "step", rs.Step)
		return
	}

	// the timeout will now cause a state transition
	cs.mtx.Lock()
	defer cs.mtx.Unlock()
	cs.metrics.MarkStepLatency(rs.Step)

	switch ti.Step {
	case cstypes.RoundStepNewHeight:
		// NewRound event fired from enterNewRound.
		// XXX: should we fire timeout here (for timeout commit)?
		cs.enterNewRound(ctx, ti.Height, 0, "timeout")

	case cstypes.RoundStepNewRound:
		cs.enterPropose(ctx, ti.Height, 0, "timeout")

	case cstypes.RoundStepPropose:
		if err := cs.eventBus.PublishEventTimeoutPropose(cs.roundState.RoundStateEvent()); err != nil {
			cs.logger.Error("failed publishing timeout propose", "err", err)
		}

		cs.enterPrevote(ctx, ti.Height, ti.Round, "timeout")

	case cstypes.RoundStepPrevoteWait:
		if err := cs.eventBus.PublishEventTimeoutWait(cs.roundState.RoundStateEvent()); err != nil {
			cs.logger.Error("failed publishing timeout wait", "err", err)
		}

		cs.enterPrecommit(ctx, ti.Height, ti.Round, "timeout")

	case cstypes.RoundStepPrecommitWait:
		if err := cs.eventBus.PublishEventTimeoutWait(cs.roundState.RoundStateEvent()); err != nil {
			cs.logger.Error("failed publishing timeout wait", "err", err)
		}

		cs.enterPrecommit(ctx, ti.Height, ti.Round, "precommit-wait-timeout")
		cs.enterNewRound(ctx, ti.Height, ti.Round+1, "precommit-wait-timeout")

	default:
		panic(fmt.Sprintf("invalid timeout step: %v", ti.Step))
	}

	return
}

func (cs *State) handleTxsAvailable(ctx context.Context) {
	cs.mtx.Lock()
	defer cs.mtx.Unlock()

	// We only need to do this for round 0.
	if cs.roundState.Round() != 0 {
		return
	}

	switch cs.roundState.Step() {
	case cstypes.RoundStepNewHeight: // timeoutCommit phase
		if cs.needProofBlock(cs.roundState.Height()) {
			// enterPropose will be called by enterNewRound
			return
		}

		// +1ms to ensure RoundStepNewRound timeout always happens after RoundStepNewHeight
		timeoutCommit := cs.roundState.StartTime().Sub(tmtime.Now()) + 1*time.Millisecond
		cs.scheduleTimeout(timeoutCommit, cs.roundState.Height(), 0, cstypes.RoundStepNewRound)

	case cstypes.RoundStepNewRound: // after timeoutCommit
		cs.enterPropose(ctx, cs.roundState.Height(), 0, "post-timeout-commit")
	}
	return
}

func (cs *State) getTracingCtx(defaultCtx context.Context) context.Context {
	if cs.tracingCtx != nil {
		return cs.tracingCtx
	}
	return defaultCtx
}

//-----------------------------------------------------------------------------
// State functions
// Used internally by handleTimeout and handleMsg to make state transitions

// Enter: `timeoutNewHeight` by startTime (commitTime+timeoutCommit),
//
//	or, if SkipTimeoutCommit==true, after receiving all precommits from (height,round-1)
//
// Enter: `timeoutPrecommits` after any +2/3 precommits from (height,round-1)
// Enter: +2/3 precommits for nil at (height,round-1)
// Enter: +2/3 prevotes any or +2/3 precommits for block or any from (height, round)
// NOTE: cs.StartTime was already set for height.
func (cs *State) enterNewRound(ctx context.Context, height int64, round int32, entryLabel string) {
	if height > cs.heightBeingTraced {
		if cs.heightSpan != nil {
			cs.heightSpan.End()
		}
		cs.heightBeingTraced = height
		cs.tracingCtx, cs.heightSpan = cs.tracer.Start(ctx, "cs.state.Height")
		cs.heightSpan.SetAttributes(attribute.Int64("height", height))
	}
	_, span := cs.tracer.Start(cs.getTracingCtx(ctx), "cs.state.enterNewRound")
	span.SetAttributes(attribute.Int("round", int(round)))
	span.SetAttributes(attribute.String("entry", entryLabel))
	defer span.End()

	// TODO: remove panics in this function and return an error

	logger := cs.logger.With("height", height, "round", round)

	if cs.roundState.Height() != height || round < cs.roundState.Round() || (cs.roundState.Round() == round && cs.roundState.Step() != cstypes.RoundStepNewHeight) {
		logger.Debug(
			"entering new round with invalid args",
			"current", fmt.Sprintf("%v/%v/%v", cs.roundState.Height(), cs.roundState.Round(), cs.roundState.Step()),
		)
		return
	}

	if now := tmtime.Now(); cs.roundState.StartTime().After(now) {
		logger.Debug("need to set a buffer and log message here for sanity", "start_time", cs.roundState.StartTime(), "now", now)
	}

	logger.Debug("entering new round", "current", fmt.Sprintf("%v/%v/%v", cs.roundState.Height(), cs.roundState.Round(), cs.roundState.Step()))

	// increment validators if necessary
	validators := cs.roundState.Validators()
	if cs.roundState.Round() < round {
		validators = validators.Copy()
		r, err := tmmath.SafeSubInt32(round, cs.roundState.Round())
		if err != nil {
			panic(err)
		}
		validators.IncrementProposerPriority(r)
	}

	// Setup new round
	// we don't fire newStep for this step,
	// but we fire an event, so update the round step first
	cs.updateRoundStep(round, cstypes.RoundStepNewRound)
	cs.roundState.SetValidators(validators)
	if round == 0 {
		// We've already reset these upon new height,
		// and meanwhile we might have received a proposal
		// for round 0.
	} else {
		logger.Debug("resetting proposal info")
		cs.roundState.SetProposal(nil)
		cs.roundState.SetProposalReceiveTime(time.Time{})
		cs.roundState.SetProposalBlock(nil)
		cs.roundState.SetProposalBlockParts(nil)
	}

	r, err := tmmath.SafeAddInt32(round, 1)
	if err != nil {
		panic(err)
	}

	cs.roundState.Votes().SetRound(r) // also track next round (round+1) to allow round-skipping
	cs.roundState.SetTriggeredTimeoutPrecommit(false)

	if err := cs.eventBus.PublishEventNewRound(cs.roundState.NewRoundEvent()); err != nil {
		cs.logger.Error("failed publishing new round", "err", err)
	}
	// Wait for txs to be available in the mempool
	// before we enterPropose in round 0. If the last block changed the app hash,
	// we may need an empty "proof" block, and enterPropose immediately.
	waitForTxs := cs.config.WaitForTxs() && round == 0 && !cs.needProofBlock(height)
	if waitForTxs {
		if cs.config.CreateEmptyBlocksInterval > 0 {
			cs.scheduleTimeout(cs.config.CreateEmptyBlocksInterval, height, round,
				cstypes.RoundStepNewRound)
		}
		return
	}

	span.End()
	cs.enterPropose(ctx, height, round, "enterNewRound")

	return
}

// needProofBlock returns true on the first height (so the genesis app hash is signed right away)
// and where the last block (height-1) caused the app hash to change
func (cs *State) needProofBlock(height int64) bool {
	if height == cs.state.InitialHeight {
		return true
	}

	lastBlockMeta := cs.blockStore.LoadBlockMeta(height - 1)
	if lastBlockMeta == nil {
		panic(fmt.Sprintf("needProofBlock: last block meta for height %d not found", height-1))
	}

	return !bytes.Equal(cs.state.AppHash, lastBlockMeta.Header.AppHash)
}

// Enter (CreateEmptyBlocks): from enterNewRound(height,round)
// Enter (CreateEmptyBlocks, CreateEmptyBlocksInterval > 0 ):
//
//	after enterNewRound(height,round), after timeout of CreateEmptyBlocksInterval
//
// Enter (!CreateEmptyBlocks) : after enterNewRound(height,round), once txs are in the mempool
func (cs *State) enterPropose(ctx context.Context, height int64, round int32, entryLabel string) {
	spanCtx, span := cs.tracer.Start(cs.getTracingCtx(ctx), "cs.state.enterPropose")
	span.SetAttributes(attribute.Int("round", int(round)))
	span.SetAttributes(attribute.String("entry", entryLabel))
	defer span.End()

	logger := cs.logger.With("height", height, "round", round)

	if cs.roundState.Height() != height || round < cs.roundState.Round() || (cs.roundState.Round() == round && cstypes.RoundStepPropose <= cs.roundState.Step()) {
		logger.Debug(
			"entering propose step with invalid args",
			"current", fmt.Sprintf("%v/%v/%v", cs.roundState.Height(), cs.roundState.Round(), cs.roundState.Step()),
		)
		return
	}

	// If this validator is the proposer of this round, and the previous block time is later than
	// our local clock time, wait to propose until our local clock time has passed the block time.
	if cs.privValidatorPubKey != nil && cs.isProposer(cs.privValidatorPubKey.Address()) {
		proposerWaitTime := proposerWaitTime(tmtime.DefaultSource{}, cs.state.LastBlockTime)
		if proposerWaitTime > 0 {
			cs.scheduleTimeout(proposerWaitTime, height, round, cstypes.RoundStepNewRound)
			return
		}
	}

	logger.Debug("entering propose step", "current", fmt.Sprintf("%v/%v/%v", cs.roundState.Height(), cs.roundState.Round(), cs.roundState.Step()))

	defer func() {
		// Done enterPropose:
		cs.updateRoundStep(round, cstypes.RoundStepPropose)
		cs.newStep()

		// If we have the whole proposal + POL, then goto Prevote now.
		// else, we'll enterPrevote when the rest of the proposal is received (in AddProposalBlockPart),
		// or else after timeoutPropose
		if cs.isProposalComplete() {
			// Do not count enterPrevote latency into enterPropose latency
			span.End()
			cs.enterPrevote(ctx, height, cs.roundState.Round(), "enterPropose")
		}
	}()

	// If we don't get the proposal and all block parts quick enough, enterPrevote
	cs.scheduleTimeout(cs.proposeTimeout(round), height, round, cstypes.RoundStepPropose)

	// Nothing more to do if we're not a validator
	if cs.privValidator == nil {
		logger.Debug("propose step; not proposing since node is not a validator")
		return
	}

	if cs.privValidatorPubKey == nil {
		// If this node is a validator & proposer in the current round, it will
		// miss the opportunity to create a block.
		logger.Error("propose step; empty priv validator public key", "err", errPubKeyIsNotSet)
		return
	}

	addr := cs.privValidatorPubKey.Address()

	// if not a validator, we're done
	if !cs.roundState.Validators().HasAddress(addr) {
		logger.Debug("propose step; not proposing since node is not in the validator set",
			"addr", addr,
			"vals", cs.roundState.Validators())
		return
	}

	if cs.isProposer(addr) {
		logger.Debug(
			"propose step; our turn to propose",
			"proposer", addr,
		)

		cs.decideProposal(spanCtx, height, round)
	} else {
		logger.Debug(
			"propose step; not our turn to propose",
			"proposer", cs.roundState.Validators().GetProposer().Address,
		)
	}
}

func (cs *State) isProposer(address []byte) bool {
	return bytes.Equal(cs.roundState.Validators().GetProposer().Address, address)
}

func (cs *State) defaultDecideProposal(ctx context.Context, height int64, round int32) {
	_, span := cs.tracer.Start(ctx, "cs.state.decideProposal")
	span.SetAttributes(attribute.Int("round", int(round)))
	defer span.End()

	var block *types.Block
	var blockParts *types.PartSet

	// Decide on block
	if cs.roundState.ValidBlock() != nil {
		// If there is valid block, choose that.
		block, blockParts = cs.roundState.ValidBlock(), cs.roundState.ValidBlockParts()
	} else {
		// Create a new proposal block from state/txs from the mempool.
		var err error
		block, err = cs.createProposalBlock(ctx)
		if err != nil {
			cs.logger.Error("unable to create proposal block", "error", err)
			return
		} else if block == nil {
			return
		}
		cs.metrics.ProposalCreateCount.Add(1)
		blockParts, err = block.MakePartSet(types.BlockPartSizeBytes)
		if err != nil {
			cs.logger.Error("unable to create proposal block part set", "error", err)
			return
		}
	}

	// Flush the WAL. Otherwise, we may not recompute the same proposal to sign,
	// and the privValidator will refuse to sign anything.
	if err := cs.wal.FlushAndSync(); err != nil {
		cs.logger.Error("failed flushing WAL to disk")
	}

	// Make proposal
	propBlockID := types.BlockID{Hash: block.Hash(), PartSetHeader: blockParts.Header()}
	proposal := types.NewProposal(height, round, cs.roundState.ValidRound(), propBlockID, block.Header.Time, block.GetTxKeys(), block.Header, block.LastCommit, block.Evidence, cs.privValidatorPubKey.Address())
	p := proposal.ToProto()

	// wait the max amount we would wait for a proposal
	ctxto, cancel := context.WithTimeout(ctx, cs.state.ConsensusParams.Timeout.Propose)
	defer cancel()
	if err := cs.privValidator.SignProposal(ctxto, cs.state.ChainID, p); err == nil {
		proposal.Signature = p.Signature

		// send proposal and block parts on internal msg queue
		cs.sendInternalMessage(ctx, msgInfo{&ProposalMessage{proposal}, "", tmtime.Now()})

		for i := 0; i < int(blockParts.Total()); i++ {
			part := blockParts.GetPart(i)
			cs.sendInternalMessage(ctx, msgInfo{&BlockPartMessage{cs.roundState.Height(), cs.roundState.Round(), part}, "", tmtime.Now()})
		}

		cs.logger.Debug("signed proposal", "height", height, "round", round, "proposal", proposal)
	} else if !cs.replayMode {
		cs.logger.Error("propose step; failed signing proposal", "height", height, "round", round, "err", err)
	}
}

// Returns true if the proposal block is complete &&
// (if POLRound was proposed, we have +2/3 prevotes from there).
func (cs *State) isProposalComplete() bool {
	if cs.roundState.Proposal() == nil || cs.roundState.ProposalBlock() == nil {
		return false
	}
	// we have the proposal. if there's a POLRound,
	// make sure we have the prevotes from it too
	if cs.roundState.Proposal().POLRound < 0 {
		return true
	}
	// if this is false the proposer is lying or we haven't received the POL yet
	return cs.roundState.Votes().Prevotes(cs.roundState.Proposal().POLRound).HasTwoThirdsMajority()

}

// Create the next block to propose and return it. Returns nil block upon error.
//
// We really only need to return the parts, but the block is returned for
// convenience so we can log the proposal block.
//
// NOTE: keep it side-effect free for clarity.
// CONTRACT: cs.privValidator is not nil.
func (cs *State) createProposalBlock(ctx context.Context) (*types.Block, error) {
	if cs.privValidator == nil {
		return nil, errors.New("entered createProposalBlock with privValidator being nil")
	}

	// TODO(sergio): wouldn't it be easier if CreateProposalBlock accepted cs.LastCommit directly?
	var lastExtCommit *types.ExtendedCommit
	switch {
	case cs.roundState.Height() == cs.state.InitialHeight:
		// We're creating a proposal for the first block.
		// The commit is empty, but not nil.
		lastExtCommit = &types.ExtendedCommit{}

	case cs.roundState.LastCommit().HasTwoThirdsMajority():
		// Make the commit from LastCommit
		lastExtCommit = cs.roundState.LastCommit().MakeExtendedCommit()

	default: // This shouldn't happen.
		cs.logger.Error("propose step; cannot propose anything without commit for the previous block")
		return nil, nil
	}

	if cs.privValidatorPubKey == nil {
		// If this node is a validator & proposer in the current round, it will
		// miss the opportunity to create a block.
		cs.logger.Error("propose step; empty priv validator public key", "err", errPubKeyIsNotSet)
		return nil, nil
	}

	proposerAddr := cs.privValidatorPubKey.Address()

	ret, err := cs.blockExec.CreateProposalBlock(ctx, cs.roundState.Height(), cs.state, lastExtCommit, proposerAddr)
	if err != nil {
		panic(err)
	}
	return ret, nil
}

// Enter: `timeoutPropose` after entering Propose.
// Enter: proposal block and POL is ready.
// If we received a valid proposal within this round and we are not locked on a block,
// we will prevote for block.
// Otherwise, if we receive a valid proposal that matches the block we are
// locked on or matches a block that received a POL in a round later than our
// locked round, prevote for the proposal, otherwise vote nil.
func (cs *State) enterPrevote(ctx context.Context, height int64, round int32, entryLabel string) {
	_, span := cs.tracer.Start(cs.getTracingCtx(ctx), "cs.state.enterPrevote")
	span.SetAttributes(attribute.Int("round", int(round)))
	span.SetAttributes(attribute.String("entry", entryLabel))
	defer span.End()

	logger := cs.logger.With("height", height, "round", round)

	if cs.roundState.Height() != height || round < cs.roundState.Round() || (cs.roundState.Round() == round && cstypes.RoundStepPrevote <= cs.roundState.Step()) {
		logger.Debug(
			"entering prevote step with invalid args",
			"current", fmt.Sprintf("%v/%v/%v", cs.roundState.Height(), cs.roundState.Round(), cs.roundState.Step()),
			"time", time.Now().UnixMilli(),
		)
		return
	}

	defer func() {
		// Done enterPrevote:
		cs.updateRoundStep(round, cstypes.RoundStepPrevote)
		cs.newStep()
	}()

	logger.Debug("entering prevote step", "current", fmt.Sprintf("%v/%v/%v", cs.roundState.Height(), cs.roundState.Round(), cs.roundState.Step()), "time", time.Now().UnixMilli())

	// Sign and broadcast vote as necessary
	cs.doPrevote(ctx, height, round)

	// Once `addVote` hits any +2/3 prevotes, we will go to PrevoteWait
	// (so we have more time to try and collect +2/3 prevotes for a single block)
}

func (cs *State) proposalIsTimely() bool {
	sp := cs.state.ConsensusParams.Synchrony.SynchronyParamsOrDefaults()
	return cs.roundState.Proposal().IsTimely(cs.roundState.ProposalReceiveTime(), sp, cs.roundState.Round())
}

func (cs *State) defaultDoPrevote(ctx context.Context, height int64, round int32) {
	logger := cs.logger.With("height", height, "round", round)

	// Check that a proposed block was not received within this round (and thus executing this from a timeout).
	if !cs.config.GossipTransactionKeyOnly && cs.roundState.ProposalBlock() == nil {
		cs.signAddVote(ctx, tmproto.PrevoteType, nil, types.PartSetHeader{})
		return
	}

	if cs.roundState.Proposal() == nil {
		logger.Info("prevote step: did not receive proposal; prevoting nil")
		cs.signAddVote(ctx, tmproto.PrevoteType, nil, types.PartSetHeader{})
		return
	}

	if cs.config.GossipTransactionKeyOnly {
		if cs.roundState.ProposalBlock() == nil {
			// If we're not the proposer, we need to build the block
			txKeys := cs.roundState.Proposal().TxKeys
			if cs.roundState.ProposalBlockParts().IsComplete() {
				block, err := cs.getBlockFromBlockParts()
				if err != nil {
					cs.signAddVote(ctx, tmproto.PrevoteType, nil, types.PartSetHeader{})
					return
				}
				// We have full proposal block and txs. Build proposal block with txKeys
				proposalBlock := cs.buildProposalBlock(height, block.Header, block.LastCommit, block.Evidence, block.ProposerAddress, txKeys)
				if proposalBlock == nil {
					cs.signAddVote(ctx, tmproto.PrevoteType, nil, types.PartSetHeader{})
					return
				}
				cs.roundState.SetProposalBlock(proposalBlock)
			} else {
				cs.signAddVote(ctx, tmproto.PrevoteType, nil, types.PartSetHeader{})
				return
			}
		}
	} else {
		if cs.roundState.ProposalBlock() == nil {
			block, err := cs.getBlockFromBlockParts()
			if err != nil {
				cs.logger.Error("Encountered error building block from parts", "block parts", cs.roundState.ProposalBlockParts())
				cs.signAddVote(ctx, tmproto.PrevoteType, nil, types.PartSetHeader{})
				return
			}
			if block == nil {
				logger.Error("prevote step: ProposalBlock is nil")
				cs.signAddVote(ctx, tmproto.PrevoteType, nil, types.PartSetHeader{})
				return
			}
			cs.roundState.SetProposalBlock(block)
		}
	}

	if !cs.roundState.Proposal().Timestamp.Equal(cs.roundState.ProposalBlock().Header.Time) {
		logger.Info("prevote step: proposal timestamp not equal; prevoting nil")
		cs.signAddVote(ctx, tmproto.PrevoteType, nil, types.PartSetHeader{})
		return
	}

	sp := cs.state.ConsensusParams.Synchrony.SynchronyParamsOrDefaults()
	if cs.roundState.Proposal().POLRound == -1 && cs.roundState.LockedRound() == -1 && !cs.proposalIsTimely() {
		logger.Info("prevote step: Proposal is not timely; prevoting nil",
			"proposed",
			tmtime.Canonical(cs.roundState.Proposal().Timestamp).Format(time.RFC3339Nano),
			"received",
			tmtime.Canonical(cs.roundState.ProposalReceiveTime()).Format(time.RFC3339Nano),
			"msg_delay",
			sp.MessageDelay,
			"precision",
			sp.Precision)
		cs.signAddVote(ctx, tmproto.PrevoteType, nil, types.PartSetHeader{})
		return
	}

	// Validate proposal block, from Tendermint's perspective
	err := cs.blockExec.ValidateBlock(ctx, cs.state, cs.roundState.ProposalBlock())
	if err != nil {
		// ProposalBlock is invalid, prevote nil.
		logger.Error("prevote step: consensus deems this block invalid; prevoting nil",
			"err", err)
		cs.signAddVote(ctx, tmproto.PrevoteType, nil, types.PartSetHeader{})
		return
	}

	/*
		The block has now passed Tendermint's validation rules.
		Before prevoting the block received from the proposer for the current round and height,
		we request the Application, via the ProcessProposal, ABCI call to confirm that the block is
		valid. If the Application does not accept the block, Tendermint prevotes nil.

		WARNING: misuse of block rejection by the Application can seriously compromise Tendermint's
		liveness properties. Please see PrepareProposal-ProcessProposal coherence and determinism
		properties in the ABCI++ specification.
	*/
	isAppValid, err := cs.blockExec.ProcessProposal(ctx, cs.roundState.ProposalBlock(), cs.state)
	if err != nil {
		panic(fmt.Sprintf("ProcessProposal: %v", err))
	}
	cs.metrics.MarkProposalProcessed(isAppValid)

	// Vote nil if the Application rejected the block
	if !isAppValid {
		var proposerAddress crypto.Address
		var numberOfTxs int

		if proposal := cs.roundState.Proposal(); proposal != nil {
			proposerAddress = proposal.ProposerAddress
		}

		if proposalBlock := cs.roundState.ProposalBlock(); proposalBlock != nil && proposalBlock.Txs != nil {
			numberOfTxs = proposalBlock.Txs.Len()
		}

		logger.Error("prevote step: state machine rejected a proposed block; this should not happen:"+
			"the proposer may be misbehaving; prevoting nil", "err", err,
			"proposerAddress", proposerAddress,
			"numberOfTxs", numberOfTxs)

		cs.signAddVote(ctx, tmproto.PrevoteType, nil, types.PartSetHeader{})
		return
	}

	/*
		22: upon <PROPOSAL, h_p, round_p, v, −1> from proposer(h_p, round_p) while step_p = propose do
		23: if valid(v) && (lockedRound_p = −1 || lockedValue_p = v) then
		24: broadcast <PREVOTE, h_p, round_p, id(v)>

		Here, cs.Proposal.POLRound corresponds to the -1 in the above algorithm rule.
		This means that the proposer is producing a new proposal that has not previously
		seen a 2/3 majority by the network.

		If we have already locked on a different value that is different from the proposed value,
		we prevote nil since we are locked on a different value. Otherwise, if we're not locked on a block
		or the proposal matches our locked block, we prevote the proposal.
	*/
	if cs.roundState.Proposal().POLRound == -1 {
		if cs.roundState.LockedRound() == -1 {
			logger.Info("prevote step: ProposalBlock is valid and there is no locked block; prevoting the proposal")
			cs.signAddVote(ctx, tmproto.PrevoteType, cs.roundState.ProposalBlock().Hash(), cs.roundState.ProposalBlockParts().Header())
			return
		}
		if cs.roundState.ProposalBlock().HashesTo(cs.roundState.LockedBlock().Hash()) {
			logger.Info("prevote step: ProposalBlock is valid and matches our locked block; prevoting the proposal")
			cs.signAddVote(ctx, tmproto.PrevoteType, cs.roundState.ProposalBlock().Hash(), cs.roundState.ProposalBlockParts().Header())
			return
		}
	}

	/*
		28: upon <PROPOSAL, h_p, round_p, v, v_r> from proposer(h_p, round_p) AND 2f + 1 <PREVOTE, h_p, v_r, id(v)> while
		step_p = propose && (v_r ≥ 0 && v_r < round_p) do
		29: if valid(v) && (lockedRound_p ≤ v_r || lockedValue_p = v) then
		30: broadcast <PREVOTE, h_p, round_p, id(v)>

		This rule is a bit confusing but breaks down as follows:

		If we see a proposal in the current round for value 'v' that lists its valid round as 'v_r'
		AND this validator saw a 2/3 majority of the voting power prevote 'v' in round 'v_r', then we will
		issue a prevote for 'v' in this round if 'v' is valid and either matches our locked value OR
		'v_r' is a round greater than or equal to our current locked round.

		'v_r' can be a round greater than to our current locked round if a 2/3 majority of
		the network prevoted a value in round 'v_r' but we did not lock on it, possibly because we
		missed the proposal in round 'v_r'.
	*/
	blockID, ok := cs.roundState.Votes().Prevotes(cs.roundState.Proposal().POLRound).TwoThirdsMajority()
	if ok && cs.roundState.ProposalBlock().HashesTo(blockID.Hash) && cs.roundState.Proposal().POLRound >= 0 && cs.roundState.Proposal().POLRound < cs.roundState.Round() {
		if cs.roundState.LockedRound() <= cs.roundState.Proposal().POLRound {
			logger.Info("prevote step: ProposalBlock is valid and received a 2/3" +
				"majority in a round later than the locked round; prevoting the proposal")
			cs.signAddVote(ctx, tmproto.PrevoteType, cs.roundState.ProposalBlock().Hash(), cs.roundState.ProposalBlockParts().Header())
			return
		}
		if cs.roundState.ProposalBlock().HashesTo(cs.roundState.LockedBlock().Hash()) {
			logger.Info("prevote step: ProposalBlock is valid and matches our locked block; prevoting the proposal")
			cs.signAddVote(ctx, tmproto.PrevoteType, cs.roundState.ProposalBlock().Hash(), cs.roundState.ProposalBlockParts().Header())
			return
		}
	}

	logger.Info("prevote step: ProposalBlock is valid but was not our locked block or " +
		"did not receive a more recent majority; prevoting nil")
	cs.signAddVote(ctx, tmproto.PrevoteType, nil, types.PartSetHeader{})
}

// Enter: any +2/3 prevotes at next round.
func (cs *State) enterPrevoteWait(height int64, round int32) {
	logger := cs.logger.With("height", height, "round", round)

	if cs.roundState.Height() != height || round < cs.roundState.Round() || (cs.roundState.Round() == round && cstypes.RoundStepPrevoteWait <= cs.roundState.Step()) {
		logger.Debug(
			"entering prevote wait step with invalid args",
			"current", fmt.Sprintf("%v/%v/%v", cs.roundState.Height(), cs.roundState.Round(), cs.roundState.Step()),
			"time", time.Now().UnixMilli(),
		)
		return
	}

	if !cs.roundState.Votes().Prevotes(round).HasTwoThirdsAny() {
		panic(fmt.Sprintf(
			"entering prevote wait step (%v/%v), but prevotes does not have any +2/3 votes",
			height, round,
		))
	}

	logger.Debug("entering prevote wait step", "current", fmt.Sprintf("%v/%v/%v", cs.roundState.Height(), cs.roundState.Round(), cs.roundState.Step()), "time", time.Now().UnixMilli())

	defer func() {
		// Done enterPrevoteWait:
		cs.updateRoundStep(round, cstypes.RoundStepPrevoteWait)
		cs.newStep()
	}()

	// Wait for some more prevotes; enterPrecommit
	cs.scheduleTimeout(cs.voteTimeout(round), height, round, cstypes.RoundStepPrevoteWait)
}

// Enter: `timeoutPrevote` after any +2/3 prevotes.
// Enter: `timeoutPrecommit` after any +2/3 precommits.
// Enter: +2/3 precomits for block or nil.
// Lock & precommit the ProposalBlock if we have enough prevotes for it (a POL in this round)
// else, precommit nil otherwise.
func (cs *State) enterPrecommit(ctx context.Context, height int64, round int32, entryLabel string) {
	_, span := cs.tracer.Start(cs.getTracingCtx(ctx), "cs.state.enterPrecommit")
	span.SetAttributes(attribute.Int("round", int(round)))
	span.SetAttributes(attribute.String("entry", entryLabel))
	defer span.End()

	logger := cs.logger.With("height", height, "round", round)

	if cs.roundState.Height() != height || round < cs.roundState.Round() || (cs.roundState.Round() == round && cstypes.RoundStepPrecommit <= cs.roundState.Step()) {
		logger.Debug(
			"entering precommit step with invalid args",
			"current", fmt.Sprintf("%v/%v/%v", cs.roundState.Height(), cs.roundState.Round(), cs.roundState.Step()),
			"time", time.Now().UnixMilli(),
			"expected", fmt.Sprintf("#%v/%v", height, round),
			"entryLabel", entryLabel,
		)
		return
	}

	logger.Debug("entering precommit step", "current", fmt.Sprintf("%v/%v/%v", cs.roundState.Height(), cs.roundState.Round(), cs.roundState.Step()), "time", time.Now().UnixMilli())

	defer func() {
		// Done enterPrecommit:
		cs.updateRoundStep(round, cstypes.RoundStepPrecommit)
		cs.newStep()
	}()

	// check for a polka
	blockID, ok := cs.roundState.Votes().Prevotes(round).TwoThirdsMajority()

	// If we don't have a polka, we must precommit nil.
	if !ok {
		if cs.roundState.LockedBlock() != nil {
			logger.Info("precommit step; no +2/3 prevotes during enterPrecommit while we are locked; precommitting nil")
		} else {
			logger.Info("precommit step; no +2/3 prevotes during enterPrecommit; precommitting nil")
		}

		cs.signAddVote(ctx, tmproto.PrecommitType, nil, types.PartSetHeader{})
		return
	}

	// At this point +2/3 prevoted for a particular block or nil.
	if err := cs.eventBus.PublishEventPolka(cs.roundState.RoundStateEvent()); err != nil {
		logger.Error("failed publishing polka", "err", err)
	}

	// the latest POLRound should be this round.
	polRound, _ := cs.roundState.Votes().POLInfo()
	if polRound < round {
		panic(fmt.Sprintf("this POLRound should be %v but got %v", round, polRound))
	}

	// +2/3 prevoted nil. Precommit nil.
	if blockID.IsNil() {
		logger.Info("precommit step: +2/3 prevoted for nil; precommitting nil")
		cs.signAddVote(ctx, tmproto.PrecommitType, nil, types.PartSetHeader{})
		return
	}
	// At this point, +2/3 prevoted for a particular block.

	// If we never received a proposal for this block, we must precommit nil
	if cs.roundState.Proposal() == nil || cs.roundState.ProposalBlock() == nil {
		logger.Info("precommit step; did not receive proposal, precommitting nil")
		cs.signAddVote(ctx, tmproto.PrecommitType, nil, types.PartSetHeader{})
		return
	}

	// If the proposal time does not match the block time, precommit nil.
	if !cs.roundState.Proposal().Timestamp.Equal(cs.roundState.ProposalBlock().Header.Time) {
		logger.Info("precommit step: proposal timestamp not equal; precommitting nil")
		cs.signAddVote(ctx, tmproto.PrecommitType, nil, types.PartSetHeader{})
		return
	}

	// If we're already locked on that block, precommit it, and update the LockedRound
	if cs.roundState.LockedBlock().HashesTo(blockID.Hash) {
		logger.Info("precommit step: +2/3 prevoted locked block; relocking")
		cs.roundState.SetLockedRound(round)

		if err := cs.eventBus.PublishEventRelock(cs.roundState.RoundStateEvent()); err != nil {
			logger.Error("precommit step: failed publishing event relock", "err", err)
		}

		cs.signAddVote(ctx, tmproto.PrecommitType, blockID.Hash, blockID.PartSetHeader)
		return
	}

	// If greater than 2/3 of the voting power on the network prevoted for
	// the proposed block, update our locked block to this block and issue a
	// precommit vote for it.
	if cs.roundState.ProposalBlock().HashesTo(blockID.Hash) {
		logger.Info("precommit step: +2/3 prevoted proposal block; locking", "hash", blockID.Hash)

		// Validate the block.
		if err := cs.blockExec.ValidateBlock(ctx, cs.state, cs.roundState.ProposalBlock()); err != nil {
			panic(fmt.Sprintf("precommit step: +2/3 prevoted for an invalid block %v; relocking", err))
		}

		cs.roundState.SetLockedRound(round)
		cs.roundState.SetLockedBlock(cs.roundState.ProposalBlock())
		cs.roundState.SetLockedBlockParts(cs.roundState.ProposalBlockParts())

		if err := cs.eventBus.PublishEventLock(cs.roundState.RoundStateEvent()); err != nil {
			logger.Error("precommit step: failed publishing event lock", "err", err)
		}

		cs.signAddVote(ctx, tmproto.PrecommitType, blockID.Hash, blockID.PartSetHeader)
		return
	}

	// There was a polka in this round for a block we don't have.
	// Fetch that block, and precommit nil.
	logger.Info("precommit step: +2/3 prevotes for a block we do not have; voting nil", "block_id", blockID)

	if !cs.roundState.ProposalBlockParts().HasHeader(blockID.PartSetHeader) {
		cs.roundState.SetProposalBlock(nil)
		cs.metrics.MarkBlockGossipStarted()
		cs.roundState.SetProposalBlockParts(types.NewPartSetFromHeader(blockID.PartSetHeader))
	}

	cs.signAddVote(ctx, tmproto.PrecommitType, nil, types.PartSetHeader{})
}

// Enter: any +2/3 precommits for next round.
func (cs *State) enterPrecommitWait(height int64, round int32) {
	logger := cs.logger.With("height", height, "round", round)

	if cs.roundState.Height() != height || round < cs.roundState.Round() || (cs.roundState.Round() == round && cs.roundState.TriggeredTimeoutPrecommit()) {
		logger.Debug(
			"entering precommit wait step with invalid args",
			"triggered_timeout", cs.roundState.TriggeredTimeoutPrecommit(),
			"current", fmt.Sprintf("%v/%v", cs.roundState.Height(), cs.roundState.Round()),
			"time", time.Now().UnixMilli(),
		)
		return
	}

	if !cs.roundState.Votes().Precommits(round).HasTwoThirdsAny() {
		panic(fmt.Sprintf(
			"entering precommit wait step (%v/%v), but precommits does not have any +2/3 votes",
			height, round,
		))
	}

	logger.Debug("entering precommit wait step", "current", fmt.Sprintf("%v/%v/%v", cs.roundState.Height(), cs.roundState.Round(), cs.roundState.Step()), "time", time.Now().UnixMilli())

	defer func() {
		// Done enterPrecommitWait:
		cs.roundState.SetTriggeredTimeoutPrecommit(true)
		cs.newStep()
	}()

	// wait for some more precommits; enterNewRound
	cs.scheduleTimeout(cs.voteTimeout(round), height, round, cstypes.RoundStepPrecommitWait)
}

// Enter: +2/3 precommits for block
func (cs *State) enterCommit(ctx context.Context, height int64, commitRound int32, entryLabel string) {
	spanCtx, span := cs.tracer.Start(cs.getTracingCtx(ctx), "cs.state.enterCommit")
	span.SetAttributes(attribute.Int("round", int(commitRound)))
	span.SetAttributes(attribute.String("entry", entryLabel))
	defer span.End()

	logger := cs.logger.With("height", height, "commit_round", commitRound)

	if cs.roundState.Height() != height || cstypes.RoundStepCommit <= cs.roundState.Step() {
		logger.Debug(
			"entering commit step with invalid args",
			"current", fmt.Sprintf("%v/%v/%v", cs.roundState.Height(), cs.roundState.Round(), cs.roundState.Step()),
			"time", time.Now().UnixMilli(),
		)
		return
	}

	logger.Debug("entering commit step", "current", fmt.Sprintf("%v/%v/%v", cs.roundState.Height(), cs.roundState.Round(), cs.roundState.Step()), "time", time.Now().UnixMilli())

	defer func() {
		// Done enterCommit:
		// keep cs.Round the same, commitRound points to the right Precommits set.
		cs.updateRoundStep(cs.roundState.Round(), cstypes.RoundStepCommit)
		cs.roundState.SetCommitRound(commitRound)
		cs.roundState.SetCommitTime(tmtime.Now())
		cs.newStep()

		// Maybe finalize immediately.
		cs.tryFinalizeCommit(spanCtx, height)
	}()

	blockID, ok := cs.roundState.Votes().Precommits(commitRound).TwoThirdsMajority()
	if !ok {
		panic("RunActionCommit() expects +2/3 precommits")
	}

	// The Locked* fields no longer matter.
	// Move them over to ProposalBlock if they match the commit hash,
	// otherwise they'll be cleared in updateToState.
	if cs.roundState.LockedBlock().HashesTo(blockID.Hash) {
		logger.Info("commit is for a locked block; set ProposalBlock=LockedBlock", "block_hash", blockID.Hash)
		cs.roundState.SetProposalBlock(cs.roundState.LockedBlock())
		cs.roundState.SetProposalBlockParts(cs.roundState.LockedBlockParts())
	}

	// If we don't have the block being committed, set up to get it.
	if !cs.roundState.ProposalBlock().HashesTo(blockID.Hash) {
		if !cs.roundState.ProposalBlockParts().HasHeader(blockID.PartSetHeader) {
			logger.Info(
				"commit is for a block we do not know about; set ProposalBlock=nil",
				"proposal", cs.roundState.ProposalBlock().Hash(),
				"commit", blockID.Hash,
			)

			// We're getting the wrong block.
			// Set up ProposalBlockParts and keep waiting.
			cs.roundState.SetProposalBlock(nil)
			cs.metrics.MarkBlockGossipStarted()
			cs.roundState.SetProposalBlockParts(types.NewPartSetFromHeader(blockID.PartSetHeader))

			if err := cs.eventBus.PublishEventValidBlock(cs.roundState.RoundStateEvent()); err != nil {
				logger.Error("failed publishing valid block", "err", err)
			}

			roundState := cs.roundState.CopyInternal()
			cs.evsw.FireEvent(types.EventValidBlockValue, roundState)
		}
	}
}

// If we have the block AND +2/3 commits for it, finalize.
func (cs *State) tryFinalizeCommit(ctx context.Context, height int64) {
	logger := cs.logger.With("height", height)

	if cs.roundState.Height() != height {
		panic(fmt.Sprintf("tryFinalizeCommit() cs.Height: %v vs height: %v", cs.roundState.Height(), height))
	}

	blockID, ok := cs.roundState.Votes().Precommits(cs.roundState.CommitRound()).TwoThirdsMajority()
	if !ok || blockID.IsNil() {
		logger.Error("failed attempt to finalize commit; there was no +2/3 majority or +2/3 was for nil")
		return
	}

	if !cs.roundState.ProposalBlock().HashesTo(blockID.Hash) {
		// TODO: this happens every time if we're not a validator (ugly logs)
		// TODO: ^^ wait, why does it matter that we're a validator?
		logger.Info(
			"failed attempt to finalize commit; we do not have the commit block",
			"proposal_block", cs.roundState.ProposalBlock().Hash(),
			"commit_block", blockID.Hash,
			"time", time.Now().UnixMilli(),
		)
		return
	}

	cs.finalizeCommit(ctx, height)
}

// Increment height and goto cstypes.RoundStepNewHeight
func (cs *State) finalizeCommit(ctx context.Context, height int64) {
	spanCtx, span := cs.tracer.Start(ctx, "cs.state.finalizeCommit")
	defer span.End()
	logger := cs.logger.With("height", height)

	if cs.roundState.Height() != height || cs.roundState.Step() != cstypes.RoundStepCommit {
		logger.Debug(
			"entering finalize commit step",
			"current", fmt.Sprintf("%v/%v/%v", cs.roundState.Height(), cs.roundState.Round(), cs.roundState.Step()),
			"time", time.Now().UnixMilli(),
		)
		return
	}

	cs.calculatePrevoteMessageDelayMetrics()

	blockID, ok := cs.roundState.Votes().Precommits(cs.roundState.CommitRound()).TwoThirdsMajority()
	block, blockParts := cs.roundState.ProposalBlock(), cs.roundState.ProposalBlockParts()

	if !ok {
		panic("cannot finalize commit; commit does not have 2/3 majority")
	}
	if !blockParts.HasHeader(blockID.PartSetHeader) {
		panic("expected ProposalBlockParts header to be commit header")
	}
	if !block.HashesTo(blockID.Hash) {
		panic("cannot finalize commit; proposal block does not hash to commit hash")
	}

	if err := cs.blockExec.ValidateBlock(ctx, cs.state, block); err != nil {
		panic(fmt.Errorf("+2/3 committed an invalid block: %w", err))
	}

	logger.Info(
		"finalizing commit of block",
		"hash", block.Hash(),
		"root", block.AppHash,
		"num_txs", len(block.Txs),
		"time", time.Now().UnixMilli(),
	)
	logger.Debug(fmt.Sprintf("%v", block))

	// Save to blockStore.
	if cs.blockStore.Height() < block.Height {
		// NOTE: the seenCommit is local justification to commit this block,
		// but may differ from the LastCommit included in the next block
		_, storeBlockSpan := cs.tracer.Start(spanCtx, "cs.state.finalizeCommit.saveblockstore")
		defer storeBlockSpan.End()
		seenExtendedCommit := cs.roundState.Votes().Precommits(cs.roundState.CommitRound()).MakeExtendedCommit()
		if cs.state.ConsensusParams.ABCI.VoteExtensionsEnabled(block.Height) {
			cs.blockStore.SaveBlockWithExtendedCommit(block, blockParts, seenExtendedCommit)
		} else {
			cs.blockStore.SaveBlock(block, blockParts, seenExtendedCommit.ToCommit())
		}
		// Calculate consensus time
		cs.metrics.ConsensusTime.Observe(time.Since(cs.roundState.StartTime()).Seconds())
	} else {
		// Happens during replay if we already saved the block but didn't commit
		logger.Debug("calling finalizeCommit on already stored block", "height", block.Height)
	}

	// Write EndHeightMessage{} for this height, implying that the blockstore
	// has saved the block.
	//
	// If we crash before writing this EndHeightMessage{}, we will recover by
	// running ApplyBlock during the ABCI handshake when we restart.  If we
	// didn't save the block to the blockstore before writing
	// EndHeightMessage{}, we'd have to change WAL replay -- currently it
	// complains about replaying for heights where an #ENDHEIGHT entry already
	// exists.
	//
	// Either way, the State should not be resumed until we
	// successfully call ApplyBlock (ie. later here, or in Handshake after
	// restart).
	endMsg := EndHeightMessage{height}
	_, fsyncSpan := cs.tracer.Start(spanCtx, "cs.state.finalizeCommit.fsync")
	defer fsyncSpan.End()
	if err := cs.wal.WriteSync(endMsg); err != nil { // NOTE: fsync
		panic(fmt.Errorf(
			"failed to write %v msg to consensus WAL due to %w; check your file system and restart the node",
			endMsg, err,
		))
	}
	fsyncSpan.End()

	// Create a copy of the state for staging and an event cache for txs.
	stateCopy := cs.state.Copy()

	// Execute and commit the block, update and save the state, and update the mempool.
	// NOTE The block.AppHash won't reflect these txs until the next block.
	startTime := time.Now()
	stateCopy, err := cs.blockExec.ApplyBlock(spanCtx,
		stateCopy,
		types.BlockID{
			Hash:          block.Hash(),
			PartSetHeader: blockParts.Header(),
		},
		block,
		cs.tracer,
	)
	cs.metrics.ApplyBlockLatency.Observe(float64(time.Since(startTime).Milliseconds()))
	if err != nil {
		logger.Error("failed to apply block", "err", err)
		return
	}

	// must be called before we update state
	cs.RecordMetrics(height, block)

	// NewHeightStep!
	cs.updateToState(stateCopy)

	// Private validator might have changed it's key pair => refetch pubkey.
	if err := cs.updatePrivValidatorPubKey(ctx); err != nil {
		logger.Error("failed to get private validator pubkey", "err", err)
	}

	// cs.StartTime is already set.
	// Schedule Round0 to start soon.
	cs.scheduleRound0(cs.roundState.GetInternalPointer())

	// By here,
	// * cs.Height has been increment to height+1
	// * cs.Step is now cstypes.RoundStepNewHeight
	// * cs.StartTime is set to when we will start round0.
}

func (cs *State) RecordMetrics(height int64, block *types.Block) {
	cs.metrics.Validators.Set(float64(cs.roundState.Validators().Size()))
	cs.metrics.ValidatorsPower.Set(float64(cs.roundState.Validators().TotalVotingPower()))

	var (
		missingValidators      int
		missingValidatorsPower int64
	)
	// height=0 -> MissingValidators and MissingValidatorsPower are both 0.
	// Remember that the first LastCommit is intentionally empty, so it's not
	// fair to increment missing validators number.
	if height > cs.state.InitialHeight {
		// Sanity check that commit size matches validator set size - only applies
		// after first block.
		var (
			commitSize = block.LastCommit.Size()
			valSetLen  = len(cs.roundState.LastValidators().Validators)
			address    types.Address
		)
		if commitSize != valSetLen {
			cs.logger.Error(fmt.Sprintf("commit size (%d) doesn't match valset length (%d) at height %d\n\n%v\n\n%v",
				commitSize, valSetLen, block.Height, block.LastCommit.Signatures, cs.roundState.LastValidators().Validators))
			return
		}

		if cs.privValidator != nil {
			if cs.privValidatorPubKey == nil {
				// Metrics won't be updated, but it's not critical.
				cs.logger.Error("recordMetrics", "err", errPubKeyIsNotSet)
			} else {
				address = cs.privValidatorPubKey.Address()
			}
		}

		for i, val := range cs.roundState.LastValidators().Validators {
			commitSig := block.LastCommit.Signatures[i]
			if commitSig.BlockIDFlag == types.BlockIDFlagAbsent {
				missingValidators++
				missingValidatorsPower += val.VotingPower
				cs.metrics.MissingValidatorsPower.With("validator_address", val.Address.String()).Set(float64(val.VotingPower))
			} else {
				cs.metrics.MissingValidatorsPower.With("validator_address", val.Address.String()).Set(0)
			}

			if bytes.Equal(val.Address, address) {
				label := []string{
					"validator_address", val.Address.String(),
				}
				cs.metrics.ValidatorPower.With(label...).Set(float64(val.VotingPower))
				if commitSig.BlockIDFlag == types.BlockIDFlagCommit {
					cs.metrics.ValidatorLastSignedHeight.With(label...).Set(float64(height))
				} else {
					cs.metrics.ValidatorMissedBlocks.With(label...).Add(float64(1))
				}
			}

		}
	}
	cs.metrics.MissingValidators.Set(float64(missingValidators))

	// NOTE: byzantine validators power and count is only for consensus evidence i.e. duplicate vote
	var (
		byzantineValidatorsPower int64
		byzantineValidatorsCount int64
	)

	for _, ev := range block.Evidence {
		if dve, ok := ev.(*types.DuplicateVoteEvidence); ok {
			if _, val := cs.roundState.Validators().GetByAddress(dve.VoteA.ValidatorAddress); val != nil {
				byzantineValidatorsCount++
				byzantineValidatorsPower += val.VotingPower
			}
		}

	}
	cs.metrics.ByzantineValidators.Set(float64(byzantineValidatorsCount))
	cs.metrics.ByzantineValidatorsPower.Set(float64(byzantineValidatorsPower))

	// Block Interval metric
	if height > 1 {
		lastBlockMeta := cs.blockStore.LoadBlockMeta(height - 1)
		if lastBlockMeta != nil {
			cs.metrics.BlockIntervalSeconds.Observe(
				block.Time.Sub(lastBlockMeta.Header.Time).Seconds(),
			)
		}
	}

	roundState := cs.GetRoundState()
	proposal := roundState.Proposal

	// Latency metric for prevote delay
	if proposal != nil {
		cs.metrics.MarkFinalRound(roundState.Round, proposal.ProposerAddress.String())
		cs.metrics.MarkProposeLatency(proposal.ProposerAddress.String(), proposal.Timestamp.Sub(roundState.StartTime).Seconds())
		for roundId := 0; int32(roundId) <= roundState.ValidRound; roundId++ {
			preVotes := roundState.Votes.Prevotes(int32(roundId))
			pl := preVotes.List()
			if pl == nil || len(pl) == 0 {
				cs.logger.Info("no prevotes to emit latency metrics for", "height", height, "round", roundId)
				continue
			}
			sort.Slice(pl, func(i, j int) bool {
				return pl[i].Timestamp.Before(pl[j].Timestamp)
			})
			firstVoteDelay := pl[0].Timestamp.Sub(roundState.StartTime).Seconds()
			for _, vote := range pl {
				currVoteDelay := vote.Timestamp.Sub(roundState.StartTime).Seconds()
				relativeVoteDelay := currVoteDelay - firstVoteDelay
				cs.metrics.MarkPrevoteLatency(vote.ValidatorAddress.String(), relativeVoteDelay)
			}
		}
	}
	cs.metrics.NumTxs.Set(float64(len(block.Data.Txs)))
	cs.metrics.TotalTxs.Add(float64(len(block.Data.Txs)))
	cs.metrics.BlockSizeBytes.Observe(float64(block.Size()))
	cs.metrics.CommittedHeight.Set(float64(block.Height))
}

//-----------------------------------------------------------------------------

func (cs *State) defaultSetProposal(proposal *types.Proposal, recvTime time.Time) error {
	// Already have one
	// TODO: possibly catch double proposals
	if cs.roundState.Proposal() != nil || proposal == nil {
		return nil
	}

	// Does not apply
	if proposal.Height != cs.roundState.Height() || proposal.Round != cs.roundState.Round() {
		return nil
	}

	// Verify POLRound, which must be -1 or in range [0, proposal.Round).
	if proposal.POLRound < -1 ||
		(proposal.POLRound >= 0 && proposal.POLRound >= proposal.Round) {
		return ErrInvalidProposalPOLRound
	}

	p := proposal.ToProto()
	// Verify signature
	if !cs.roundState.Validators().GetProposer().PubKey.VerifySignature(
		types.ProposalSignBytes(cs.state.ChainID, p), proposal.Signature,
	) {
		return ErrInvalidProposalSignature
	}

	proposal.Signature = p.Signature
	cs.roundState.SetProposal(proposal)
	cs.roundState.SetProposalReceiveTime(recvTime)
	cs.calculateProposalTimestampDifferenceMetric()
	// We don't update cs.ProposalBlockParts if it is already set.
	// This happens if we're already in cstypes.RoundStepCommit or if there is a valid block in the current round.
	// TODO: We can check if Proposal is for a different block as this is a sign of misbehavior!
	if cs.roundState.ProposalBlockParts() == nil {
		cs.metrics.MarkBlockGossipStarted()
		cs.roundState.SetProposalBlockParts(types.NewPartSetFromHeader(proposal.BlockID.PartSetHeader))
	}

	cs.logger.Debug("received proposal", "proposal", proposal)
	return nil
}

// NOTE: block is not necessarily valid.
// Asynchronously triggers either enterPrevote (before we timeout of propose) or tryFinalizeCommit,
// once we have the full block.
func (cs *State) addProposalBlockPart(
	msg *BlockPartMessage,
	peerID types.NodeID,
) (added bool, err error) {
	height, round, part := msg.Height, msg.Round, msg.Part

	// Blocks might be reused, so round mismatch is OK
	if cs.roundState.Height() != height {
		cs.logger.Debug("received block part from wrong height", "height", height, "round", round)
		cs.metrics.BlockGossipPartsReceived.With("matches_current", "false").Add(1)
		return false, nil
	}

	// We're not expecting a block part.
	if cs.roundState.ProposalBlockParts() == nil {
		cs.metrics.BlockGossipPartsReceived.With("matches_current", "false").Add(1)
		// NOTE: this can happen when we've gone to a higher round and
		// then receive parts from the previous round - not necessarily a bad peer.
		cs.logger.Debug(
			"received a block part when we are not expecting any",
			"height", height,
			"round", round,
			"index", part.Index,
			"peer", peerID,
		)
		return false, nil
	}

	added, err = cs.roundState.ProposalBlockParts().AddPart(part)
	if err != nil {
		if errors.Is(err, types.ErrPartSetInvalidProof) || errors.Is(err, types.ErrPartSetUnexpectedIndex) {
			cs.metrics.BlockGossipPartsReceived.With("matches_current", "false").Add(1)
		}
		return added, err
	}

	cs.metrics.BlockGossipPartsReceived.With("matches_current", "true").Add(1)

	if cs.roundState.ProposalBlockParts().ByteSize() > cs.state.ConsensusParams.Block.MaxBytes {
		return added, fmt.Errorf("total size of proposal block parts exceeds maximum block bytes (%d > %d)",
			cs.roundState.ProposalBlockParts().ByteSize(), cs.state.ConsensusParams.Block.MaxBytes,
		)
	}
	if added && cs.roundState.ProposalBlockParts().IsComplete() {
		cs.metrics.MarkBlockGossipComplete()
		block, err := cs.getBlockFromBlockParts()
		if err != nil {
			cs.logger.Error("Encountered error building block from parts", "block parts", cs.roundState.ProposalBlockParts())
			return false, err
		}

		cs.roundState.SetProposalBlock(block)
		// NOTE: it's possible to receive complete proposal blocks for future rounds without having the proposal
		cs.logger.Info("received complete proposal block", "height", cs.roundState.ProposalBlock().Height, "hash", cs.roundState.ProposalBlock().Hash(), "time", time.Now().UnixMilli())

		if err := cs.eventBus.PublishEventCompleteProposal(cs.roundState.CompleteProposalEvent()); err != nil {
			cs.logger.Error("failed publishing event complete proposal", "err", err)
		}
	}

	return added, nil
}

func (cs *State) getBlockFromBlockParts() (*types.Block, error) {
	bz, err := io.ReadAll(cs.roundState.ProposalBlockParts().GetReader())
	if err != nil {
		return nil, err
	}

	var pbb = new(tmproto.Block)
	err = proto.Unmarshal(bz, pbb)
	if err != nil {
		return nil, err
	}

	block, err := types.BlockFromProto(pbb)
	if err != nil {
		return nil, err
	}
	return block, nil
}

func (cs *State) tryCreateProposalBlock(ctx context.Context, height int64, round int32, header types.Header, lastCommit *types.Commit, evidence []types.Evidence, proposerAddress types.Address) bool {
	_, span := cs.tracer.Start(ctx, "cs.state.tryCreateProposalBlock")
	span.SetAttributes(attribute.Int("round", int(round)))
	defer span.End()

	// Blocks might be reused, so round mismatch is OK
	if cs.roundState.Height() != height {
		cs.logger.Info("received block part from wrong height", "height", height, "round", round)
		cs.metrics.BlockGossipPartsReceived.With("matches_current", "false").Add(1)
		return false
	}
	// We may not have a valid proposal yet (e.g. only received proposal for a wrong height)
	if cs.roundState.Proposal() == nil {
		return false
	}
	block := cs.buildProposalBlock(height, header, lastCommit, evidence, proposerAddress, cs.roundState.Proposal().TxKeys)
	if block == nil {
		return false
	}
	cs.roundState.SetProposalBlock(block)
	partSet, err := block.MakePartSet(types.BlockPartSizeBytes)
	if err != nil {
		return false
	}
	cs.roundState.SetProposalBlockParts(partSet)
	// NOTE: it's possible to receive complete proposal blocks for future rounds without having the proposal
	cs.metrics.MarkBlockGossipComplete()
	return true
}

// Build a proposal block from mempool txs. If cs.config.GossipTransactionKeyOnly=true
// proposals only contain txKeys so we rebuild the block using mempool txs
func (cs *State) buildProposalBlock(height int64, header types.Header, lastCommit *types.Commit, evidence []types.Evidence, proposerAddress types.Address, txKeys []types.TxKey) *types.Block {
	txs, missingTxs := cs.blockExec.SafeGetTxsByKeys(txKeys)
	if len(missingTxs) > 0 {
		cs.metrics.ProposalMissingTxs.Set(float64(len(missingTxs)))
		cs.logger.Debug("Missing txs when trying to build block", "missing_txs", cs.blockExec.GetMissingTxs(txKeys))
		return nil
	}
	block := cs.state.MakeBlock(height, txs, lastCommit, evidence, proposerAddress)
	block.Version = header.Version
	block.Data.Txs = txs
	block.DataHash = block.Data.Hash(true)
	block.Header.Time = header.Time
	block.Header.ProposerAddress = header.ProposerAddress
	return block
}

func (cs *State) handleCompleteProposal(ctx context.Context, height int64, handleBlockPartSpan otrace.Span) {
	// Update Valid* if we can.
	prevotes := cs.roundState.Votes().Prevotes(cs.roundState.Round())
	blockID, hasTwoThirds := prevotes.TwoThirdsMajority()
	if hasTwoThirds && !blockID.IsNil() && (cs.roundState.ValidRound() < cs.roundState.Round()) {
		if cs.roundState.ProposalBlock().HashesTo(blockID.Hash) {
			cs.logger.Debug(
				"updating valid block to new proposal block",
				"valid_round", cs.roundState.Round(),
				"valid_block_hash", cs.roundState.ProposalBlock().Hash(),
			)

			cs.roundState.SetValidRound(cs.roundState.Round())
			cs.roundState.SetValidBlock(cs.roundState.ProposalBlock())
			cs.roundState.SetValidBlockParts(cs.roundState.ProposalBlockParts())
		}
		// TODO: In case there is +2/3 majority in Prevotes set for some
		// block and cs.ProposalBlock contains different block, either
		// proposer is faulty or voting power of faulty processes is more
		// than 1/3. We should trigger in the future accountability
		// procedure at this point.
	}

	// Do not count prevote/precommit/commit into handleBlockPartMsg's span
	handleBlockPartSpan.End()

	if cs.roundState.Step() <= cstypes.RoundStepPropose && cs.isProposalComplete() {
		// Move onto the next step
		cs.enterPrevote(ctx, height, cs.roundState.Round(), "complete-proposal")
		if hasTwoThirds { // this is optimisation as this will be triggered when prevote is added
			cs.enterPrecommit(ctx, height, cs.roundState.Round(), "complete-proposal")
		}
	} else if cs.roundState.Step() == cstypes.RoundStepCommit {
		// If we're waiting on the proposal block...
		cs.tryFinalizeCommit(ctx, height)
	}
}

// Attempt to add the vote. if its a duplicate signature, dupeout the validator
func (cs *State) tryAddVote(ctx context.Context, vote *types.Vote, peerID types.NodeID, handleVoteMsgSpan otrace.Span) (bool, error) {
	added, err := cs.addVote(ctx, vote, peerID, handleVoteMsgSpan)
	if err != nil {
		// If the vote height is off, we'll just ignore it,
		// But if it's a conflicting sig, add it to the cs.evpool.
		// If it's otherwise invalid, punish peer.
		//nolint: gocritic
		if voteErr, ok := err.(*types.ErrVoteConflictingVotes); ok {
			if cs.privValidatorPubKey == nil {
				return false, errPubKeyIsNotSet
			}

			if bytes.Equal(vote.ValidatorAddress, cs.privValidatorPubKey.Address()) {
				cs.logger.Error(
					"found conflicting vote from ourselves; did you unsafe_reset a validator?",
					"height", vote.Height,
					"round", vote.Round,
					"type", vote.Type,
				)

				return added, err
			}

			// report conflicting votes to the evidence pool
			cs.evpool.ReportConflictingVotes(voteErr.VoteA, voteErr.VoteB)
			cs.logger.Debug(
				"found and sent conflicting votes to the evidence pool",
				"vote_a", voteErr.VoteA,
				"vote_b", voteErr.VoteB,
			)

			return added, err
		} else if errors.Is(err, types.ErrVoteNonDeterministicSignature) {
			cs.logger.Debug("vote has non-deterministic signature", "err", err)
		} else {
			// Either
			// 1) bad peer OR
			// 2) not a bad peer? this can also err sometimes with "Unexpected step" OR
			// 3) tmkms use with multiple validators connecting to a single tmkms instance
			//		(https://github.com/tendermint/tendermint/issues/3839).
			cs.logger.Info("failed attempting to add vote", "err", err)
			return added, ErrAddingVote
		}
	}

	return added, nil
}

func (cs *State) addVote(
	ctx context.Context,
	vote *types.Vote,
	peerID types.NodeID,
	handleVoteMsgSpan otrace.Span,
) (added bool, err error) {
	cs.logger.Debug(
		"adding vote",
		"vote_height", vote.Height,
		"vote_type", vote.Type,
		"val_index", vote.ValidatorIndex,
		"cs_height", cs.roundState.Height(),
	)
	if vote.Height < cs.roundState.Height() || (vote.Height == cs.roundState.Height() && vote.Round < cs.roundState.Round()) {
		cs.metrics.MarkLateVote(vote)
	}

	// A precommit for the previous height?
	// These come in while we wait timeoutCommit
	if vote.Height+1 == cs.roundState.Height() && vote.Type == tmproto.PrecommitType {
		if cs.roundState.Step() != cstypes.RoundStepNewHeight {
			// Late precommit at prior height is ignored
			cs.logger.Debug("precommit vote came in after commit timeout and has been ignored", "vote", vote)
			return
		}

		added, err = cs.roundState.LastCommit().AddVote(vote)
		if !added {
			return
		}

		cs.logger.Debug("added vote to last precommits", "last_commit", cs.roundState.LastCommit().StringShort())
		if err := cs.eventBus.PublishEventVote(types.EventDataVote{Vote: vote}); err != nil {
			return added, err
		}

		cs.evsw.FireEvent(types.EventVoteValue, vote)

		handleVoteMsgSpan.End()
		// if we can skip timeoutCommit and have all the votes now,
		if cs.bypassCommitTimeout() && cs.roundState.LastCommit().HasAll() {
			// go straight to new round (skip timeout commit)
			// cs.scheduleTimeout(time.Duration(0), cs.Height, 0, cstypes.RoundStepNewHeight)
			cs.enterNewRound(ctx, cs.roundState.Height(), 0, "skip-timeout")
		}

		return
	}

	// Height mismatch is ignored.
	// Not necessarily a bad peer, but not favorable behavior.
	if vote.Height != cs.roundState.Height() {
		cs.logger.Debug("vote ignored and not added", "vote_height", vote.Height, "cs_height", cs.roundState.Height(), "peer", peerID)
		return
	}

	// Check to see if the chain is configured to extend votes.
	if cs.state.ConsensusParams.ABCI.VoteExtensionsEnabled(cs.roundState.Height()) {
		// The chain is configured to extend votes, check that the vote is
		// not for a nil block and verify the extensions signature against the
		// corresponding public key.

		var myAddr []byte
		if cs.privValidatorPubKey != nil {
			myAddr = cs.privValidatorPubKey.Address()
		}
		// Verify VoteExtension if precommit and not nil
		// https://github.com/tendermint/tendermint/issues/8487
		if vote.Type == tmproto.PrecommitType && !vote.BlockID.IsNil() &&
			!bytes.Equal(vote.ValidatorAddress, myAddr) { // Skip the VerifyVoteExtension call if the vote was issued by this validator.

			// The core fields of the vote message were already validated in the
			// consensus reactor when the vote was received.
			// Here, we verify the signature of the vote extension included in the vote
			// message.
			_, val := cs.state.Validators.GetByIndex(vote.ValidatorIndex)
			if err := vote.VerifyExtension(cs.state.ChainID, val.PubKey); err != nil {
				return false, err
			}

			err := cs.blockExec.VerifyVoteExtension(ctx, vote)
			cs.metrics.MarkVoteExtensionReceived(err == nil)
			if err != nil {
				return false, err
			}
		}
	} else {
		// Vote extensions are not enabled on the network.
		// strip the extension data from the vote in case any is present.
		//
		// TODO punish a peer if it sent a vote with an extension when the feature
		// is disabled on the network.
		// https://github.com/tendermint/tendermint/issues/8565
		if stripped := vote.StripExtension(); stripped {
			cs.logger.Error("vote included extension data but vote extensions are not enabled", "peer", peerID)
		}
	}

	height := cs.roundState.Height()
	added, err = cs.roundState.Votes().AddVote(vote, peerID)
	if !added {
		// Either duplicate, or error upon cs.Votes.AddByIndex()
		return
	}
	if vote.Round == cs.roundState.Round() {
		vals := cs.state.Validators
		_, val := vals.GetByIndex(vote.ValidatorIndex)
		cs.metrics.MarkVoteReceived(vote.Type, val.VotingPower, vals.TotalVotingPower())
	}

	if err := cs.eventBus.PublishEventVote(types.EventDataVote{Vote: vote}); err != nil {
		return added, err
	}
	cs.evsw.FireEvent(types.EventVoteValue, vote)

	switch vote.Type {
	case tmproto.PrevoteType:
		prevotes := cs.roundState.Votes().Prevotes(vote.Round)
		cs.logger.Debug("added vote to prevote", "vote", vote, "prevotes", prevotes.StringShort())

		// Check to see if >2/3 of the voting power on the network voted for any non-nil block.
		if blockID, ok := prevotes.TwoThirdsMajority(); ok && !blockID.IsNil() {
			// Greater than 2/3 of the voting power on the network voted for some
			// non-nil block

			// Update Valid* if we can.
			if cs.roundState.ValidRound() < vote.Round && vote.Round == cs.roundState.Round() {
				if cs.roundState.ProposalBlock().HashesTo(blockID.Hash) {
					cs.logger.Debug("updating valid block because of POL", "valid_round", cs.roundState.ValidRound(), "pol_round", vote.Round)
					cs.roundState.SetValidRound(vote.Round)
					cs.roundState.SetValidBlock(cs.roundState.ProposalBlock())
					cs.roundState.SetValidBlockParts(cs.roundState.ProposalBlockParts())
				} else {
					cs.logger.Debug(
						"valid block we do not know about; set ProposalBlock=nil",
						"proposal", cs.roundState.ProposalBlock().Hash(),
						"block_id", blockID.Hash,
					)

					// we're getting the wrong block
					cs.roundState.SetProposalBlock(nil)
				}

				if !cs.roundState.ProposalBlockParts().HasHeader(blockID.PartSetHeader) {
					cs.metrics.MarkBlockGossipStarted()
					cs.roundState.SetProposalBlockParts(types.NewPartSetFromHeader(blockID.PartSetHeader))
				}

				roundState := cs.roundState.CopyInternal()
				cs.evsw.FireEvent(types.EventValidBlockValue, roundState)
				if err := cs.eventBus.PublishEventValidBlock(cs.roundState.RoundStateEvent()); err != nil {
					return added, err
				}
			}
		}

		handleVoteMsgSpan.End()
		// If +2/3 prevotes for *anything* for future round:
		switch {
		case cs.roundState.Round() < vote.Round && prevotes.HasTwoThirdsAny():
			// Round-skip if there is any 2/3+ of votes ahead of us
			cs.enterNewRound(ctx, height, vote.Round, "prevote-future")

		case cs.roundState.Round() == vote.Round && cstypes.RoundStepPrevote <= cs.roundState.Step(): // current round
			blockID, ok := prevotes.TwoThirdsMajority()
			if ok && (cs.isProposalComplete() || blockID.IsNil()) {
				cs.enterPrecommit(ctx, height, vote.Round, "prevote-future")
			} else if prevotes.HasTwoThirdsAny() {
				cs.enterPrevoteWait(height, vote.Round)
			}

		case cs.roundState.Proposal() != nil && 0 <= cs.roundState.Proposal().POLRound && cs.roundState.Proposal().POLRound == vote.Round:
			// If the proposal is now complete, enter prevote of cs.Round.
			if cs.isProposalComplete() {
				cs.enterPrevote(ctx, height, cs.roundState.Round(), "prevote-future")
			}
		}

	case tmproto.PrecommitType:
		precommits := cs.roundState.Votes().Precommits(vote.Round)
		cs.logger.Debug("added vote to precommit",
			"height", vote.Height,
			"round", vote.Round,
			"validator", vote.ValidatorAddress.String(),
			"vote_timestamp", vote.Timestamp,
			"data", precommits.LogString())

		blockID, ok := precommits.TwoThirdsMajority()
		handleVoteMsgSpan.End()
		if ok {
			// Executed as TwoThirdsMajority could be from a higher round
			cs.enterNewRound(ctx, height, vote.Round, "precommit-two-thirds")
			cs.enterPrecommit(ctx, height, vote.Round, "precommit-two-thirds")

			if !blockID.IsNil() {
				cs.enterCommit(ctx, height, vote.Round, "precommit-two-thirds")
				if cs.bypassCommitTimeout() && precommits.HasAll() {
					cs.enterNewRound(ctx, cs.roundState.Height(), 0, "precommit-skip-round")
				}
			} else {
				cs.enterPrecommitWait(height, vote.Round)
			}
		} else if cs.roundState.Round() <= vote.Round && precommits.HasTwoThirdsAny() {
			cs.enterNewRound(ctx, height, vote.Round, "precommit-two-thirds-any")
			cs.enterPrecommitWait(height, vote.Round)
		}

	default:
		panic(fmt.Sprintf("unexpected vote type %v", vote.Type))
	}

	return added, err
}

// CONTRACT: cs.privValidator is not nil.
func (cs *State) signVote(
	ctx context.Context,
	msgType tmproto.SignedMsgType,
	hash []byte,
	header types.PartSetHeader,
) (*types.Vote, error) {
	// Flush the WAL. Otherwise, we may not recompute the same vote to sign,
	// and the privValidator will refuse to sign anything.
	if err := cs.wal.FlushAndSync(); err != nil {
		return nil, err
	}

	if cs.privValidatorPubKey == nil {
		return nil, errPubKeyIsNotSet
	}

	addr := cs.privValidatorPubKey.Address()
	valIdx, _ := cs.roundState.Validators().GetByAddress(addr)

	vote := &types.Vote{
		ValidatorAddress: addr,
		ValidatorIndex:   valIdx,
		Height:           cs.roundState.Height(),
		Round:            cs.roundState.Round(),
		Timestamp:        tmtime.Now(),
		Type:             msgType,
		BlockID:          types.BlockID{Hash: hash, PartSetHeader: header},
	}

	// If the signedMessageType is for precommit,
	// use our local precommit Timeout as the max wait time for getting a singed commit. The same goes for prevote.
	timeout := time.Second
	if msgType == tmproto.PrecommitType && !vote.BlockID.IsNil() {
		timeout = cs.voteTimeout(cs.roundState.Round())
		// if the signedMessage type is for a non-nil precommit, add
		// VoteExtension
		if cs.state.ConsensusParams.ABCI.VoteExtensionsEnabled(cs.roundState.Height()) {
			ext, err := cs.blockExec.ExtendVote(ctx, vote)
			if err != nil {
				return nil, err
			}
			vote.Extension = ext
		}
	}

	v := vote.ToProto()

	ctxto, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	err := cs.privValidator.SignVote(ctxto, cs.state.ChainID, v)
	vote.Signature = v.Signature
	vote.ExtensionSignature = v.ExtensionSignature
	vote.Timestamp = v.Timestamp

	return vote, err
}

// sign the vote and publish on internalMsgQueue
func (cs *State) signAddVote(
	ctx context.Context,
	msgType tmproto.SignedMsgType,
	hash []byte,
	header types.PartSetHeader,
) *types.Vote {
	if cs.privValidator == nil { // the node does not have a key
		return nil
	}

	if cs.privValidatorPubKey == nil {
		// Vote won't be signed, but it's not critical.
		cs.logger.Error("signAddVote", "err", errPubKeyIsNotSet)
		return nil
	}

	// If the node not in the validator set, do nothing.
	if !cs.roundState.Validators().HasAddress(cs.privValidatorPubKey.Address()) {
		return nil
	}

	// TODO: pass pubKey to signVote
	vote, err := cs.signVote(ctx, msgType, hash, header)
	if err != nil {
		cs.logger.Error("failed signing vote", "height", cs.roundState.Height(), "round", cs.roundState.Round(), "vote", vote, "err", err)
		return nil
	}
	if !cs.state.ConsensusParams.ABCI.VoteExtensionsEnabled(vote.Height) {
		// The signer will sign the extension, make sure to remove the data on the way out
		vote.StripExtension()
	}
	cs.sendInternalMessage(ctx, msgInfo{&VoteMessage{vote}, "", tmtime.Now()})
	cs.logger.Info("signed and pushed vote", "height", cs.roundState.Height(), "round", cs.roundState.Round(), "vote", vote)
	return vote
}

// updatePrivValidatorPubKey get's the private validator public key and
// memoizes it. This func returns an error if the private validator is not
// responding or responds with an error.
func (cs *State) updatePrivValidatorPubKey(rctx context.Context) error {
	if cs.privValidator == nil {
		return nil
	}

	timeout := cs.voteTimeout(cs.roundState.Round())

	// no GetPubKey retry beyond the proposal/voting in RetrySignerClient
	if cs.roundState.Step() >= cstypes.RoundStepPrecommit && cs.privValidatorType == types.RetrySignerClient {
		timeout = 0
	}

	// set context timeout depending on the configuration and the State step,
	// this helps in avoiding blocking of the remote signer connection.
	ctxto, cancel := context.WithTimeout(rctx, timeout)
	defer cancel()
	pubKey, err := cs.privValidator.GetPubKey(ctxto)
	if err != nil {
		return err
	}
	cs.privValidatorPubKey = pubKey
	return nil
}

// look back to check existence of the node's consensus votes before joining consensus
func (cs *State) checkDoubleSigningRisk(height int64) error {
	if cs.privValidator != nil && cs.privValidatorPubKey != nil && cs.config.DoubleSignCheckHeight > 0 && height > 0 {
		valAddr := cs.privValidatorPubKey.Address()
		doubleSignCheckHeight := cs.config.DoubleSignCheckHeight
		if doubleSignCheckHeight > height {
			doubleSignCheckHeight = height
		}

		for i := int64(1); i < doubleSignCheckHeight; i++ {
			lastCommit := cs.LoadCommit(height - i)
			if lastCommit != nil {
				for sigIdx, s := range lastCommit.Signatures {
					if s.BlockIDFlag == types.BlockIDFlagCommit && bytes.Equal(s.ValidatorAddress, valAddr) {
						cs.logger.Info("found signature from the same key", "sig", s, "idx", sigIdx, "height", height-i)
						return ErrSignatureFoundInPastBlocks
					}
				}
			}
		}
	}

	return nil
}

func (cs *State) calculatePrevoteMessageDelayMetrics() {
	if cs.roundState.Proposal() == nil {
		return
	}
	ps := cs.roundState.Votes().Prevotes(cs.roundState.Round())
	pl := ps.List()

	sort.Slice(pl, func(i, j int) bool {
		return pl[i].Timestamp.Before(pl[j].Timestamp)
	})

	var votingPowerSeen int64
	for _, v := range pl {
		_, val := cs.roundState.Validators().GetByAddress(v.ValidatorAddress)
		votingPowerSeen += val.VotingPower
		if votingPowerSeen >= cs.roundState.Validators().TotalVotingPower()*2/3+1 {
			cs.metrics.QuorumPrevoteDelay.With("proposer_address", cs.roundState.Validators().GetProposer().Address.String()).Set(v.Timestamp.Sub(cs.roundState.Proposal().Timestamp).Seconds())
			break
		}
	}
	if ps.HasAll() {
		cs.metrics.FullPrevoteDelay.With("proposer_address", cs.roundState.Validators().GetProposer().Address.String()).Set(pl[len(pl)-1].Timestamp.Sub(cs.roundState.Proposal().Timestamp).Seconds())
	}
}

//---------------------------------------------------------

func CompareHRS(h1 int64, r1 int32, s1 cstypes.RoundStepType, h2 int64, r2 int32, s2 cstypes.RoundStepType) int {
	if h1 < h2 {
		return -1
	} else if h1 > h2 {
		return 1
	}
	if r1 < r2 {
		return -1
	} else if r1 > r2 {
		return 1
	}
	if s1 < s2 {
		return -1
	} else if s1 > s2 {
		return 1
	}
	return 0
}

// repairWalFile decodes messages from src (until the decoder errors) and
// writes them to dst.
func repairWalFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	var (
		dec = NewWALDecoder(in)
		enc = NewWALEncoder(out)
	)

	// best-case repair (until first error is encountered)
	for {
		msg, err := dec.Decode()
		if err != nil {
			break
		}

		err = enc.Encode(msg)
		if err != nil {
			return fmt.Errorf("failed to encode msg: %w", err)
		}
	}

	return nil
}

func (cs *State) proposeTimeout(round int32) time.Duration {
	tp := cs.state.ConsensusParams.Timeout.TimeoutParamsOrDefaults()
	p := tp.Propose
	if cs.config.UnsafeProposeTimeoutOverride != 0 {
		p = cs.config.UnsafeProposeTimeoutOverride
	}
	pd := tp.ProposeDelta
	if cs.config.UnsafeProposeTimeoutDeltaOverride != 0 {
		pd = cs.config.UnsafeProposeTimeoutDeltaOverride
	}
	return time.Duration(
		p.Nanoseconds()+pd.Nanoseconds()*int64(round),
	) * time.Nanosecond
}

func (cs *State) voteTimeout(round int32) time.Duration {
	tp := cs.state.ConsensusParams.Timeout.TimeoutParamsOrDefaults()
	v := tp.Vote
	if cs.config.UnsafeVoteTimeoutOverride != 0 {
		v = cs.config.UnsafeVoteTimeoutOverride
	}
	vd := tp.VoteDelta
	if cs.config.UnsafeVoteTimeoutDeltaOverride != 0 {
		vd = cs.config.UnsafeVoteTimeoutDeltaOverride
	}
	return time.Duration(
		v.Nanoseconds()+vd.Nanoseconds()*int64(round),
	) * time.Nanosecond
}

func (cs *State) commitTime(t time.Time) time.Time {
	c := cs.state.ConsensusParams.Timeout.Commit
	if cs.config.UnsafeCommitTimeoutOverride != 0 {
		c = cs.config.UnsafeCommitTimeoutOverride
	}
	return t.Add(c)
}

func (cs *State) bypassCommitTimeout() bool {
	if cs.config.UnsafeBypassCommitTimeoutOverride != nil {
		return *cs.config.UnsafeBypassCommitTimeoutOverride
	}
	return cs.state.ConsensusParams.Timeout.BypassCommitTimeout
}

func (cs *State) calculateProposalTimestampDifferenceMetric() {
	if cs.roundState.Proposal() != nil && cs.roundState.Proposal().POLRound == -1 {
		sp := cs.state.ConsensusParams.Synchrony.SynchronyParamsOrDefaults()
		isTimely := cs.roundState.Proposal().IsTimely(cs.roundState.ProposalReceiveTime(), sp, cs.roundState.Round())
		cs.metrics.ProposalTimestampDifference.With("is_timely", fmt.Sprintf("%t", isTimely)).
			Observe(cs.roundState.ProposalReceiveTime().Sub(cs.roundState.Proposal().Timestamp).Seconds())
	}
}

// proposerWaitTime determines how long the proposer should wait to propose its next block.
// If the result is zero, a block can be proposed immediately.
//
// Block times must be monotonically increasing, so if the block time of the previous
// block is larger than the proposer's current time, then the proposer will sleep
// until its local clock exceeds the previous block time.
func proposerWaitTime(lt tmtime.Source, bt time.Time) time.Duration {
	t := lt.Now()
	if bt.After(t) {
		return bt.Sub(t)
	}
	return 0
}
