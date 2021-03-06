/**
* @file
* @copyright defined in go-seele/LICENSE
 */

package state

import (
	"math/big"

	"github.com/hashicorp/golang-lru"
	"github.com/seeleteam/go-seele/common"
	"github.com/seeleteam/go-seele/core/types"
	"github.com/seeleteam/go-seele/database"
	"github.com/seeleteam/go-seele/trie"
)

// StateCacheCapacity is the capacity of state cache
const StateCacheCapacity = 1000

var (
	stateBalance0 = big.NewInt(0)
)

// Statedb is used to store accounts into the MPT tree
type Statedb struct {
	db           database.Database
	trie         *trie.Trie
	stateObjects *lru.Cache // stateObjects maps account addresses of common.Address type to the state objects of *StateObject type

	dbErr  error  // dbErr is used for record the database error.
	refund uint64 // The refund counter, also used by state transitioning.

	// Receipt logs for current processed tx.
	curTxIndex uint
	curLogs    []*types.Log

	// State modifications for current processed tx.
	curJournal journal
}

// NewStatedb constructs and returns a statedb instance
func NewStatedb(root common.Hash, db database.Database) (*Statedb, error) {
	trie, err := trie.NewTrie(root, []byte("S"), db)
	if err != nil {
		return nil, err
	}

	stateCache, err := lru.New(StateCacheCapacity)
	if err != nil {
		return nil, err
	}

	return &Statedb{
		db:           db,
		trie:         trie,
		stateObjects: stateCache,
		curJournal:   journal{},
	}, nil
}

// GetCopy is a memory copy of state db.
func (s *Statedb) GetCopy() (*Statedb, error) {
	copies, err := lru.New(StateCacheCapacity)
	if err != nil {
		panic(err) // call panic, in case of the error which happens only when StateCacheCapacity is negative.
	}

	for _, k := range s.stateObjects.Keys() {
		v, ok := s.stateObjects.Peek(k)
		if ok {
			copies.Add(k, v)
		}
	}

	cpyTrie, err := s.trie.ShallowCopy()
	if err != nil {
		return nil, err
	}

	return &Statedb{
		db:           s.db,
		trie:         cpyTrie,
		stateObjects: copies,

		dbErr:  s.dbErr,
		refund: s.refund,
	}, nil
}

// setError only records the first error.
func (s *Statedb) setError(err error) {
	if s.dbErr == nil {
		s.dbErr = err
	}
}

// GetBalance returns the balance of the specified account if exists.
// Otherwise, returns zero.
func (s *Statedb) GetBalance(addr common.Address) *big.Int {
	object := s.getStateObject(addr)
	if object != nil {
		return object.GetAmount()
	}
	return stateBalance0
}

// SetBalance sets the balance of the specified account
func (s *Statedb) SetBalance(addr common.Address, balance *big.Int) {
	object := s.getStateObject(addr)
	if object != nil {
		s.curJournal.append(balanceChange{&addr, object.GetAmount()})
		object.SetAmount(balance)
	}
}

// AddBalance adds the specified amount to the balance for the specified account
func (s *Statedb) AddBalance(addr common.Address, amount *big.Int) {
	object := s.getStateObject(addr)
	if object != nil {
		s.curJournal.append(balanceChange{&addr, object.GetAmount()})
		object.AddAmount(amount)
	}
}

// SubBalance substracts the specified amount from the balance for the specified account
func (s *Statedb) SubBalance(addr common.Address, amount *big.Int) {
	object := s.getStateObject(addr)
	if object != nil {
		s.curJournal.append(balanceChange{&addr, object.GetAmount()})
		object.SubAmount(amount)
	}
}

// GetNonce gets the nonce of the specified account
func (s *Statedb) GetNonce(addr common.Address) uint64 {
	object := s.getStateObject(addr)
	if object != nil {
		return object.GetNonce()
	}
	return 0
}

// SetNonce sets the nonce of the specified account
func (s *Statedb) SetNonce(addr common.Address, nonce uint64) {
	object := s.getStateObject(addr)
	if object != nil {
		s.curJournal.append(nonceChange{&addr, object.GetNonce()})
		object.SetNonce(nonce)
	}
}

// Commit commits memory state objects to db
func (s *Statedb) Commit(batch database.Batch) (common.Hash, error) {
	if s.dbErr != nil {
		return common.EmptyHash, s.dbErr
	}

	for _, key := range s.stateObjects.Keys() {
		value, ok := s.stateObjects.Peek(key)
		if ok {
			addr := key.(common.Address)
			object := value.(*StateObject)
			if err := s.commitOne(addr, object, batch); err != nil {
				return common.EmptyHash, err
			}
		}
	}

	return s.trie.Commit(batch), nil
}

func (s *Statedb) commitOne(addr common.Address, obj *StateObject, batch database.Batch) error {
	// Commit storage change.
	if err := obj.commitStorageTrie(s.db, batch); err != nil {
		return err
	}

	// Commit code change.
	if obj.dirtyCode && batch != nil {
		obj.serializeCode(batch)
		obj.dirtyCode = false
	}

	// Commit account info change.
	if obj.dirtyAccount {
		data := common.SerializePanic(obj.account)
		s.trie.Put(addr[:], data)
		obj.dirtyAccount = false
	}

	// Remove the account from state DB if suicided.
	if obj.suicided && !obj.deleted {
		obj.deleted = true
		s.trie.Delete(addr.Bytes())
	}

	return nil
}

func (s *Statedb) cache(addr common.Address, obj *StateObject) {
	if s.stateObjects.Len() == StateCacheCapacity {
		s.Commit(nil)

		// clear a quarter of the cached state infos to avoid frequent commits
		for i := 0; i < StateCacheCapacity/4; i++ {
			s.stateObjects.RemoveOldest()
		}
	}

	s.stateObjects.Add(addr, obj)
}

// GetOrNewStateObject gets or creates a state object
func (s *Statedb) GetOrNewStateObject(addr common.Address) *StateObject {
	object := s.getStateObject(addr)
	if object == nil {
		object = newStateObject(addr)
		object.SetNonce(0)
		s.cache(addr, object)
	}

	return object
}

func (s *Statedb) getStateObject(addr common.Address) *StateObject {
	if value, ok := s.stateObjects.Get(addr); ok {
		if object := value.(*StateObject); !object.deleted {
			return object
		}

		// object has already been deleted from trie.
		return nil
	}

	object := newStateObject(addr)
	val, _ := s.trie.Get(addr[:])
	if len(val) == 0 {
		return nil
	}

	if err := common.Deserialize(val, &object.account); err != nil {
		return nil
	}

	s.cache(addr, object)
	return object
}

// Prepare resets the logs and journal to process a new tx.
func (s *Statedb) Prepare(txIndex int) {
	s.curTxIndex = uint(txIndex)
	s.curLogs = nil

	s.curJournal.entries = s.curJournal.entries[:0]
}

// GetCurrentLogs returns the current transaction logs.
func (s *Statedb) GetCurrentLogs() []*types.Log {
	return s.curLogs
}
