// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package state

import (
	"bytes"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/pathdb"
)

var (
	flattenTestAddr = common.HexToAddress("0x1000000000000000000000000000000000000001")
	flattenTestSlot = common.HexToHash("0x01")
)

// flattenChain is a path-scheme state database with full state history and a
// small diff layer limit, so that committing one block flattens the bottom layer.
type flattenChain struct {
	t             *testing.T
	tdb           *triedb.Database
	db            *CachingDB
	maxDiffLayers int
	roots         []common.Hash
	vals          []uint64 // nonce and slot value written by each block
	gen           uint64   // number of rewinds so far
	disk          int      // block whose state is the disk layer, -1 if none
}

func newFlattenChain(t *testing.T, maxDiffLayers uint64, blocks int) *flattenChain {
	t.Helper()
	disk, err := rawdb.Open(rawdb.NewMemoryDatabase(), rawdb.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tdb := triedb.NewDatabase(disk, &triedb.Config{PathDB: &pathdb.Config{
		StateHistory:        0,
		EnableStateIndexing: true,
		TrieCleanSize:       256 * 1024,
		StateCleanSize:      256 * 1024,
		WriteBufferSize:     256 * 1024,
		NoAsyncFlush:        true,
		MaxDiffLayers:       maxDiffLayers,
	}})
	c := &flattenChain{t: t, tdb: tdb, db: NewDatabase(tdb, nil), maxDiffLayers: int(maxDiffLayers), disk: -1}
	t.Cleanup(func() { tdb.Close() })
	for i := 0; i < blocks; i++ {
		c.commit()
	}
	return c
}

// commit adds one block that sets the test account's nonce and slot to the
// block's position in the chain plus one, offset by 100 for every rewind so far.
// A block that reuses a rewound block's position (and state ID) therefore holds
// different values.
func (c *flattenChain) commit() {
	c.t.Helper()
	parent := types.EmptyRootHash
	if len(c.roots) > 0 {
		parent = c.roots[len(c.roots)-1]
	}
	st, err := New(parent, c.db)
	if err != nil {
		c.t.Fatal(err)
	}
	// Filler accounts make the account trie deep enough that reads resolve nodes lazily.
	if len(c.roots) == 0 {
		for i := 0; i < 256; i++ {
			st.SetNonce(common.BigToAddress(big.NewInt(int64(i+1))), 1, tracing.NonceChangeUnspecified)
		}
	}
	n := uint64(len(c.roots)) + 1 + 100*c.gen
	st.SetNonce(flattenTestAddr, n, tracing.NonceChangeUnspecified)
	st.SetState(flattenTestAddr, flattenTestSlot, common.BigToHash(new(big.Int).SetUint64(n)))
	root, err := st.Commit(uint64(len(c.roots)), true, false)
	if err != nil {
		c.t.Fatal(err)
	}
	c.roots = append(c.roots, root)
	c.vals = append(c.vals, n)
	c.disk = max(c.disk, len(c.roots)-1-c.maxDiffLayers)
	c.waitIndexed()
}

// rewind recovers the state to two blocks below the disk layer (or the first
// block if the chain is shorter), the way a deep reorg or a head reset does,
// and drops the newer blocks. It reports false if there is no such block.
func (c *flattenChain) rewind() bool {
	c.t.Helper()
	target := max(c.disk-2, 0)
	if target >= c.disk {
		return false
	}
	if err := c.tdb.Recover(c.roots[target]); err != nil {
		c.t.Fatalf("recover to state %d: %v", target, err)
	}
	c.roots = c.roots[:target+1]
	c.vals = c.vals[:target+1]
	c.disk = target
	c.gen++
	c.waitIndexed()
	return true
}

// reorg rewinds and then commits a different branch back to the old length,
// so the new blocks reuse the rewound blocks' state IDs.
func (c *flattenChain) reorg() bool {
	c.t.Helper()
	n := len(c.roots)
	if !c.rewind() {
		return false
	}
	for len(c.roots) < n {
		c.commit()
	}
	return true
}

func (c *flattenChain) waitIndexed() {
	c.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		left, err := c.tdb.IndexProgress()
		if err == nil && left == 0 {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("state history not indexed: left %d, err %v", left, err)
		}
		time.Sleep(time.Millisecond)
	}
}

// open returns a StateDB for the state after block idx.
func (c *flattenChain) open(idx int) *StateDB {
	c.t.Helper()
	st, err := New(c.roots[idx], c.db)
	if err != nil {
		c.t.Fatalf("open state %d: %v", idx, err)
	}
	return st
}

// checkFlattenState verifies that st, opened at block idx, reads the values want.
func checkFlattenState(st *StateDB, idx int, want uint64) error {
	if nonce := st.GetNonce(flattenTestAddr); nonce != want {
		return fmt.Errorf("state %d: nonce %d, want %d (state error: %v)", idx, nonce, want, st.Error())
	}
	if slot := st.GetState(flattenTestAddr, flattenTestSlot); slot != common.BigToHash(new(big.Int).SetUint64(want)) {
		return fmt.Errorf("state %d: slot %x, want %d (state error: %v)", idx, slot, want, st.Error())
	}
	if err := st.Error(); err != nil {
		return fmt.Errorf("state %d: unexpected state error: %v", idx, err)
	}
	return nil
}

// TestReadAcrossFlattenSequences runs every short sequence of opening states
// near the flatten edge, committing blocks, rewinding and reading. Invariant:
// a read never returns values of a state other than the one it was opened at.
// Without a rewind every read must also succeed; after a rewind a read may
// fail instead.
//
// Events: O opens the disk layer state, P the bottom diff layer state, H the
// head state, C commits a block, B rewinds below the disk layer, G rewinds and
// commits a different branch of the same length (reorg), A reads the account
// and S a storage slot through every opened state.
func TestReadAcrossFlattenSequences(t *testing.T) {
	const maxDiffLayers = 2
	maxLen := 5
	if testing.Short() {
		maxLen = 4
	}
	events := []byte("OPHCBGAS")

	var run func(seq []byte)
	run = func(seq []byte) {
		// Run sequences that end in a read after at least one open.
		if len(seq) > 0 && (seq[len(seq)-1] == 'A' || seq[len(seq)-1] == 'S') && bytes.ContainsAny(seq, "OPH") {
			if err := runFlattenSequence(t, maxDiffLayers, seq); err != nil {
				t.Fatalf("sequence %s: %v", seq, err)
			}
		}
		if len(seq) == maxLen {
			return
		}
		for _, e := range events {
			run(append(append([]byte{}, seq...), e))
		}
	}
	run(nil)
}

func runFlattenSequence(t *testing.T, maxDiffLayers int, seq []byte) error {
	c := newFlattenChain(t, uint64(maxDiffLayers), maxDiffLayers+2)

	type opened struct {
		st      *StateDB
		idx     int
		want    uint64
		rewound bool // a rewind happened after the state was opened
	}
	var (
		states []opened
		reads  int
	)
	for _, e := range seq {
		head := len(c.roots) - 1
		open := func(idx int) {
			if idx >= 0 {
				states = append(states, opened{st: c.open(idx), idx: idx, want: c.vals[idx]})
			}
		}
		switch e {
		case 'O':
			open(c.disk)
		case 'P':
			if c.disk < head {
				open(c.disk + 1)
			}
		case 'H':
			open(head)
		case 'C':
			c.commit()
		case 'B', 'G':
			var rewound bool
			if e == 'B' {
				rewound = c.rewind()
			} else {
				rewound = c.reorg()
			}
			if rewound {
				for i := range states {
					states[i].rewound = true
				}
			}
		case 'A', 'S':
			reads++
			for _, s := range states {
				// A failed read makes the getters return zero values, so read
				// first and compare only when the reads succeeded.
				var got, fresh common.Hash
				if e == 'A' {
					got = common.BigToHash(new(big.Int).SetUint64(s.st.GetNonce(flattenTestAddr)))
				} else {
					got = s.st.GetState(flattenTestAddr, flattenTestSlot)
					// A slot not read before forces a fresh storage read.
					fresh = s.st.GetState(flattenTestAddr, common.BigToHash(big.NewInt(int64(1000+reads))))
				}
				if err := s.st.Error(); err != nil {
					if s.rewound {
						continue
					}
					return fmt.Errorf("state %d: read %c: %v", s.idx, e, err)
				}
				want := common.BigToHash(new(big.Int).SetUint64(s.want))
				if got != want || fresh != (common.Hash{}) {
					return fmt.Errorf("state %d: read %c got %x (fresh slot %x), want %x", s.idx, e, got, fresh, want)
				}
			}
		}
	}
	return nil
}

// TestReadAfterLayerFlatten opens the oldest state served by the live layer
// tree, commits one more block so that state is flattened past, and then reads
// through the already opened state.
func TestReadAfterLayerFlatten(t *testing.T) {
	const maxDiffLayers = 2
	c := newFlattenChain(t, maxDiffLayers, maxDiffLayers+2)

	idx := len(c.roots) - 1 - maxDiffLayers
	st := c.open(idx)

	c.commit()

	if err := checkFlattenState(st, idx, c.vals[idx]); err != nil {
		t.Fatal(err)
	}
}
