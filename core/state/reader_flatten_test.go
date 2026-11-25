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
	t     *testing.T
	tdb   *triedb.Database
	db    *CachingDB
	roots []common.Hash
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
	c := &flattenChain{t: t, tdb: tdb, db: NewDatabase(tdb, nil)}
	t.Cleanup(func() { tdb.Close() })
	for i := 0; i < blocks; i++ {
		c.commit()
	}
	return c
}

// commit adds one block that sets the test account's nonce and slot to the
// block's position in the chain plus one.
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
	n := uint64(len(c.roots)) + 1
	st.SetNonce(flattenTestAddr, n, tracing.NonceChangeUnspecified)
	st.SetState(flattenTestAddr, flattenTestSlot, common.BigToHash(new(big.Int).SetUint64(n)))
	root, err := st.Commit(uint64(len(c.roots)), true, false)
	if err != nil {
		c.t.Fatal(err)
	}
	c.roots = append(c.roots, root)
	c.waitIndexed()
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

// checkFlattenState verifies that st, opened at block idx, reads that block's values.
func checkFlattenState(st *StateDB, idx int) error {
	want := uint64(idx) + 1
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
// near the flatten edge, committing blocks and reading, and checks that every
// read returns the values of the state it was opened at.
//
// Events: O opens the disk layer state, P the bottom diff layer state, H the
// head state, C commits a block, R reads through every opened state.
func TestReadAcrossFlattenSequences(t *testing.T) {
	const (
		maxDiffLayers = 2
		maxLen        = 5
	)
	events := []byte("OPHCR")

	var run func(seq []byte)
	run = func(seq []byte) {
		if len(seq) > 0 && seq[len(seq)-1] == 'R' {
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
		st  *StateDB
		idx int
	}
	var (
		states []opened
		reads  int
	)
	for _, e := range seq {
		head := len(c.roots) - 1
		switch e {
		case 'O':
			states = append(states, opened{c.open(head - maxDiffLayers), head - maxDiffLayers})
		case 'P':
			states = append(states, opened{c.open(head - maxDiffLayers + 1), head - maxDiffLayers + 1})
		case 'H':
			states = append(states, opened{c.open(head), head})
		case 'C':
			c.commit()
		case 'R':
			reads++
			for _, s := range states {
				if err := checkFlattenState(s.st, s.idx); err != nil {
					return err
				}
				// A slot not read before forces a fresh storage read.
				fresh := common.BigToHash(big.NewInt(int64(1000 + reads)))
				if v := s.st.GetState(flattenTestAddr, fresh); v != (common.Hash{}) {
					return fmt.Errorf("state %d: unset slot reads %x", s.idx, v)
				}
				if err := s.st.Error(); err != nil {
					return fmt.Errorf("state %d: fresh slot read error: %v", s.idx, err)
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

	if err := checkFlattenState(st, idx); err != nil {
		t.Fatal(err)
	}
}
