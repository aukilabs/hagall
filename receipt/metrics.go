package receipt

import (
	"time"

	"github.com/aukilabs/go-tooling/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	errTypeLabel     = "error_type"
	ncsEndpointLabel = "ncs_endpoint"
)

var (
	receiptSend = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "receipt_send",
		Help: "The number of receipts sent to NCS.",
	}, []string{
		ncsEndpointLabel,
	})

	receiptSendError = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "receipt_send_errors",
		Help: "The errors that occured while sending a receipt to NCS.",
	}, []string{
		ncsEndpointLabel,
		errTypeLabel,
	})

	receiptSendLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "receipt_send_latency",
		Help: "The time to send a receipt to NCS.",
	}, []string{
		ncsEndpointLabel,
	})

	receiptVerificationError = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "receipt_verification_errors",
		Help: "Invalid receipt counter.",
	}, []string{
		errTypeLabel,
	})
)

func instrumentReceiptSend(endpoint string, fn func() error) error {
	start := time.Now()

	err := fn()

	receiptSendLatency.With(prometheus.Labels{
		ncsEndpointLabel: endpoint,
	}).Observe(time.Since(start).Seconds())

	receiptSend.With(prometheus.Labels{
		ncsEndpointLabel: endpoint,
	}).Inc()

	if err != nil {
		receiptSendError.
			With(prometheus.Labels{
				ncsEndpointLabel: endpoint,
				errTypeLabel:     errors.Type(err),
			}).
			Inc()
		return err
	}

	return nil
}

func instrumentReceiptVerification(fn func() error) error {
	if err := fn(); err != nil {
		receiptVerificationError.
			With(prometheus.Labels{
				errTypeLabel: errors.Type(err),
			}).
			Inc()
		return err
	}
	return nil
}
