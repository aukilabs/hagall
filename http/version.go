package http

import (
	"net/http"
)

func HandleVersion(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(version))
	}
}
