package receipt

import (
	"bytes"
	"context"

	"github.com/aukilabs/go-tooling/pkg/errors"
	"github.com/aukilabs/go-tooling/pkg/logs"
	"github.com/aukilabs/hagall-common/ncsclient"
	"github.com/ethereum/go-ethereum/crypto"
)

type ReceiptHandler struct {
	NCSEndpoint string
	ReceiptChan chan ncsclient.ReceiptPayload //buffered
}

func (rh ReceiptHandler) HandleReceipts(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case payload := <-rh.ReceiptChan:
				if err := instrumentReceiptVerification(func() error {
					return rh.VerifyPayload(payload)
				}); err != nil {
					logs.Warn(errors.Newf("invalid receipt payload").
						WithTag("receipt", payload.Receipt).
						WithTag("hash", payload.Hash).
						WithTag("signature", payload.Signature).
						Wrap(err))

				} else {
					rh.ForwardToNCS(ctx, payload)
				}
			}
		}
	}()
}

func (rh ReceiptHandler) ForwardToNCS(ctx context.Context, payload ncsclient.ReceiptPayload) {
	go func() {
		client := ncsclient.NewNCSClient(rh.NCSEndpoint, nil)

		if err := instrumentReceiptSend(rh.NCSEndpoint, func() error {
			return client.PostReceipt(ctx, payload)
		}); err != nil {
			logs.Warn(errors.New("forward to network credit service failed").Wrap(err))
		}
	}()
}

func (rh ReceiptHandler) VerifyPayload(payload ncsclient.ReceiptPayload) error {
	// verify hash
	hash := crypto.Keccak256Hash([]byte(payload.Receipt))
	if !bytes.Equal(hash.Bytes(), payload.Hash) {
		return errors.New("failed to verify receipt hash")
	}

	// verify that signature is actually signature of some kind, and not some junk data
	_, err := crypto.Ecrecover(payload.Hash, payload.Signature)
	if err != nil {
		return errors.New("failed to verify signature").Wrap(err)
	}
	return nil
}
