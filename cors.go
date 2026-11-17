package main

import (
	"net/http"
	"slices"
	"strings"
)

var corsOrigins = []string{"http://localhost:4200", "https://localhost:4200", "https://skviderm.by"}
var corsMethods = []string{"GET", "POST", "PATCH", "DELETE", "PUT", "OPTIONS"}
var corsHeaders = []string{"content-type", "authorization", "accept", "x-csrf-token", "x-phone-csrf", "x-guest-csrf"}

func storefrontCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowedOrigin := slices.Contains(corsOrigins, origin)
		method := r.Header.Get("Access-Control-Request-Method")
		preflight := r.Method == http.MethodOptions && method != ""
		headers := w.Header()

		if preflight {
			problem := preflightProblem(origin, allowedOrigin, method, r.Header.Get("Access-Control-Request-Headers"))
			if problem != "" {
				headers["Content-Type"] = nil
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(problem))
				return
			}
			headers.Set("Access-Control-Allow-Methods", strings.Join(corsMethods, ", "))
			headers.Set("Access-Control-Allow-Headers", strings.Join(corsHeaders, ", "))
		}
		if allowedOrigin {
			headers.Set("Access-Control-Allow-Origin", origin)
		}
		headers.Set("Access-Control-Allow-Credentials", "true")
		headers.Add("Vary", "Origin, Access-Control-Request-Method, Access-Control-Request-Headers")
		if preflight {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func preflightProblem(origin string, allowedOrigin bool, method, requestHeaders string) string {
	switch {
	case origin == "":
		return "Request header `Origin` is required but is missing"
	case !allowedOrigin:
		return "Origin is not allowed to make this request"
	case !slices.Contains(corsMethods, method):
		return "Requested method is not allowed"
	}
	if requestHeaders != "" {
		for header := range strings.SplitSeq(requestHeaders, ",") {
			name := strings.ToLower(strings.TrimSpace(header))
			if !slices.Contains(corsHeaders, name) {
				return "One or more request headers are not allowed"
			}
		}
	}
	return ""
}
