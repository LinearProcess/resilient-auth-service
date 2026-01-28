package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

var db *sql.DB
var rdb *redis.Client

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		requestID := time.Now().UnixNano()

		// Wrap ResponseWriter to capture status code
		ww := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(ww, r)

		duration := time.Since(start)

		log.Printf(
			"request_id=%d method=%s path=%s status=%d duration=%s",
			requestID,
			r.Method,
			r.URL.Path,
			ww.statusCode,
			duration,
		)
	})
}

func rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()
		ip := r.RemoteAddr

		key := "rate_limit:" + ip

		// Increment counter
		count, err := rdb.Incr(ctx, key).Result()
		if err != nil {
			log.Println("rate limit redis error:", err)
			next.ServeHTTP(w, r) // fail open for now
			return
		}

		// Set expiration if first request
		if count == 1 {
			rdb.Expire(ctx, key, time.Minute)
		}

		if count > 10 {
			log.Printf("RATE LIMITED ip=%s count=%d", ip, count)
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}


func healthHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	status := map[string]string{
		"service": "up",
	}

	// Check DB
	if err := db.PingContext(ctx); err != nil {
		status["database"] = "down"
	} else {
		status["database"] = "up"
	}

	// Check Redis
	if err := rdb.Ping(ctx).Err(); err != nil {
		status["redis"] = "down"
	} else {
		status["redis"] = "up"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func main() {
	// Postgres connection
	connStr := "postgres://authuser:authpass@postgres:5432/authdb?sslmode=disable"
	var err error
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal("DB connection error:", err)
	}

	// Redis connection
	rdb = redis.NewClient(&redis.Options{
		Addr: "redis:6379",
	})

	handler := rateLimitMiddleware(loggingMiddleware(http.HandlerFunc(healthHandler)))
	http.Handle("/health", handler)


	log.Println("Auth service running on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}


