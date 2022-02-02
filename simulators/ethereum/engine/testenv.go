package main

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/hive/hivesim"
)

// TestEnv is the environment of a single test.
type TestEnv struct {
	*hivesim.T
	TestName     string
	RPC          *rpc.Client
	Eth          *ethclient.Client
	Engine       *EngineClient
	CLMock       *CLMocker
	Vault        *Vault
	Timeout      <-chan time.Time
	PoSSync      chan interface{}
	ClientParams hivesim.Params

	// This holds most recent context created by the Ctx method.
	// Every time Ctx is called, it creates a new context with the default
	// timeout and cancels the previous one.
	lastCtx    context.Context
	lastCancel context.CancelFunc
	syncCancel context.CancelFunc
}

func RunTest(testName string, ttd *big.Int, t *hivesim.T, c *hivesim.Client, fn func(*TestEnv), cParams hivesim.Params) {
	// Setup the CL Mocker for this test
	clMocker := NewCLMocker(t, ttd)
	defer func() {
		clMocker.Shutdown()
		select {
		case <-clMocker.OnExit:
			// Happy path
		case <-time.After(time.Second * 10):
			t.Fatalf("FAIL (%s): Timeout on wait for CLMocker to exit", testName)
		}

	}()

	vault = newVault()

	// Add main client to CLMocker
	clMocker.AddEngineClient(t, c)

	// This sets up debug logging of the requests and responses.
	client := &http.Client{
		Transport: &loggingRoundTrip{
			t:     t,
			hc:    c,
			inner: http.DefaultTransport,
		},
	}

	// Create Engine client from main hivesim.Client to be used by tests
	ec := NewEngineClient(t, c)
	defer ec.Close()

	rpcClient, _ := rpc.DialHTTPWithClient(fmt.Sprintf("http://%v:8545/", c.IP), client)
	defer rpcClient.Close()
	env := &TestEnv{
		T:            t,
		TestName:     testName,
		RPC:          rpcClient,
		Eth:          ethclient.NewClient(rpcClient),
		Engine:       ec,
		CLMock:       clMocker,
		Vault:        vault,
		PoSSync:      make(chan interface{}, 1),
		ClientParams: cParams,
	}

	// Defer closing the last context
	defer func() {
		if env.lastCtx != nil {
			env.lastCancel()
		}
	}()

	// Create test end channel and defer closing it
	testend := make(chan interface{})
	defer func() { close(testend) }()

	// Start thread to wait for client to be synced to the latest PoS block
	defer func() {
		if env.syncCancel != nil {
			env.syncCancel()
		}
	}()
	go func() {
		syncRpcClient, err := rpc.DialHTTPWithClient(fmt.Sprintf("http://%v:8545/", c.IP), client)
		if err != nil {
			t.Logf("WARN (%v): Unable to create Eth client for PoS sync routine", env.TestName)
			close(env.PoSSync)
			return
		}
		eth := ethclient.NewClient(syncRpcClient)
		var ctx context.Context
		for {
			select {
			case <-testend:
				close(env.PoSSync)
				return
			case <-clMocker.OnExit:
				t.Logf("WARN (%v): CLMocker finished block production while waiting for PoS sync", env.TestName)
				close(env.PoSSync)
				return
			case <-time.After(time.Second):
				if clMocker.TTDReached {
					ctx, env.syncCancel = context.WithTimeout(context.Background(), rpcTimeout)
					bn, err := eth.BlockNumber(ctx)
					env.syncCancel = nil
					if err != nil {
						t.Logf("WARN (%v): Unable to obtain latest block", env.TestName)
						close(env.PoSSync)
						return
					}
					if clMocker.LatestFinalizedNumber != nil && bn >= clMocker.LatestFinalizedNumber.Uint64() {
						t.Logf("INFO (%v): Client is now synced to latest PoS block", env.TestName)
						env.PoSSync <- nil
						return
					}
				}
			}
		}

	}()

	// Setup timeout
	env.Timeout = time.After(DefaultTestCaseTimeout)

	// Run the test
	fn(env)
}

func (t *TestEnv) StartClient(clientType string, params hivesim.Params) (*hivesim.Client, *EngineClient, error) {
	c := t.T.StartClient(clientType, params, files)
	ec := NewEngineClient(t.T, c)
	return c, ec, nil
}

// Wait for a client to reach sync status past the PoS transition, with `DefaultPoSSyncTimeout` timeout
func (t *TestEnv) WaitForPoSSync() {
	select {
	case <-time.After(DefaultPoSSyncTimeout):
		t.Fatalf("FAIL (%v): timeout waiting for PoS sync", t.TestName)
	case resp, open := <-t.PoSSync:
		if !open {
			// PoS sync routine failed or timed-out
			t.Fatalf("FAIL (%v): Error during wait of PoS sync routine", t.TestName)
		}
		t.PoSSync <- resp
	}
}

// Naive generic function that works in all situations.
// A better solution is to use logs to wait for confirmations.
func (t *TestEnv) WaitForTxConfirmations(txHash common.Hash, n uint64) (*types.Receipt, error) {
	var (
		receipt *types.Receipt
		err     error
	)

	for i := 0; i < 90; i++ {
		receipt, err = t.Eth.TransactionReceipt(t.Ctx(), txHash)
		if err != nil && err != ethereum.NotFound {
			return nil, err
		}
		if receipt != nil {
			fmt.Printf("WaitForTxConfirmations: Got receipt for %v\n", txHash)
			break
		}
		time.Sleep(time.Second)
	}
	if receipt == nil {
		return nil, ethereum.NotFound
	}

	for i := 0; i < 90; i++ {
		currentBlock, err := t.Eth.BlockByNumber(t.Ctx(), nil)
		if err != nil {
			return nil, err
		}

		if currentBlock.NumberU64() >= receipt.BlockNumber.Uint64()+n {
			fmt.Printf("WaitForTxConfirmations: Reached confirmation block (%v) for %v\n", currentBlock.NumberU64(), txHash)
			if checkReceipt, err := t.Eth.TransactionReceipt(t.Ctx(), txHash); checkReceipt != nil {
				if bytes.Compare(receipt.PostState, checkReceipt.PostState) == 0 && receipt.BlockHash == checkReceipt.BlockHash {
					return checkReceipt, nil
				} else { // chain reorg
					return t.WaitForTxConfirmations(txHash, n)
				}
			} else {
				return nil, err
			}
		}

		time.Sleep(time.Second)
	}

	return nil, ethereum.NotFound
}

func (t *TestEnv) WaitForBlock(blockNumber *big.Int) (*types.Block, error) {
	for i := 0; i < 90; i++ {
		currentHeader, err := t.Eth.BlockByNumber(t.Ctx(), nil)
		if err != nil {
			return nil, err
		}
		if currentHeader.Number().Cmp(blockNumber) == 0 {
			return currentHeader, nil
		} else if currentHeader.Number().Cmp(blockNumber) > 0 {
			prevHeader, err := t.Eth.BlockByNumber(t.Ctx(), blockNumber)
			if err != nil {
				return nil, err
			}
			return prevHeader, nil
		}
		time.Sleep(time.Second)
	}
	return nil, nil
}

// Sets the fee recipient for the next block and returns the number where it will be included.
// A transaction can be included to be sent before getPayload if necessary
func (t *TestEnv) setNextFeeRecipient(feeRecipient common.Address, ec *EngineClient, tx *types.Transaction) (*big.Int, error) {
	for {
		select {
		case <-t.CLMock.OnPayloadPrepare:
			// Will yield later.
		case <-t.CLMock.OnExit:
			t.Fatalf("FAIL (%v): CLMocker stopped producing blocks", t.TestName)
		case <-t.Timeout:
			t.Fatalf("FAIL (%v): Test timeout", t.TestName)
		}

		if ec == nil || (t.CLMock.NextBlockProducer != nil && t.CLMock.NextBlockProducer.Equals(ec)) {
			defer t.CLMock.OnPayloadPrepare.Yield()
			t.CLMock.NextFeeRecipient = feeRecipient
			if tx != nil {
				err := ec.Eth.SendTransaction(ec.Ctx(), tx)
				if err != nil {
					return nil, err
				}
			}
			return big.NewInt(t.CLMock.LatestFinalizedNumber.Int64() + 1), nil
		}
		// Unlock and keep trying to get the requested Engine Client
		t.CLMock.OnPayloadPrepare.Yield()
		time.Sleep(PoSBlockProductionPeriod)
	}
}

// CallContext is a helper method that forwards a raw RPC request to
// the underlying RPC client. This can be used to call RPC methods
// that are not supported by the ethclient.Client.
func (t *TestEnv) CallContext(ctx context.Context, result interface{}, method string, args ...interface{}) error {
	return t.RPC.CallContext(ctx, result, method, args...)
}

// Ctx returns a context with the default timeout.
// For subsequent calls to Ctx, it also cancels the previous context.
func (t *TestEnv) Ctx() context.Context {
	if t.lastCtx != nil {
		t.lastCancel()
	}
	t.lastCtx, t.lastCancel = context.WithTimeout(context.Background(), rpcTimeout)
	return t.lastCtx
}
