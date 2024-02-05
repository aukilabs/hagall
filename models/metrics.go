package models

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	appKeyLabel = "app_key"
)

var (
	hagallSessionCount = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "session_count",
		Help: "The number of sessions.",
	}, []string{appKeyLabel})

	hagallSessionCountTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "session_count_total",
		Help: "The total number of sessions.",
	}, []string{appKeyLabel})
)

func instrumentIncreaseSessionGauge(appKey string) {
	hagallSessionCount.
		With(prometheus.Labels{appKeyLabel: appKey}).
		Inc()
}

func instrumentDecreaseSessionGauge(appKey string) {
	hagallSessionCount.
		With(prometheus.Labels{appKeyLabel: appKey}).
		Dec()
}

func instrumentCountSession(appKey string) {
	hagallSessionCountTotal.
		With(prometheus.Labels{appKeyLabel: appKey}).
		Inc()
}
