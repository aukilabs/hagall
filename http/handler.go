package http

import "net/http"

func HandleHealthCheck(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func HandleReadyCheck(readinessCheck func() bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !readinessCheck() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func HandleVersion(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(version))
	}
}
