package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const maintenanceModeKey = "maintenance_mode"
const postPlacementsURL = "https://evropochta.by/mvc/application/ops/placement/"

func newOperationsRedis(url string) (*redis.Client, error) {
	options, err := redis.ParseURL(url)
	if err != nil {
		return nil, errors.New("invalid REDIS_URL for maintenance and readiness")
	}
	options.ContextTimeoutEnabled = true
	options.DisableIdentity = true
	options.Protocol = 2
	return redis.NewClient(options), nil
}

func registerOperations(mux *http.ServeMux, db database, auth *accountAuth) {
	var cache *redis.Client
	if auth != nil {
		cache = auth.operationsRedis
	}
	mux.HandleFunc("GET /api/health/ready", readinessHandler(db, cache))
	mux.HandleFunc("GET /api/post_placements", postPlacementsHandler(&http.Client{Timeout: 4 * time.Second}))
	mux.HandleFunc("GET /api/maintenance/status", maintenanceStatusHandler(cache))
	if cache != nil {
		mux.HandleFunc("GET /api/maintenance/enable", auth.maintenanceChange(true))
		mux.HandleFunc("GET /api/maintenance/disable", auth.maintenanceChange(false))
	}
}

func readinessHandler(db database, cache *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		w.Header().Set("Cache-Control", "no-store")
		var check int
		if db == nil || db.QueryRow(ctx, "SELECT 1").Scan(&check) != nil ||
			(cache != nil && cache.Ping(ctx).Err() != nil) {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, map[string]string{"status": "ready"})
	}
}

type maintenanceStatusResponse struct {
	Status  string `json:"status"`
	Enabled bool   `json:"enabled"`
	TTL     *int64 `json:"ttl"`
}

func maintenanceStatusHandler(cache *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if cache == nil {
			writeText(w, http.StatusServiceUnavailable, "Maintenance status unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		ttl, err := cache.TTL(ctx, maintenanceModeKey).Result()
		switch {
		case err != nil:
			writeText(w, http.StatusServiceUnavailable, "Maintenance status unavailable")
		case ttl == -2:
			writeJSON(w, maintenanceStatusResponse{Status: "disabled"})
		default:
			remaining := max(int64(ttl/time.Second), 0)
			writeJSON(w, maintenanceStatusResponse{Status: "enabled", Enabled: true, TTL: &remaining})
		}
	}
}

func (auth *accountAuth) maintenanceChange(enable bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, authenticated := auth.protectedSession(w, r)
		if !authenticated {
			return
		}
		if !*claims.IsAdmin {
			writeText(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		status, err := "disabled", error(nil)
		if enable {
			status = "enabled"
			ttl := time.Duration(0)
			if raw := r.URL.Query().Get("ttl"); raw != "" {
				seconds, err := strconv.ParseUint(raw, 10, 64)
				if err != nil || seconds > uint64(math.MaxInt64/int64(time.Second)) {
					writeText(w, http.StatusBadRequest, "Invalid maintenance TTL")
					return
				}
				if seconds == 0 {
					if err := auth.operationsRedis.Del(ctx, maintenanceModeKey).Err(); err != nil {
						writeText(w, http.StatusServiceUnavailable, "Maintenance status unavailable")
						return
					}
					writeJSON(w, maintenanceChangeResponse(status))
					return
				}
				ttl = time.Duration(seconds) * time.Second
			}
			err = auth.operationsRedis.Set(ctx, maintenanceModeKey, status, ttl).Err()
		} else {
			err = auth.operationsRedis.Del(ctx, maintenanceModeKey).Err()
		}
		if err != nil {
			writeText(w, http.StatusServiceUnavailable, "Maintenance status unavailable")
			return
		}
		writeJSON(w, maintenanceChangeResponse(status))
	}
}

func maintenanceChangeResponse(status string) map[string]string {
	return map[string]string{"status": status, "message": "Maintenance mode " + status + " successfully"}
}

type postPlacement struct {
	ID        int32  `json:"id"`
	Number    int32  `json:"number"`
	Address   string `json:"address"`
	Longitude string `json:"longitude"`
	Latitude  string `json:"latitude"`
}

func postPlacementsHandler(client *http.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		placements, err := loadPostPlacements(r.Context(), client)
		if err != nil {
			slog.Warn("Europochta placements unavailable")
			placements = []postPlacement{}
		}
		writeJSON(w, listResponse[postPlacement]{Status: "success", Length: len(placements), Data: placements})
	}
}

func loadPostPlacements(ctx context.Context, client *http.Client) ([]postPlacement, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, postPlacementsURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, errors.New("Europochta API returned a non-success status")
	}
	const maxPlacementsBytes = 4 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maxPlacementsBytes+1))
	if err != nil || len(data) > maxPlacementsBytes {
		return nil, errors.New("invalid Europochta response size")
	}
	var placements []postPlacement
	if err := json.Unmarshal(data, &placements); err != nil || placements == nil {
		return nil, errors.New("invalid Europochta response format")
	}
	for i := range placements {
		// The source sometimes escapes Unicode inside the JSON address string a second time.
		if address := placements[i].Address; strings.Contains(address, "\\") {
			var decoded string
			if json.Unmarshal([]byte(`"`+strings.ReplaceAll(address, `"`, `\"`)+`"`), &decoded) == nil {
				placements[i].Address = decoded
			}
		}
	}
	slices.SortStableFunc(placements, func(a, b postPlacement) int { return cmp.Compare(a.Number, b.Number) })
	return placements, nil
}
