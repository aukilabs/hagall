package receipt

import (
	"time"

	"github.com/aukilabs/hagall-common/errors"
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

func instrumentReceiptLatency(endpoint string, start time.Time) {
	receiptSendLatency.With(prometheus.Labels{
		ncsEndpointLabel: endpoint,
	}).Observe(time.Since(start).Seconds())
}

func instrumentReceiptSend(endpoint string) {
	receiptSend.With(prometheus.Labels{
		ncsEndpointLabel: endpoint,
	}).Inc()
}

func instrumentReceiptSendError(endpoint string, err error) {
	receiptSendError.
		With(prometheus.Labels{
			ncsEndpointLabel: endpoint,
			errTypeLabel:     errors.Type(err),
		}).
		Inc()
}

func instrumentReceiptVerificationError(err error) {
	receiptVerificationError.
		With(prometheus.Labels{
			errTypeLabel: errors.Type(err),
		}).
		Inc()
}
