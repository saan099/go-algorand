// Copyright (C) 2019-2020 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package ledger

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/algorand/go-deadlock"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/crypto/merkletrie"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/logging/telemetryspec"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/util/db"
)

const (
	// balancesFlushInterval defines how frequently we want to flush our balances to disk.
	balancesFlushInterval = 5 * time.Second
	// pendingDeltasFlushThreshold is the deltas count threshold above we flush the pending balances regardless of the flush interval.
	pendingDeltasFlushThreshold = 128
	// trieRebuildAccountChunkSize defines the number of accounts that would get read at a single chunk
	// before added to the trie during trie construction
	trieRebuildAccountChunkSize = 16384
	// trieRebuildCommitFrequency defines the number of accounts that would get added before we call evict to commit the changes and adjust the memory cache.
	trieRebuildCommitFrequency = 65536
	// trieAccumulatedChangesFlush defines the number of pending changes that would be applied to the merkle trie before
	// we attempt to commit them to disk while writing a batch of rounds balances to disk.
	trieAccumulatedChangesFlush = 256
)

// trieCachedNodesCount defines how many balances trie nodes we would like to keep around in memory.
// value was calibrated using BenchmarkCalibrateCacheNodeSize
var trieCachedNodesCount = 9000

// A modifiedAccount represents an account that has been modified since
// the persistent state stored in the account DB (i.e., in the range of
// rounds covered by the accountUpdates tracker).
type modifiedAccount struct {
	// data stores the most recent AccountData for this modified
	// account.
	data basics.AccountData

	// ndelta keeps track of how many times this account appears in
	// accountUpdates.deltas.  This is used to evict modifiedAccount
	// entries when all changes to an account have been reflected in
	// the account DB, and no outstanding modifications remain.
	ndeltas int
}

type modifiedCreatable struct {
	// Type of the creatable: app or asset
	ctype basics.CreatableType

	// Created if true, deleted if false
	created bool

	// creator of the app/asset
	creator basics.Address

	// Keeps track of how many times this app/asset appears in
	// accountUpdates.creatableDeltas
	ndeltas int
}

type accountUpdates struct {
	// constant variables ( initialized on initialize, and never changed afterward )

	// initAccounts specifies initial account values for database.
	initAccounts map[basics.Address]basics.AccountData

	// initProto specifies the initial consensus parameters.
	initProto config.ConsensusParams

	// dbDirectory is the directory where the ledger and block sql file resides as well as the parent directroy for the catchup files to be generated
	dbDirectory string

	// catchpointInterval is the configured interval at which the accountUpdates would generate catchpoint labels and catchpoint files.
	catchpointInterval uint64

	// archivalLedger determines whether the associated ledger was configured as archival ledger or not.
	archivalLedger bool

	// catchpointFileHistoryLength defines how many catchpoint files we want to store back.
	// 0 means don't store any, -1 mean unlimited and positive number suggest the number of most recent catchpoint files.
	catchpointFileHistoryLength int

	// vacuumOnStartup controls whether the accounts database would get vacuumed on startup.
	vacuumOnStartup bool

	// dynamic variables

	// Connection to the database.
	dbs dbPair

	// Prepared SQL statements for fast accounts DB lookups.
	accountsq *accountsDbQueries

	// dbRound is always exactly accountsRound(),
	// cached to avoid SQL queries.
	dbRound basics.Round

	// deltas stores updates for every round after dbRound.
	deltas []map[basics.Address]accountDelta

	// accounts stores the most recent account state for every
	// address that appears in deltas.
	accounts map[basics.Address]modifiedAccount

	// creatableDeltas stores creatable updates for every round after dbRound.
	creatableDeltas []map[basics.CreatableIndex]modifiedCreatable

	// creatables stores the most recent state for every creatable that
	// appears in creatableDeltas
	creatables map[basics.CreatableIndex]modifiedCreatable

	// protos stores consensus parameters dbRound and every
	// round after it; i.e., protos is one longer than deltas.
	protos []config.ConsensusParams

	// totals stores the totals for dbRound and every round after it;
	// i.e., totals is one longer than deltas.
	roundTotals []AccountTotals

	// roundDigest stores the digest of the block for every round starting with dbRound and every round after it.
	roundDigest []crypto.Digest

	// log copied from ledger
	log logging.Logger

	// lastFlushTime is the time we last flushed updates to
	// the accounts DB (bumping dbRound).
	lastFlushTime time.Time

	// ledger is the source ledger, which is used to syncronize
	// the rounds at which we need to flush the balances to disk
	// in favor of the catchpoint to be generated.
	ledger ledgerForTracker

	// The Trie tracking the current account balances. Always matches the balances that were
	// written to the database.
	balancesTrie *merkletrie.Trie

	// The last catchpoint label that was writted to the database. Should always align with what's in the database.
	// note that this is the last catchpoint *label* and not the catchpoint file.
	lastCatchpointLabel string

	// catchpointWriting help to syncronize the catchpoint file writing. When this channel is closed, no writting is going on.
	// the channel is non-closed while writing the current accounts state to disk.
	catchpointWriting chan struct{}

	// catchpointSlowWriting suggest to the accounts writer that it should finish writing up the catchpoint file ASAP.
	// when this channel is closed, the accounts writer would try and complete the writing as soon as possible.
	// otherwise, it would take it's time and perform periodic sleeps between chunks processing.
	catchpointSlowWriting chan struct{}

	// ctx is the context for the committing go-routine. It's also used as the "parent" of the catchpoint generation operation.
	ctx context.Context

	// ctxCancel is the canceling function for canceling the commiting go-routine ( i.e. signaling the commiting go-routine that it's time to abort )
	ctxCancel context.CancelFunc

	// deltasAccum stores the accumulated deltas for every round starting dbRound-1.
	deltasAccum []int

	// committedOffset is the offset at which we'd like to persist all the previous account information to disk.
	committedOffset chan deferedCommit

	// accountsMu is the syncronization mutex for accessing the various non-static varaibles.
	accountsMu deadlock.RWMutex

	// accountsWriting provides syncronization around the background writing of account balances.
	accountsWriting sync.WaitGroup

	// commitSyncerClosed is the blocking channel for syncronizing closing the commitSyncer goroutine. Once it's closed, the
	// commitSyncer can be assumed to have aborted.
	commitSyncerClosed chan struct{}
}

type deferedCommit struct {
	offset   uint64
	dbRound  basics.Round
	lookback basics.Round
}

// initialize initializes the accountUpdates structure
func (au *accountUpdates) initialize(cfg config.Local, dbPathPrefix string, genesisProto config.ConsensusParams, genesisAccounts map[basics.Address]basics.AccountData) {
	au.initProto = genesisProto
	au.initAccounts = genesisAccounts
	au.dbDirectory = filepath.Dir(dbPathPrefix)
	au.archivalLedger = cfg.Archival
	au.catchpointInterval = cfg.CatchpointInterval
	au.catchpointFileHistoryLength = cfg.CatchpointFileHistoryLength
	if cfg.CatchpointFileHistoryLength < -1 {
		au.catchpointFileHistoryLength = -1
	}
	au.vacuumOnStartup = cfg.OptimizeAccountsDatabaseOnStartup
	// initialize the commitSyncerClosed with a closed channel ( since the commitSyncer go-routine is not active )
	au.commitSyncerClosed = make(chan struct{})
	close(au.commitSyncerClosed)
}

// loadFromDisk is the 2nd level initialization, and is required before the accountUpdates becomes functional
// The close function is expected to be call in pair with loadFromDisk
func (au *accountUpdates) loadFromDisk(l ledgerForTracker) error {
	au.accountsMu.Lock()
	defer au.accountsMu.Unlock()
	var writingCatchpointRound uint64
	lastBalancesRound, lastestBlockRound, err := au.initializeFromDisk(l)

	if err != nil {
		return err
	}

	var writingCatchpointDigest crypto.Digest

	writingCatchpointRound, _, err = au.accountsq.readCatchpointStateUint64(context.Background(), catchpointStateWritingCatchpoint)
	if err != nil {
		return err
	}

	writingCatchpointDigest, err = au.initializeCaches(lastBalancesRound, lastestBlockRound, basics.Round(writingCatchpointRound))
	if err != nil {
		return err
	}

	if writingCatchpointRound != 0 && au.catchpointInterval != 0 {
		au.generateCatchpoint(basics.Round(writingCatchpointRound), au.lastCatchpointLabel, writingCatchpointDigest, time.Duration(0))
	}

	return nil
}

// waitAccountsWriting waits for all the pending ( or current ) account writing to be completed.
func (au *accountUpdates) waitAccountsWriting() {
	au.accountsWriting.Wait()
}

// close closes the accountUpdates, waiting for all the child go-routine to complete
func (au *accountUpdates) close() {
	if au.ctxCancel != nil {
		au.ctxCancel()
	}
	au.waitAccountsWriting()
	// this would block until the commitSyncerClosed channel get closed.
	<-au.commitSyncerClosed
}

// Lookup returns the accound data for a given address at a given round. The withRewards indicates whether the
// rewards should be added to the AccountData before returning. Note that the function doesn't update the account with the rewards,
// even while it could return the AccoutData which represent the "rewarded" account data.
func (au *accountUpdates) Lookup(rnd basics.Round, addr basics.Address, withRewards bool) (data basics.AccountData, err error) {
	au.accountsMu.RLock()
	defer au.accountsMu.RUnlock()
	return au.lookupImpl(rnd, addr, withRewards)
}

// ListAssets lists the assets by their asset index, limiting to the first maxResults
func (au *accountUpdates) ListAssets(maxAssetIdx basics.AssetIndex, maxResults uint64) ([]basics.CreatableLocator, error) {
	return au.listCreatables(basics.CreatableIndex(maxAssetIdx), maxResults, basics.AssetCreatable)
}

// ListApplications lists the application by their app index, limiting to the first maxResults
func (au *accountUpdates) ListApplications(maxAppIdx basics.AppIndex, maxResults uint64) ([]basics.CreatableLocator, error) {
	return au.listCreatables(basics.CreatableIndex(maxAppIdx), maxResults, basics.AppCreatable)
}

// listCreatables lists the application/asset by their app/asset index, limiting to the first maxResults
func (au *accountUpdates) listCreatables(maxCreatableIdx basics.CreatableIndex, maxResults uint64, ctype basics.CreatableType) ([]basics.CreatableLocator, error) {
	au.accountsMu.RLock()
	defer au.accountsMu.RUnlock()

	// Sort indices for creatables that have been created/deleted. If this
	// turns out to be too inefficient, we could keep around a heap of
	// created/deleted asset indices in memory.
	keys := make([]basics.CreatableIndex, 0, len(au.creatables))
	for cidx, delta := range au.creatables {
		if delta.ctype != ctype {
			continue
		}
		if cidx <= maxCreatableIdx {
			keys = append(keys, cidx)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] > keys[j] })

	// Check for creatables that haven't been synced to disk yet.
	var unsyncedCreatables []basics.CreatableLocator
	deletedCreatables := make(map[basics.CreatableIndex]bool)
	for _, cidx := range keys {
		delta := au.creatables[cidx]
		if delta.created {
			// Created but only exists in memory
			unsyncedCreatables = append(unsyncedCreatables, basics.CreatableLocator{
				Type:    delta.ctype,
				Index:   cidx,
				Creator: delta.creator,
			})
		} else {
			// Mark deleted creatables for exclusion from the results set
			deletedCreatables[cidx] = true
		}
	}

	// Check in-memory created creatables, which will always be newer than anything
	// in the database
	var res []basics.CreatableLocator
	for _, loc := range unsyncedCreatables {
		if uint64(len(res)) == maxResults {
			return res, nil
		}
		res = append(res, loc)
	}

	// Fetch up to maxResults - len(res) + len(deletedCreatables) from the database,
	// so we have enough extras in case creatables were deleted
	numToFetch := maxResults - uint64(len(res)) + uint64(len(deletedCreatables))
	dbResults, err := au.accountsq.listCreatables(maxCreatableIdx, numToFetch, ctype)
	if err != nil {
		return nil, err
	}

	// Now we merge the database results with the in-memory results
	for _, loc := range dbResults {
		// Check if we have enough results
		if uint64(len(res)) == maxResults {
			return res, nil
		}

		// Creatable was deleted
		if _, ok := deletedCreatables[loc.Index]; ok {
			continue
		}

		// We're OK to include this result
		res = append(res, loc)
	}

	return res, nil
}

// GetLastCatchpointLabel retrieves the last catchpoint label that was stored to the database.
func (au *accountUpdates) GetLastCatchpointLabel() string {
	au.accountsMu.RLock()
	defer au.accountsMu.RUnlock()
	return au.lastCatchpointLabel
}

// GetCreatorForRound returns the creator for a given asset/app index at a given round
func (au *accountUpdates) GetCreatorForRound(rnd basics.Round, cidx basics.CreatableIndex, ctype basics.CreatableType) (creator basics.Address, ok bool, err error) {
	au.accountsMu.RLock()
	defer au.accountsMu.RUnlock()
	return au.getCreatorForRoundImpl(rnd, cidx, ctype)
}

// committedUpTo enqueues commiting the balances for round committedRound-lookback.
// The defered committing is done so that we could calculate the historical balances lookback rounds back.
// Since we don't want to hold off the tracker's mutex for too long, we'll defer the database persistance of this
// operation to a syncer goroutine. The one caviat is that when storing a catchpoint round, we would want to
// wait until the catchpoint creation is done, so that the persistance of the catchpoint file would have an
// uninterrupted view of the balances at a given point of time.
func (au *accountUpdates) committedUpTo(committedRound basics.Round) (retRound basics.Round) {
	var isCatchpointRound, hasMultipleIntermediateCatchpoint bool
	var offset uint64
	var dc deferedCommit
	au.accountsMu.RLock()
	defer func() {
		au.accountsMu.RUnlock()
		if dc.offset != 0 {
			au.committedOffset <- dc
		}
	}()

	retRound = basics.Round(0)
	var pendingDeltas int

	lookback := basics.Round(au.protos[len(au.protos)-1].MaxBalLookback)
	if committedRound < lookback {
		return
	}

	retRound = au.dbRound
	newBase := committedRound - lookback
	if newBase <= au.dbRound {
		// Already forgotten
		return
	}

	if newBase > au.dbRound+basics.Round(len(au.deltas)) {
		au.log.Panicf("committedUpTo: block %d too far in the future, lookback %d, dbRound %d, deltas %d", committedRound, lookback, au.dbRound, len(au.deltas))
	}

	hasIntermediateCatchpoint := false
	hasMultipleIntermediateCatchpoint = false
	// check if there was a catchpoint between au.dbRound+lookback and newBase+lookback
	if au.catchpointInterval > 0 {
		nextCatchpointRound := ((uint64(au.dbRound+lookback) + au.catchpointInterval) / au.catchpointInterval) * au.catchpointInterval

		if nextCatchpointRound < uint64(newBase+lookback) {
			mostRecentCatchpointRound := (uint64(committedRound) / au.catchpointInterval) * au.catchpointInterval
			newBase = basics.Round(nextCatchpointRound) - lookback
			if mostRecentCatchpointRound > nextCatchpointRound {
				hasMultipleIntermediateCatchpoint = true
				// skip if there is more than one catchpoint in queue
				newBase = basics.Round(mostRecentCatchpointRound) - lookback
			}
			hasIntermediateCatchpoint = true
		}
	}

	// if we're still writing the previous balances, we can't move forward yet.
	select {
	case <-au.catchpointWriting:
		// the channel catchpointWriting is currently closed, meaning that we're currently not writing any
		// catchpoint file. At this point, we should attempt to enqueue further tasks as usual.
	default:
		// if we hit this path, it means that the channel is currently non-closed, which means that we're still writing a catchpoint.
		// see if we're writing a catchpoint in that range.
		if hasIntermediateCatchpoint {
			// check if we're already attempting to perform fast-writing.
			select {
			case <-au.catchpointSlowWriting:
				// yes, we're already doing fast-writing.
			default:
				// no, we're not yet doing fast writing, make it so.
				close(au.catchpointSlowWriting)
			}
		}
		return
	}

	offset = uint64(newBase - au.dbRound)

	// check to see if this is a catchpoint round
	isCatchpointRound = ((offset + uint64(lookback+au.dbRound)) > 0) && (au.catchpointInterval != 0) && (0 == (uint64((offset + uint64(lookback+au.dbRound))) % au.catchpointInterval))

	// calculate the number of pending deltas
	pendingDeltas = au.deltasAccum[offset] - au.deltasAccum[0]

	// If we recently flushed, wait to aggregate some more blocks.
	// ( unless we're creating a catchpoint, in which case we want to flush it right away
	//   so that all the instances of the catchpoint would contain the exacy same data )
	flushTime := time.Now()
	if !flushTime.After(au.lastFlushTime.Add(balancesFlushInterval)) && !isCatchpointRound && pendingDeltas < pendingDeltasFlushThreshold {
		return au.dbRound
	}

	if isCatchpointRound && au.archivalLedger {
		au.catchpointWriting = make(chan struct{}, 1)
		au.catchpointSlowWriting = make(chan struct{}, 1)
		if hasMultipleIntermediateCatchpoint {
			close(au.catchpointSlowWriting)
		}
	}

	dc = deferedCommit{
		offset:   offset,
		dbRound:  au.dbRound,
		lookback: lookback,
	}
	au.accountsWriting.Add(1)
	return
}

// newBlock is the accountUpdates implementation of the ledgerTracker interface. This is the "external" facing function
// which invokes the internal implementation after taking the lock.
func (au *accountUpdates) newBlock(blk bookkeeping.Block, delta StateDelta) {
	au.accountsMu.Lock()
	defer au.accountsMu.Unlock()
	au.newBlockImpl(blk, delta)
}

// Totals returns the totals for a given round
func (au *accountUpdates) Totals(rnd basics.Round) (totals AccountTotals, err error) {
	au.accountsMu.RLock()
	defer au.accountsMu.RUnlock()
	return au.totalsImpl(rnd)
}

// GetCatchpointStream returns an io.Reader to the catchpoint file associated with the provided round
func (au *accountUpdates) GetCatchpointStream(round basics.Round) (io.ReadCloser, error) {
	dbFileName := ""
	err := au.dbs.rdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		dbFileName, _, _, err = getCatchpoint(tx, round)
		return
	})
	if err != nil && err != sql.ErrNoRows {
		// we had some sql error.
		return nil, fmt.Errorf("accountUpdates: getCatchpointStream: unable to lookup catchpoint %d: %v", round, err)
	}
	if dbFileName != "" {
		catchpointPath := filepath.Join(au.dbDirectory, dbFileName)
		file, err := os.OpenFile(catchpointPath, os.O_RDONLY, 0666)
		if err == nil && file != nil {
			return file, nil
		}
		// else, see if this is a file-not-found error
		if os.IsNotExist(err) {
			// the database told us that we have this file.. but we couldn't find it.
			// delete it from the database.
			err := au.saveCatchpointFile(round, "", 0, "")
			if err != nil {
				au.log.Warnf("accountUpdates: getCatchpointStream: unable to delete missing catchpoint entry: %v", err)
				return nil, err
			}

			return nil, ErrNoEntry{}
		}
		// it's some other error.
		return nil, fmt.Errorf("accountUpdates: getCatchpointStream: unable to open catchpoint file '%s' %v", catchpointPath, err)
	}

	// if the database doesn't know about that round, see if we have that file anyway:
	fileName := filepath.Join("catchpoints", catchpointRoundToPath(round))
	catchpointPath := filepath.Join(au.dbDirectory, fileName)
	file, err := os.OpenFile(catchpointPath, os.O_RDONLY, 0666)
	if err == nil && file != nil {
		// great, if found that we should have had this in the database.. add this one now :
		fileInfo, err := file.Stat()
		if err != nil {
			// we couldn't get the stat, so just return with the file.
			return file, nil
		}

		err = au.saveCatchpointFile(round, fileName, fileInfo.Size(), "")
		if err != nil {
			au.log.Warnf("accountUpdates: getCatchpointStream: unable to save missing catchpoint entry: %v", err)
		}
		return file, nil
	}
	return nil, ErrNoEntry{}
}

// functions below this line are all internal functions

// accountUpdatesLedgerEvaluator is a "ledger emulator" which is used *only* by initializeCaches, as a way to shortcut
// the locks taken by the real ledger object when making requests that are being served by the accountUpdates.
// Using this struct allow us to take the tracker lock *before* calling the loadFromDisk, and having the operation complete
// without taking any locks. Note that it's not only the locks performance that is gained : by having the loadFrom disk
// not requiring any external locks, we can safely take a trackers lock on the ledger during reloadLedger, which ensures
// that even during catchpoint catchup mode switch, we're still correctly protected by a mutex.
type accountUpdatesLedgerEvaluator struct {
	// au is the associated accountUpdates structure which invoking the trackerEvalVerified function, passing this structure as input.
	// the accountUpdatesLedgerEvaluator would access the underlying accountUpdates function directly, bypassing the balances mutex lock.
	au *accountUpdates
	// prevHeader is the previous header to the current one. The usage of this is only in the context of initializeCaches where we iteratively
	// building the StateDelta, which requires a peek on the "previous" header information.
	prevHeader bookkeeping.BlockHeader
}

// GenesisHash returns the genesis hash
func (aul *accountUpdatesLedgerEvaluator) GenesisHash() crypto.Digest {
	return aul.au.ledger.GenesisHash()
}

// BlockHdr returns the header of the given round. When the evaluator is running, it's only referring to the previous header, which is what we
// are providing here. Any attempt to access a different header would get denied.
func (aul *accountUpdatesLedgerEvaluator) BlockHdr(r basics.Round) (bookkeeping.BlockHeader, error) {
	if r == aul.prevHeader.Round {
		return aul.prevHeader, nil
	}
	return bookkeeping.BlockHeader{}, ErrNoEntry{}
}

// Lookup returns the account balance for a given address at a given round
func (aul *accountUpdatesLedgerEvaluator) Lookup(rnd basics.Round, addr basics.Address) (basics.AccountData, error) {
	return aul.au.lookupImpl(rnd, addr, true)
}

// Totals returns the totals for a given round
func (aul *accountUpdatesLedgerEvaluator) Totals(rnd basics.Round) (AccountTotals, error) {
	return aul.au.totalsImpl(rnd)
}

// isDup return whether a transaction is a duplicate one. It's not needed by the accountUpdatesLedgerEvaluator and implemeted as a stub.
func (aul *accountUpdatesLedgerEvaluator) isDup(config.ConsensusParams, basics.Round, basics.Round, basics.Round, transactions.Txid, txlease) (bool, error) {
	// this is a non-issue since this call will never be made on non-validating evaluation
	return false, fmt.Errorf("accountUpdatesLedgerEvaluator: tried to check for dup during accountUpdates initilization ")
}

// LookupWithoutRewards returns the account balance for a given address at a given round, without the reward
func (aul *accountUpdatesLedgerEvaluator) LookupWithoutRewards(rnd basics.Round, addr basics.Address) (basics.AccountData, error) {
	return aul.au.lookupImpl(rnd, addr, false)
}

// GetCreatorForRound returns the asset/app creator for a given asset/app index at a given round
func (aul *accountUpdatesLedgerEvaluator) GetCreatorForRound(rnd basics.Round, cidx basics.CreatableIndex, ctype basics.CreatableType) (creator basics.Address, ok bool, err error) {
	return aul.au.getCreatorForRoundImpl(rnd, cidx, ctype)
}

// totalsImpl returns the totals for a given round
func (au *accountUpdates) totalsImpl(rnd basics.Round) (totals AccountTotals, err error) {
	offset, err := au.roundOffset(rnd)
	if err != nil {
		return
	}

	totals = au.roundTotals[offset]
	return
}

// initializeCaches fills up the accountUpdates cache with the most recent ~320 blocks
func (au *accountUpdates) initializeCaches(lastBalancesRound, lastestBlockRound, writingCatchpointRound basics.Round) (catchpointBlockDigest crypto.Digest, err error) {
	var blk bookkeeping.Block
	var delta StateDelta

	accLedgerEval := accountUpdatesLedgerEvaluator{
		au: au,
	}
	if lastBalancesRound < lastestBlockRound {
		accLedgerEval.prevHeader, err = au.ledger.BlockHdr(lastBalancesRound)
		if err != nil {
			return
		}
	}

	for lastBalancesRound < lastestBlockRound {
		next := lastBalancesRound + 1

		blk, err = au.ledger.Block(next)
		if err != nil {
			return
		}

		delta, err = au.ledger.trackerEvalVerified(blk, &accLedgerEval)
		if err != nil {
			return
		}

		au.newBlockImpl(blk, delta)
		lastBalancesRound = next

		if next == basics.Round(writingCatchpointRound) {
			catchpointBlockDigest = blk.Digest()
		}

		accLedgerEval.prevHeader = *delta.hdr
	}
	return
}

// initializeFromDisk performs the atomic operation of loading the accounts data information from disk
// and preparing the accountUpdates for operation, including initlizating the commitSyncer goroutine.
func (au *accountUpdates) initializeFromDisk(l ledgerForTracker) (lastBalancesRound, lastestBlockRound basics.Round, err error) {
	au.dbs = l.trackerDB()
	au.log = l.trackerLog()
	au.ledger = l

	if au.initAccounts == nil {
		err = fmt.Errorf("accountUpdates.initializeFromDisk: initAccounts not set")
		return
	}

	lastestBlockRound = l.Latest()
	err = au.dbs.wdb.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		var err0 error
		au.dbRound, err0 = au.accountsInitialize(ctx, tx)
		if err0 != nil {
			return err0
		}
		// Check for blocks DB and tracker DB un-sync
		if au.dbRound > lastestBlockRound {
			au.log.Warnf("accountUpdates.initializeFromDisk: resetting accounts DB (on round %v, but blocks DB's latest is %v)", au.dbRound, lastestBlockRound)
			err0 = accountsReset(tx)
			if err0 != nil {
				return err0
			}
			au.dbRound, err0 = au.accountsInitialize(ctx, tx)
			if err0 != nil {
				return err0
			}
		}

		totals, err0 := accountsTotals(tx, false)
		if err0 != nil {
			return err0
		}

		au.roundTotals = []AccountTotals{totals}
		return nil
	})
	if err != nil {
		return
	}

	// the VacuumDatabase would be a no-op if au.vacuumOnStartup is cleared.
	au.vacuumDatabase(context.Background())
	if err != nil {
		return
	}

	au.accountsq, err = accountsDbInit(au.dbs.rdb.Handle, au.dbs.wdb.Handle)

	au.lastCatchpointLabel, _, err = au.accountsq.readCatchpointStateString(context.Background(), catchpointStateLastCatchpoint)
	if err != nil {
		return
	}

	hdr, err := l.BlockHdr(au.dbRound)
	if err != nil {
		return
	}
	au.protos = []config.ConsensusParams{config.Consensus[hdr.CurrentProtocol]}
	au.deltas = nil
	au.creatableDeltas = nil
	au.accounts = make(map[basics.Address]modifiedAccount)
	au.creatables = make(map[basics.CreatableIndex]modifiedCreatable)
	au.deltasAccum = []int{0}

	// keep these channel closed if we're not generating catchpoint
	au.catchpointWriting = make(chan struct{}, 1)
	au.catchpointSlowWriting = make(chan struct{}, 1)
	close(au.catchpointSlowWriting)
	close(au.catchpointWriting)
	au.ctx, au.ctxCancel = context.WithCancel(context.Background())
	au.committedOffset = make(chan deferedCommit, 1)
	au.commitSyncerClosed = make(chan struct{})
	go au.commitSyncer(au.committedOffset)

	lastBalancesRound = au.dbRound

	return
}

// accountHashBuilder calculates the hash key used for the trie by combining the account address and the account data
func accountHashBuilder(addr basics.Address, accountData basics.AccountData, encodedAccountData []byte) []byte {
	hash := make([]byte, 4+crypto.DigestSize)
	// write out the lowest 32 bits of the reward base. This should improve the caching of the trie by allowing
	// recent updated to be in-cache, and "older" nodes will be left alone.
	for i, rewards := 3, accountData.RewardsBase; i >= 0; i, rewards = i-1, rewards>>8 {
		// the following takes the rewards & 255 -> hash[i]
		hash[i] = byte(rewards)
	}
	entryHash := crypto.Hash(append(addr[:], encodedAccountData[:]...))
	copy(hash[4:], entryHash[:])
	return hash[:]
}

// accountsInitialize initializes the accounts DB if needed and return currrent account round.
// as part of the initialization, it tests the current database schema version, and perform upgrade
// procedures to bring it up to the database schema supported by the binary.
func (au *accountUpdates) accountsInitialize(ctx context.Context, tx *sql.Tx) (basics.Round, error) {
	// check current database version.
	dbVersion, err := db.GetUserVersion(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("accountsInitialize unable to read database schema version : %v", err)
	}

	// if database version is greater than supported by current binary, write a warning. This would keep the existing
	// fallback behaviour where we could use an older binary iff the schema happen to be backward compatible.
	if dbVersion > accountDBVersion {
		au.log.Warnf("accountsInitialize database schema version is %d, but algod supports only %d", dbVersion, accountDBVersion)
	}

	if dbVersion < accountDBVersion {
		au.log.Infof("accountsInitialize upgrading database schema from version %d to version %d", dbVersion, accountDBVersion)

		for dbVersion < accountDBVersion {
			au.log.Infof("accountsInitialize performing upgrade from version %d", dbVersion)
			// perform the initialization/upgrade
			switch dbVersion {
			case 0:
				dbVersion, err = au.upgradeDatabaseSchema0(ctx, tx)
				if err != nil {
					au.log.Warnf("accountsInitialize failed to upgrade accounts database (ledger.tracker.sqlite) from schema 0 : %v", err)
					return 0, err
				}
			case 1:
				dbVersion, err = au.upgradeDatabaseSchema1(ctx, tx)
				if err != nil {
					au.log.Warnf("accountsInitialize failed to upgrade accounts database (ledger.tracker.sqlite) from schema 1 : %v", err)
					return 0, err
				}
			default:
				return 0, fmt.Errorf("accountsInitialize unable to upgrade database from schema version %d", dbVersion)
			}
		}

		au.log.Infof("accountsInitialize database schema upgrade complete")
	}

	rnd, hashRound, err := accountsRound(tx)
	if err != nil {
		return 0, err
	}

	if hashRound != rnd {
		// if the hashed round is different then the base round, something was modified, and the accounts aren't in sync
		// with the hashes.
		err = resetAccountHashes(tx)
		if err != nil {
			return 0, err
		}
		// if catchpoint is disabled on this node, we could complete the initialization right here.
		if au.catchpointInterval == 0 {
			return rnd, nil
		}
	}

	// create the merkle trie for the balances
	committer, err := makeMerkleCommitter(tx, false)
	if err != nil {
		return 0, fmt.Errorf("accountsInitialize was unable to makeMerkleCommitter: %v", err)
	}
	trie, err := merkletrie.MakeTrie(committer, trieCachedNodesCount)
	if err != nil {
		return 0, fmt.Errorf("accountsInitialize was unable to MakeTrie: %v", err)
	}

	// we might have a database that was previously initialized, and now we're adding the balances trie. In that case, we need to add all the existing balances to this trie.
	// we can figure this out by examinine the hash of the root:
	rootHash, err := trie.RootHash()
	if err != nil {
		return rnd, fmt.Errorf("accountsInitialize was unable to retrieve trie root hash: %v", err)
	}

	if rootHash.IsZero() {
		au.log.Infof("accountsInitialize rebuilding merkle trie for round %d", rnd)
		var accountsIterator encodedAccountsBatchIter
		defer accountsIterator.Close()
		startTrieBuildTime := time.Now()
		accountsCount := 0
		lastRebuildTime := startTrieBuildTime
		pendingAccounts := 0
		for {
			bal, err := accountsIterator.Next(ctx, tx, trieRebuildAccountChunkSize)
			if err != nil {
				return rnd, err
			}
			if len(bal) == 0 {
				break
			}
			accountsCount += len(bal)
			pendingAccounts += len(bal)
			for _, balance := range bal {
				var accountData basics.AccountData
				err = protocol.Decode(balance.AccountData, &accountData)
				if err != nil {
					return rnd, err
				}
				hash := accountHashBuilder(balance.Address, accountData, balance.AccountData)
				added, err := trie.Add(hash)
				if err != nil {
					return rnd, fmt.Errorf("accountsInitialize was unable to add changes to trie: %v", err)
				}
				if !added {
					au.log.Warnf("accountsInitialize attempted to add duplicate hash '%s' to merkle trie for account %v", hex.EncodeToString(hash), balance.Address)
				}
			}

			if pendingAccounts >= trieRebuildCommitFrequency {
				// this trie Evict will commit using the current transaction.
				// if anything goes wrong, it will still get rolled back.
				_, err = trie.Evict(true)
				if err != nil {
					return 0, fmt.Errorf("accountsInitialize was unable to commit changes to trie: %v", err)
				}
				pendingAccounts = 0
			}

			if len(bal) < trieRebuildAccountChunkSize {
				break
			}

			if time.Now().Sub(lastRebuildTime) > 5*time.Second {
				// let the user know that the trie is still being rebuilt.
				au.log.Infof("accountsInitialize still building the trie, and processed so far %d accounts", accountsCount)
				lastRebuildTime = time.Now()
			}
		}

		// this trie Evict will commit using the current transaction.
		// if anything goes wrong, it will still get rolled back.
		_, err = trie.Evict(true)
		if err != nil {
			return 0, fmt.Errorf("accountsInitialize was unable to commit changes to trie: %v", err)
		}

		// we've just updated the markle trie, update the hashRound to reflect that.
		err = updateAccountsRound(tx, rnd, rnd)
		if err != nil {
			return 0, fmt.Errorf("accountsInitialize was unable to update the account round to %d: %v", rnd, err)
		}

		au.log.Infof("accountsInitialize rebuilt the merkle trie with %d entries in %v", accountsCount, time.Now().Sub(startTrieBuildTime))
	}
	au.balancesTrie = trie
	return rnd, nil
}

// upgradeDatabaseSchema0 upgrades the database schema from version 0 to version 1
//
// Schema of version 0 is expected to be aligned with the schema used on version 2.0.8 or before.
// Any database of version 2.0.8 would be of version 0. At this point, the database might
// have the following tables : ( i.e. a newly created database would not have these )
// * acctrounds
// * accounttotals
// * accountbase
// * assetcreators
// * storedcatchpoints
// * accounthashes
// * catchpointstate
//
// As the first step of the upgrade, the above tables are being created if they do not already exists.
// Following that, the assetcreators table is being altered by adding a new column to it (ctype).
// Last, in case the database was just created, it would get initialized with the following:
// The accountbase would get initialized with the au.initAccounts
// The accounttotals would get initialized to align with the initialization account added to accountbase
// The acctrounds would get updated to indicate that the balance matches round 0
//
func (au *accountUpdates) upgradeDatabaseSchema0(ctx context.Context, tx *sql.Tx) (updatedDBVersion int32, err error) {
	au.log.Infof("accountsInitialize initializing schema")
	err = accountsInit(tx, au.initAccounts, au.initProto)
	if err != nil {
		return 0, fmt.Errorf("accountsInitialize unable to initialize schema : %v", err)
	}
	_, err = db.SetUserVersion(ctx, tx, 1)
	if err != nil {
		return 0, fmt.Errorf("accountsInitialize unable to update database schema version from 0 to 1: %v", err)
	}
	return 1, nil
}

// upgradeDatabaseSchema1 upgrades the database schema from version 1 to version 2
//
// The schema updated to verison 2 intended to ensure that the encoding of all the accounts data is
// both canonical and identical across the entire network. On release 2.0.5 we released an upgrade to the messagepack.
// the upgraded messagepack was decoding the account data correctly, but would have different
// encoding compared to it's predecessor. As a result, some of the account data that was previously stored
// would have different encoded representation than the one on disk.
// To address this, this startup proceduce would attempt to scan all the accounts data. for each account data, we would
// see if it's encoding aligns with the current messagepack encoder. If it doesn't we would update it's encoding.
// then, depending if we found any such account data, we would reset the merkle trie and stored catchpoints.
// once the upgrade is complete, the accountsInitialize would (if needed) rebuild the merke trie using the new
// encoded accounts.
//
// This upgrade doesn't change any of the actual database schema ( i.e. tables, indexes ) but rather just performing
// a functional update to it's content.
//
func (au *accountUpdates) upgradeDatabaseSchema1(ctx context.Context, tx *sql.Tx) (updatedDBVersion int32, err error) {
	// update accounts encoding.
	au.log.Infof("accountsInitialize verifying accounts data encoding")
	modifiedAccounts, err := reencodeAccounts(ctx, tx)
	if err != nil {
		return 0, err
	}

	if modifiedAccounts > 0 {
		au.log.Infof("accountsInitialize reencoded %d accounts", modifiedAccounts)

		au.log.Infof("accountsInitialize resetting account hashes")
		// reset the merkle trie
		err = resetAccountHashes(tx)
		if err != nil {
			return 0, fmt.Errorf("accountsInitialize unable to reset account hashes : %v", err)
		}

		au.log.Infof("accountsInitialize preparing queries")
		// initialize a new accountsq with the incoming transaction.
		accountsq, err := accountsDbInit(tx, tx)
		if err != nil {
			return 0, fmt.Errorf("accountsInitialize unable to prepare queries : %v", err)
		}

		// close the prepared statements when we're done with them.
		defer accountsq.close()

		au.log.Infof("accountsInitialize resetting prior catchpoints")
		// delete the last catchpoint label if we have any.
		_, err = accountsq.writeCatchpointStateString(ctx, catchpointStateLastCatchpoint, "")
		if err != nil {
			return 0, fmt.Errorf("accountsInitialize unable to clear prior catchpoint : %v", err)
		}

		au.log.Infof("accountsInitialize deleting stored catchpoints")
		// delete catchpoints.
		err = au.deleteStoredCatchpoints(ctx, accountsq)
		if err != nil {
			return 0, fmt.Errorf("accountsInitialize unable to delete stored catchpoints : %v", err)
		}
	} else {
		au.log.Infof("accountsInitialize found that no accounts needed to be reencoded")
	}

	// update version
	_, err = db.SetUserVersion(ctx, tx, 2)
	if err != nil {
		return 0, fmt.Errorf("accountsInitialize unable to update database schema version from 1 to 2: %v", err)
	}
	return 2, nil
}

// deleteStoredCatchpoints iterates over the storedcatchpoints table and deletes all the files stored on disk.
// once all the files have been deleted, it would go ahead and remove the entries from the table.
func (au *accountUpdates) deleteStoredCatchpoints(ctx context.Context, dbQueries *accountsDbQueries) (err error) {
	catchpointsFilesChunkSize := 50
	for {
		fileNames, err := dbQueries.getOldestCatchpointFiles(ctx, catchpointsFilesChunkSize, 0)
		if err != nil {
			return err
		}
		if len(fileNames) == 0 {
			break
		}

		for round, fileName := range fileNames {
			absCatchpointFileName := filepath.Join(au.dbDirectory, fileName)
			err = os.Remove(absCatchpointFileName)
			if err == nil || os.IsNotExist(err) {
				// it's ok if the file doesn't exist. just remove it from the database and we'll be good to go.
				err = nil
			} else {
				// we can't delete the file, abort -
				return fmt.Errorf("unable to delete old catchpoint file '%s' : %v", absCatchpointFileName, err)
			}
			// clear the entry from the database
			err = dbQueries.storeCatchpoint(ctx, round, "", "", 0)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// accountsUpdateBalances applies the given deltas array to the merkle trie
func (au *accountUpdates) accountsUpdateBalances(accountsDeltasRound []map[basics.Address]accountDelta, offset uint64) (err error) {
	if au.catchpointInterval == 0 {
		return nil
	}
	var added, deleted bool
	accumulatedChanges := 0
	for i := uint64(0); i < offset; i++ {
		accountsDeltas := accountsDeltasRound[i]
		for addr, delta := range accountsDeltas {
			if !delta.old.IsZero() {
				deleteHash := accountHashBuilder(addr, delta.old, protocol.Encode(&delta.old))
				deleted, err = au.balancesTrie.Delete(deleteHash)
				if err != nil {
					return err
				}
				if !deleted {
					au.log.Warnf("failed to delete hash '%s' from merkle trie for account %v", hex.EncodeToString(deleteHash), addr)
				} else {
					accumulatedChanges++
				}
			}
			if !delta.new.IsZero() {
				addHash := accountHashBuilder(addr, delta.new, protocol.Encode(&delta.new))
				added, err = au.balancesTrie.Add(addHash)
				if err != nil {
					return err
				}
				if !added {
					au.log.Warnf("attempted to add duplicate hash '%s' to merkle trie for account %v", hex.EncodeToString(addHash), addr)
				} else {
					accumulatedChanges++
				}
			}
		}
		if accumulatedChanges >= trieAccumulatedChangesFlush {
			accumulatedChanges = 0
			err = au.balancesTrie.Commit()
			if err != nil {
				return
			}
		}
	}
	// write it all to disk.
	if accumulatedChanges > 0 {
		err = au.balancesTrie.Commit()
	}
	return
}

// newBlockImpl is the accountUpdates implementation of the ledgerTracker interface. This is the "internal" facing function
// which assumes that no lock need to be taken.
func (au *accountUpdates) newBlockImpl(blk bookkeeping.Block, delta StateDelta) {
	proto := config.Consensus[blk.CurrentProtocol]
	rnd := blk.Round()

	if rnd <= au.latest() {
		// Duplicate, ignore.
		return
	}

	if rnd != au.latest()+1 {
		au.log.Panicf("accountUpdates: newBlock %d too far in the future, dbRound %d, deltas %d", rnd, au.dbRound, len(au.deltas))
	}
	au.deltas = append(au.deltas, delta.accts)
	au.protos = append(au.protos, proto)
	au.creatableDeltas = append(au.creatableDeltas, delta.creatables)
	au.roundDigest = append(au.roundDigest, blk.Digest())
	au.deltasAccum = append(au.deltasAccum, len(delta.accts)+au.deltasAccum[len(au.deltasAccum)-1])

	var ot basics.OverflowTracker
	newTotals := au.roundTotals[len(au.roundTotals)-1]
	allBefore := newTotals.All()
	newTotals.applyRewards(delta.hdr.RewardsLevel, &ot)

	for addr, data := range delta.accts {
		newTotals.delAccount(proto, data.old, &ot)
		newTotals.addAccount(proto, data.new, &ot)

		macct := au.accounts[addr]
		macct.ndeltas++
		macct.data = data.new
		au.accounts[addr] = macct
	}

	for cidx, cdelta := range delta.creatables {
		mcreat := au.creatables[cidx]
		mcreat.creator = cdelta.creator
		mcreat.created = cdelta.created
		mcreat.ctype = cdelta.ctype
		mcreat.ndeltas++
		au.creatables[cidx] = mcreat
	}

	if ot.Overflowed {
		au.log.Panicf("accountUpdates: newBlock %d overflowed totals", rnd)
	}
	allAfter := newTotals.All()
	if allBefore != allAfter {
		au.log.Panicf("accountUpdates: sum of money changed from %d to %d", allBefore.Raw, allAfter.Raw)
	}

	au.roundTotals = append(au.roundTotals, newTotals)
}

// lookupImpl returns the accound data for a given address at a given round. The withRewards indicates whether the
// rewards should be added to the AccountData before returning. Note that the function doesn't update the account with the rewards,
// even while it could return the AccoutData which represent the "rewarded" account data.
func (au *accountUpdates) lookupImpl(rnd basics.Round, addr basics.Address, withRewards bool) (data basics.AccountData, err error) {
	offset, err := au.roundOffset(rnd)
	if err != nil {
		return
	}

	offsetForRewards := offset

	defer func() {
		if withRewards {
			totals := au.roundTotals[offsetForRewards]
			proto := au.protos[offsetForRewards]
			data = data.WithUpdatedRewards(proto, totals.RewardsLevel)
		}
	}()

	// Check if this is the most recent round, in which case, we can
	// use a cache of the most recent account state.
	if offset == uint64(len(au.deltas)) {
		macct, ok := au.accounts[addr]
		if ok {
			return macct.data, nil
		}
	} else {
		// Check if the account has been updated recently.  Traverse the deltas
		// backwards to ensure that later updates take priority if present.
		for offset > 0 {
			offset--
			d, ok := au.deltas[offset][addr]
			if ok {
				return d.new, nil
			}
		}
	}

	// No updates of this account in the in-memory deltas; use on-disk DB.
	// The check in roundOffset() made sure the round is exactly the one
	// present in the on-disk DB.  As an optimization, we avoid creating
	// a separate transaction here, and directly use a prepared SQL query
	// against the database.
	return au.accountsq.lookup(addr)
}

// getCreatorForRoundImpl returns the asset/app creator for a given asset/app index at a given round
func (au *accountUpdates) getCreatorForRoundImpl(rnd basics.Round, cidx basics.CreatableIndex, ctype basics.CreatableType) (creator basics.Address, ok bool, err error) {
	offset, err := au.roundOffset(rnd)
	if err != nil {
		return basics.Address{}, false, err
	}

	// If this is the most recent round, au.creatables has will have the latest
	// state and we can skip scanning backwards over creatableDeltas
	if offset == uint64(len(au.deltas)) {
		// Check if we already have the asset/creator in cache
		creatableDelta, ok := au.creatables[cidx]
		if ok {
			if creatableDelta.created && creatableDelta.ctype == ctype {
				return creatableDelta.creator, true, nil
			}
			return basics.Address{}, false, nil
		}
	} else {
		for offset > 0 {
			offset--
			creatableDelta, ok := au.creatableDeltas[offset][cidx]
			if ok {
				if creatableDelta.created && creatableDelta.ctype == ctype {
					return creatableDelta.creator, true, nil
				}
				return basics.Address{}, false, nil
			}
		}
	}

	// Check the database
	return au.accountsq.lookupCreator(cidx, ctype)
}

// accountsCreateCatchpointLabel creates a catchpoint label and write it.
func (au *accountUpdates) accountsCreateCatchpointLabel(committedRound basics.Round, totals AccountTotals, ledgerBlockDigest crypto.Digest, trieBalancesHash crypto.Digest) (label string, err error) {
	cpLabel := makeCatchpointLabel(committedRound, ledgerBlockDigest, trieBalancesHash, totals)
	label = cpLabel.String()
	_, err = au.accountsq.writeCatchpointStateString(context.Background(), catchpointStateLastCatchpoint, label)
	return
}

// roundOffset calculates the offset of the given round compared to the current dbRound. Requires that the lock would be taken.
func (au *accountUpdates) roundOffset(rnd basics.Round) (offset uint64, err error) {
	if rnd < au.dbRound {
		err = fmt.Errorf("round %d before dbRound %d", rnd, au.dbRound)
		return
	}

	off := uint64(rnd - au.dbRound)
	if off > uint64(len(au.deltas)) {
		err = fmt.Errorf("round %d too high: dbRound %d, deltas %d", rnd, au.dbRound, len(au.deltas))
		return
	}

	return off, nil
}

// commitSyncer is the syncer go-routine function which perform the database updates. Internally, it dequeue deferedCommits and
// send the tasks to commitRound for completing the operation.
func (au *accountUpdates) commitSyncer(deferedCommits chan deferedCommit) {
	defer close(au.commitSyncerClosed)
	for {
		select {
		case committedOffset, ok := <-deferedCommits:
			if !ok {
				return
			}
			au.commitRound(committedOffset.offset, committedOffset.dbRound, committedOffset.lookback)
		case <-au.ctx.Done():
			// drain the pending commits queue:
			drained := false
			for !drained {
				select {
				case <-deferedCommits:
					au.accountsWriting.Done()
				default:
					drained = true
				}
			}
			return
		}
	}
}

// commitRound write to the database a "chunk" of rounds, and update the dbRound accordingly.
func (au *accountUpdates) commitRound(offset uint64, dbRound basics.Round, lookback basics.Round) {
	defer au.accountsWriting.Done()
	au.accountsMu.RLock()

	// we can exit right away, as this is the result of mis-ordered call to committedUpTo.
	if au.dbRound < dbRound || offset < uint64(au.dbRound-dbRound) {
		// if this is an archival ledger, we might need to close the catchpointWriting channel
		if au.archivalLedger {
			// determine if this was a catchpoint round
			isCatchpointRound := ((offset + uint64(lookback+dbRound)) > 0) && (au.catchpointInterval != 0) && (0 == (uint64((offset + uint64(lookback+dbRound))) % au.catchpointInterval))
			if isCatchpointRound {
				// it was a catchpoint round, so close the channel.
				close(au.catchpointWriting)
			}
		}
		au.accountsMu.RUnlock()
		return
	}

	// adjust the offset according to what happend meanwhile..
	offset -= uint64(au.dbRound - dbRound)
	dbRound = au.dbRound

	newBase := basics.Round(offset) + dbRound
	flushTime := time.Now()
	isCatchpointRound := ((offset + uint64(lookback+dbRound)) > 0) && (au.catchpointInterval != 0) && (0 == (uint64((offset + uint64(lookback+dbRound))) % au.catchpointInterval))

	// create a copy of the deltas, round totals and protos for the range we're going to flush.
	deltas := make([]map[basics.Address]accountDelta, offset, offset)
	creatableDeltas := make([]map[basics.CreatableIndex]modifiedCreatable, offset, offset)
	roundTotals := make([]AccountTotals, offset+1, offset+1)
	protos := make([]config.ConsensusParams, offset+1, offset+1)
	copy(deltas, au.deltas[:offset])
	copy(creatableDeltas, au.creatableDeltas[:offset])
	copy(roundTotals, au.roundTotals[:offset+1])
	copy(protos, au.protos[:offset+1])

	// Keep track of how many changes to each account we flush to the
	// account DB, so that we can drop the corresponding refcounts in
	// au.accounts.
	flushcount := make(map[basics.Address]int)
	creatableFlushcount := make(map[basics.CreatableIndex]int)

	var committedRoundDigest crypto.Digest

	if isCatchpointRound {
		committedRoundDigest = au.roundDigest[offset+uint64(lookback)-1]
	}

	au.accountsMu.RUnlock()

	// in committedUpTo, we expect that this function we close the catchpointWriting when
	// it's on a catchpoint round and it's an archival ledger. Doing this in a defered function
	// here would prevent us from "forgetting" to close that channel later on.
	defer func() {
		if isCatchpointRound && au.archivalLedger {
			close(au.catchpointWriting)
		}
	}()

	for i := uint64(0); i < offset; i++ {
		for addr := range deltas[i] {
			flushcount[addr] = flushcount[addr] + 1
		}
		for cidx := range creatableDeltas[i] {
			creatableFlushcount[cidx] = creatableFlushcount[cidx] + 1
		}
	}

	var catchpointLabel string
	beforeUpdatingBalancesTime := time.Now()
	var trieBalancesHash crypto.Digest

	err := au.dbs.wdb.AtomicCommitWriteLock(func(ctx context.Context, tx *sql.Tx) (err error) {
		treeTargetRound := basics.Round(0)
		if au.catchpointInterval > 0 {
			mc, err0 := makeMerkleCommitter(tx, false)
			if err0 != nil {
				return err0
			}
			if au.balancesTrie == nil {
				trie, err := merkletrie.MakeTrie(mc, trieCachedNodesCount)
				if err != nil {
					au.log.Warnf("unable to create merkle trie during committedUpTo: %v", err)
					return err
				}
				au.balancesTrie = trie
			} else {
				au.balancesTrie.SetCommitter(mc)
			}
			treeTargetRound = dbRound + basics.Round(offset)
		}
		for i := uint64(0); i < offset; i++ {
			err = accountsNewRound(tx, deltas[i], creatableDeltas[i])
			if err != nil {
				return err
			}
		}
		err = totalsNewRounds(tx, deltas[:offset], roundTotals[1:offset+1], protos[1:offset+1])
		if err != nil {
			return err
		}

		err = au.accountsUpdateBalances(deltas, offset)
		if err != nil {
			return err
		}

		err = updateAccountsRound(tx, dbRound+basics.Round(offset), treeTargetRound)
		if err != nil {
			return err
		}

		if isCatchpointRound {
			trieBalancesHash, err = au.balancesTrie.RootHash()
			if err != nil {
				return
			}
		}
		return nil
	}, &au.accountsMu)

	if err != nil {
		au.balancesTrie = nil
		au.log.Warnf("unable to advance account snapshot: %v", err)
		return
	}

	if isCatchpointRound {
		catchpointLabel, err = au.accountsCreateCatchpointLabel(dbRound+basics.Round(offset)+lookback, roundTotals[offset], committedRoundDigest, trieBalancesHash)
		if err != nil {
			au.log.Warnf("commitRound : unable to create a catchpoint label: %v", err)
		}
	}
	if au.balancesTrie != nil {
		_, err = au.balancesTrie.Evict(false)
		if err != nil {
			au.log.Warnf("merkle trie failed to evict: %v", err)
		}
	}

	if isCatchpointRound && catchpointLabel != "" {
		au.lastCatchpointLabel = catchpointLabel
	}
	updatingBalancesDuration := time.Now().Sub(beforeUpdatingBalancesTime)

	// Drop reference counts to modified accounts, and evict them
	// from in-memory cache when no references remain.
	for addr, cnt := range flushcount {
		macct, ok := au.accounts[addr]
		if !ok {
			au.log.Panicf("inconsistency: flushed %d changes to %s, but not in au.accounts", cnt, addr)
		}

		if cnt > macct.ndeltas {
			au.log.Panicf("inconsistency: flushed %d changes to %s, but au.accounts had %d", cnt, addr, macct.ndeltas)
		}

		macct.ndeltas -= cnt
		if macct.ndeltas == 0 {
			delete(au.accounts, addr)
		} else {
			au.accounts[addr] = macct
		}
	}

	for cidx, cnt := range creatableFlushcount {
		mcreat, ok := au.creatables[cidx]
		if !ok {
			au.log.Panicf("inconsistency: flushed %d changes to creatable %d, but not in au.creatables", cnt, cidx)
		}

		if cnt > mcreat.ndeltas {
			au.log.Panicf("inconsistency: flushed %d changes to creatable %d, but au.creatables had %d", cnt, cidx, mcreat.ndeltas)
		}

		mcreat.ndeltas -= cnt
		if mcreat.ndeltas == 0 {
			delete(au.creatables, cidx)
		} else {
			au.creatables[cidx] = mcreat
		}
	}

	au.deltas = au.deltas[offset:]
	au.deltasAccum = au.deltasAccum[offset:]
	au.roundDigest = au.roundDigest[offset:]
	au.protos = au.protos[offset:]
	au.roundTotals = au.roundTotals[offset:]
	au.creatableDeltas = au.creatableDeltas[offset:]
	au.dbRound = newBase
	au.lastFlushTime = flushTime

	au.accountsMu.Unlock()

	if isCatchpointRound && au.archivalLedger && catchpointLabel != "" {
		// generate the catchpoint file. This need to be done inline so that it will block any new accounts that from being written.
		// the generateCatchpoint expects that the accounts data would not be modified in the background during it's execution.
		au.generateCatchpoint(basics.Round(offset)+dbRound+lookback, catchpointLabel, committedRoundDigest, updatingBalancesDuration)
	}

}

// latest returns the latest round
func (au *accountUpdates) latest() basics.Round {
	return au.dbRound + basics.Round(len(au.deltas))
}

// generateCatchpoint generates a single catchpoint file
func (au *accountUpdates) generateCatchpoint(committedRound basics.Round, label string, committedRoundDigest crypto.Digest, updatingBalancesDuration time.Duration) {
	beforeGeneratingCatchpointTime := time.Now()
	catchpointGenerationStats := telemetryspec.CatchpointGenerationEventDetails{
		BalancesWriteTime: uint64(updatingBalancesDuration.Nanoseconds()),
	}

	// the retryCatchpointCreation is used to repeat the catchpoint file generation in case the node crashed / aborted during startup
	// before the catchpoint file generation could be completed.
	retryCatchpointCreation := false
	au.log.Debugf("accountUpdates: generateCatchpoint: generating catchpoint for round %d", committedRound)
	defer func() {
		if !retryCatchpointCreation {
			// clear the writingCatchpoint flag
			_, err := au.accountsq.writeCatchpointStateUint64(context.Background(), catchpointStateWritingCatchpoint, uint64(0))
			if err != nil {
				au.log.Warnf("accountUpdates: generateCatchpoint unable to clear catchpoint state '%s' for round %d: %v", catchpointStateWritingCatchpoint, committedRound, err)
			}
		}
	}()

	_, err := au.accountsq.writeCatchpointStateUint64(context.Background(), catchpointStateWritingCatchpoint, uint64(committedRound))
	if err != nil {
		au.log.Warnf("accountUpdates: generateCatchpoint unable to write catchpoint state '%s' for round %d: %v", catchpointStateWritingCatchpoint, committedRound, err)
		return
	}

	relCatchpointFileName := filepath.Join("catchpoints", catchpointRoundToPath(committedRound))
	absCatchpointFileName := filepath.Join(au.dbDirectory, relCatchpointFileName)

	more := true
	const shortChunkExecutionDuration = 50 * time.Millisecond
	const longChunkExecutionDuration = 1 * time.Second
	var chunkExecutionDuration time.Duration
	select {
	case <-au.catchpointSlowWriting:
		chunkExecutionDuration = longChunkExecutionDuration
	default:
		chunkExecutionDuration = shortChunkExecutionDuration
	}

	var catchpointWriter *catchpointWriter
	err = au.dbs.rdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		catchpointWriter = makeCatchpointWriter(au.ctx, absCatchpointFileName, tx, committedRound, committedRoundDigest, label)
		for more {
			stepCtx, stepCancelFunction := context.WithTimeout(au.ctx, chunkExecutionDuration)
			writeStepStartTime := time.Now()
			more, err = catchpointWriter.WriteStep(stepCtx)
			// accumulate the actual time we've spent writing in this step.
			catchpointGenerationStats.CPUTime += uint64(time.Now().Sub(writeStepStartTime).Nanoseconds())
			stepCancelFunction()
			if more && err == nil {
				// we just wrote some data, but there is more to be written.
				// go to sleep for while.
				// before going to sleep, extend the transaction timeout so that we won't get warnings:
				db.ResetTransactionWarnDeadline(ctx, tx, time.Now().Add(1*time.Second))
				select {
				case <-time.After(100 * time.Millisecond):
				case <-au.ctx.Done():
					retryCatchpointCreation = true
					err2 := catchpointWriter.Abort()
					if err2 != nil {
						return fmt.Errorf("error removing catchpoint file : %v", err2)
					}
					return nil
				case <-au.catchpointSlowWriting:
					chunkExecutionDuration = longChunkExecutionDuration
				}
			}
			if err != nil {
				err = fmt.Errorf("unable to create catchpoint : %v", err)
				err2 := catchpointWriter.Abort()
				if err2 != nil {
					au.log.Warnf("accountUpdates: generateCatchpoint: error removing catchpoint file : %v", err2)
				}
				return
			}
		}
		return
	})

	if err != nil {
		au.log.Warnf("accountUpdates: generateCatchpoint: %v", err)
		return
	}
	if catchpointWriter == nil {
		au.log.Warnf("accountUpdates: generateCatchpoint: nil catchpointWriter")
		return
	}

	err = au.saveCatchpointFile(committedRound, relCatchpointFileName, catchpointWriter.GetSize(), catchpointWriter.GetCatchpoint())
	if err != nil {
		au.log.Warnf("accountUpdates: generateCatchpoint: unable to save catchpoint: %v", err)
		return
	}
	catchpointGenerationStats.FileSize = uint64(catchpointWriter.GetSize())
	catchpointGenerationStats.WritingDuration = uint64(time.Now().Sub(beforeGeneratingCatchpointTime).Nanoseconds())
	catchpointGenerationStats.AccountsCount = catchpointWriter.GetTotalAccounts()
	catchpointGenerationStats.CatchpointLabel = catchpointWriter.GetCatchpoint()
	au.log.EventWithDetails(telemetryspec.Accounts, telemetryspec.CatchpointGenerationEvent, catchpointGenerationStats)
	au.log.With("writingDuration", catchpointGenerationStats.WritingDuration).
		With("CPUTime", catchpointGenerationStats.CPUTime).
		With("balancesWriteTime", catchpointGenerationStats.BalancesWriteTime).
		With("accountsCount", catchpointGenerationStats.AccountsCount).
		With("fileSize", catchpointGenerationStats.FileSize).
		With("catchpointLabel", catchpointGenerationStats.CatchpointLabel).
		Infof("Catchpoint file was generated")
}

// catchpointRoundToPath calculate the catchpoint file path for a given round
func catchpointRoundToPath(rnd basics.Round) string {
	irnd := int64(rnd) / 256
	outStr := ""
	for irnd > 0 {
		outStr = filepath.Join(outStr, fmt.Sprintf("%02x", irnd%256))
		irnd = irnd / 256
	}
	outStr = filepath.Join(outStr, strconv.FormatInt(int64(rnd), 10)+".catchpoint")
	return outStr
}

// saveCatchpointFile stores the provided fileName as the stored catchpoint for the given round.
// after a successfull insert operation to the database, it would delete up to 2 old entries, as needed.
// deleting 2 entries while inserting single entry allow us to adjust the size of the backing storage and have the
// database and storage realign.
func (au *accountUpdates) saveCatchpointFile(round basics.Round, fileName string, fileSize int64, catchpoint string) (err error) {
	if au.catchpointFileHistoryLength != 0 {
		err = au.accountsq.storeCatchpoint(context.Background(), round, fileName, catchpoint, fileSize)
		if err != nil {
			au.log.Warnf("accountUpdates: saveCatchpoint: unable to save catchpoint: %v", err)
			return
		}
	} else {
		err = os.Remove(fileName)
		if err != nil {
			au.log.Warnf("accountUpdates: saveCatchpoint: unable to remove file (%s): %v", fileName, err)
			return
		}
	}
	if au.catchpointFileHistoryLength == -1 {
		return
	}
	var filesToDelete map[basics.Round]string
	filesToDelete, err = au.accountsq.getOldestCatchpointFiles(context.Background(), 2, au.catchpointFileHistoryLength)
	if err != nil {
		return fmt.Errorf("unable to delete catchpoint file, getOldestCatchpointFiles failed : %v", err)
	}
	for round, fileToDelete := range filesToDelete {
		absCatchpointFileName := filepath.Join(au.dbDirectory, fileToDelete)
		err = os.Remove(absCatchpointFileName)
		if err == nil || os.IsNotExist(err) {
			// it's ok if the file doesn't exist. just remove it from the database and we'll be good to go.
			err = nil
		} else {
			// we can't delete the file, abort -
			return fmt.Errorf("unable to delete old catchpoint file '%s' : %v", absCatchpointFileName, err)
		}
		err = au.accountsq.storeCatchpoint(context.Background(), round, "", "", 0)
		if err != nil {
			return fmt.Errorf("unable to delete old catchpoint entry '%s' : %v", fileToDelete, err)
		}
	}
	return
}

// the vacuumDatabase performs a full vacuum of the accounts database.
func (au *accountUpdates) vacuumDatabase(ctx context.Context) (err error) {
	if !au.vacuumOnStartup {
		return
	}

	startTime := time.Now()
	vacuumExitCh := make(chan struct{}, 1)
	vacuumLoggingAbort := sync.WaitGroup{}
	vacuumLoggingAbort.Add(1)
	// vacuuming the database can take a while. A long while. We want to have a logging function running in a separate go-routine that would log the progress to the log file.
	// also, when we're done vacuuming, we should sent an event notifying of the total time it took to vacuum the database.
	go func() {
		defer vacuumLoggingAbort.Done()
		au.log.Infof("Vacuuming accounts database started")
		for {
			select {
			case <-time.After(5 * time.Second):
				au.log.Infof("Vacuuming accounts database in progress")
			case <-vacuumExitCh:
				return
			}
		}
	}()

	vacuumStats, err := au.dbs.wdb.Vacuum(ctx)
	close(vacuumExitCh)
	vacuumLoggingAbort.Wait()

	if err != nil {
		au.log.Warnf("Vacuuming account database failed : %v", err)
		return err
	}
	vacuumElapsedTime := time.Now().Sub(startTime)

	au.log.Infof("Vacuuming accounts database completed within %v, reducing number of pages from %d to %d and size from %d to %d", vacuumElapsedTime, vacuumStats.PagesBefore, vacuumStats.PagesAfter, vacuumStats.SizeBefore, vacuumStats.SizeAfter)

	vacuumTelemetryStats := telemetryspec.BalancesAccountVacuumEventDetails{
		VacuumTimeNanoseconds:  vacuumElapsedTime.Nanoseconds(),
		BeforeVacuumPageCount:  vacuumStats.PagesBefore,
		AfterVacuumPageCount:   vacuumStats.PagesAfter,
		BeforeVacuumSpaceBytes: vacuumStats.SizeBefore,
		AfterVacuumSpaceBytes:  vacuumStats.SizeAfter,
	}

	au.log.EventWithDetails(telemetryspec.Accounts, telemetryspec.BalancesAccountVacuumEvent, vacuumTelemetryStats)
	return
}
