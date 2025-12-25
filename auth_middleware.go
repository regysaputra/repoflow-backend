package main

import (
	"context"
	"net/http"
)

type contextKey string

const userIDKey contextKey = "userID"

// AuthMiddleware is middleware
func AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// For production use "Authorization: Bearer <token>"
		userID := r.Header.Get("X-User-ID")

		if userID == "" {
			SendJSON(w, http.StatusUnauthorized, Response{false, "Unauthorized"})
			return
		}

		ctx := context.WithValue(r.Context(), userIDKey, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
