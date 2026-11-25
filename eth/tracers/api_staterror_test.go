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

package tracers

import (
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

var errInjectedRead = errors.New("injected state read failure")

// failingAccountDB wraps a state database so that reading one account fails,
// the way a reader does when its layer is flattened past mid-trace.
type failingAccountDB struct {
	state.Database
	addr common.Address
}

func (db *failingAccountDB) Reader(root common.Hash) (state.Reader, error) {
	r, err := db.Database.Reader(root)
	if err != nil {
		return nil, err
	}
	return &failingAccountReader{Reader: r, addr: db.addr}, nil
}

type failingAccountReader struct {
	state.Reader
	addr common.Address
}

func (r *failingAccountReader) Account(addr common.Address) (*types.StateAccount, error) {
	if addr == r.addr {
		return nil, errInjectedRead
	}
	return r.Reader.Account(addr)
}

// TestTraceTxReportsStateReadError checks that a failed state read is reported
// as such, not as the misleading nonce error it causes.
func TestTraceTxReportsStateReadError(t *testing.T) {
	t.Parallel()

	accounts := newAccounts(2)
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			accounts[0].addr: {Balance: big.NewInt(params.Ether)},
			accounts[1].addr: {Balance: big.NewInt(params.Ether)},
		},
	}
	signer := types.HomesteadSigner{}
	var target *types.Transaction
	backend := newTestBackend(t, 2, genesis, func(i int, b *core.BlockGen) {
		tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    uint64(i),
			To:       &accounts[1].addr,
			Value:    big.NewInt(1000),
			Gas:      params.TxGas,
			GasPrice: b.BaseFee(),
		}), signer, accounts[0].key)
		b.AddTx(tx)
		target = tx
	})
	defer backend.teardown()
	api := NewAPI(backend)

	block := backend.chain.GetBlockByNumber(2)
	parent := backend.chain.GetBlockByNumber(1)
	statedb, err := state.New(parent.Root(), &failingAccountDB{Database: backend.chain.StateCache(), addr: accounts[0].addr})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := core.TransactionToMessage(target, signer, block.BaseFee())
	if err != nil {
		t.Fatal(err)
	}
	txctx := &Context{BlockHash: block.Hash(), BlockNumber: block.Number(), TxIndex: 0, TxHash: target.Hash()}
	blockCtx := core.NewEVMBlockContext(block.Header(), api.chainContext(t.Context()), nil)

	_, _, err = api.traceTx(t.Context(), target, msg, txctx, blockCtx, statedb, nil, nil)
	if !errors.Is(err, errInjectedRead) {
		t.Fatalf("want the state read error, have %v", err)
	}
	if strings.Contains(err.Error(), "nonce") {
		t.Fatalf("state read error reported as a nonce error: %v", err)
	}
}
