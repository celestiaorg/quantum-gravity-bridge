package committer

import (
	"context"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/pkg/errors"
	log "github.com/xlab/suplog"

	"github.com/InjectiveLabs/peggo/orchestrator/ethereum/provider"
	"github.com/InjectiveLabs/peggo/orchestrator/ethereum/util"
	"github.com/InjectiveLabs/peggo/orchestrator/metrics"
)

// NewEthCommitter returns an instance of EVMCommitter, which
// can be used to submit txns into Ethereum, Matic, and other EVM-compatible networks.
func NewEthCommitter(
	fromAddress common.Address,
	fromSigner bind.SignerFn,
	evmProvider provider.EVMProviderWithRet,
	committerOpts ...EVMCommitterOption,
) (EVMCommitter, error) {
	committer := &ethCommitter{
		committerOpts: defaultOptions(),
		svcTags: metrics.Tags{
			"module": "eth_committer",
		},

		fromAddress: fromAddress,
		fromSigner:  fromSigner,
		evmProvider: evmProvider,
		nonceCache:  util.NewNonceCache(),
	}

	if err := applyOptions(committer.committerOpts, committerOpts...); err != nil {
		return nil, err
	}

	committer.nonceCache.Sync(fromAddress, func() (uint64, error) {
		nonce, err := evmProvider.PendingNonceAt(context.TODO(), fromAddress)
		return nonce, err
	})

	return committer, nil
}

type ethCommitter struct {
	committerOpts *options

	fromAddress common.Address
	fromSigner  bind.SignerFn

	evmProvider provider.EVMProviderWithRet
	nonceCache  util.NonceCache

	svcTags metrics.Tags
}

func (e *ethCommitter) FromAddress() common.Address {
	return e.fromAddress
}

func (e *ethCommitter) Provider() provider.EVMProvider {
	return e.evmProvider
}

func (e *ethCommitter) SendTx(
	ctx context.Context,
	recipient common.Address,
	txData []byte,
) (txHash common.Hash, err error) {
	metrics.ReportFuncCall(e.svcTags)
	doneFn := metrics.ReportFuncTiming(e.svcTags)
	defer doneFn()

	opts := &bind.TransactOpts{
		From:   e.fromAddress,
		Signer: e.fromSigner,

		GasPrice: e.committerOpts.GasPrice.BigInt(),
		GasLimit: e.committerOpts.GasLimit,
		Context:  ctx, // with RPC timeout
	}

	resyncNonces := func(from common.Address) {
		e.nonceCache.Sync(from, func() (uint64, error) {
			nonce, err := e.evmProvider.PendingNonceAt(context.TODO(), from)
			if err != nil {
				log.WithError(err).Warningln("unable to acquire nonce")
			}

			return nonce, err
		})
	}

	if err := e.nonceCache.Serialize(e.fromAddress, func() (err error) {
		nonce := e.nonceCache.Incr(e.fromAddress)
		var resyncUsed bool

		for {
			opts.Nonce = big.NewInt(nonce)
			opts.Context, _ = context.WithTimeout(ctx, e.committerOpts.RPCTimeout)

			tx := types.NewTransaction(opts.Nonce.Uint64(), recipient, nil, opts.GasLimit, opts.GasPrice, txData)
			signedTx, err := opts.Signer(opts.From, tx)
			if err != nil {
				e.nonceCache.Decr(e.fromAddress)

				err := errors.Wrap(err, "failed to sign transaction")

				return err
			}

			txHash = signedTx.Hash()

			txHashRet, err := e.evmProvider.SendTransactionWithRet(opts.Context, signedTx)
			if err == nil {
				// override with a real hash from node resp
				txHash = txHashRet
				return nil
			} else {
				log.WithFields(log.Fields{
					"txHash":    txHash.Hex(),
					"txHashRet": txHashRet.Hex(),
				}).WithError(err).Warningln("SendTransaction failed with error")
			}

			switch {
			case strings.Contains(err.Error(), "invalid sender"):
				e.nonceCache.Decr(e.fromAddress)

				err := errors.New("failed to sign transaction")

				return err
			case strings.Contains(err.Error(), "nonce is too low"),
				strings.Contains(err.Error(), "nonce is too high"),
				strings.Contains(err.Error(), "the tx doesn't have the correct nonce"):

				if resyncUsed {
					log.Errorf("nonces synced, but still wrong nonce for %s: %d", e.fromAddress, nonce)
					err = errors.Wrapf(err, "nonce %d mismatch", nonce)
					return err
				}

				resyncNonces(e.fromAddress)

				resyncUsed = true
				// try again with new nonce
				nonce = e.nonceCache.Incr(e.fromAddress)
				opts.Nonce = big.NewInt(nonce)

				continue

			default:
				if strings.Contains(err.Error(), "known transaction") {
					// skip one nonce step, try to send again
					nonce := e.nonceCache.Incr(e.fromAddress)
					opts.Nonce = big.NewInt(nonce)
					continue
				}

				if strings.Contains(err.Error(), "VM Exception") {
					// a VM execution consumes gas and nonce is increasing
					return err
				}

				e.nonceCache.Decr(e.fromAddress)

				return err
			}
		}
	}); err != nil {
		metrics.ReportFuncError(e.svcTags)

		return common.Hash{}, err
	}

	return txHash, nil
}