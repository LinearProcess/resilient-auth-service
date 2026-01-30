package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/google/uuid"

)

var db *sql.DB
var rdb *redis.Client
var ctx = context.Background()

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

func initDB() {
	query := `
	CREATE TABLE IF NOT EXISTS users (
		id SERIAL PRIMARY KEY,
		email TEXT UNIQUE NOT NULL,
		password_hash TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);`

	_, err := db.Exec(query)
	if err != nil {
		log.Fatal("Failed to create users table:", err)
	}
}

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func registerHandler(w http.ResponseWriter, r *http.Request) {
	var req registerRequest

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	_, err = db.Exec("INSERT INTO users (email, password_hash) VALUES ($1, $2)", req.Email, string(hash))
	if err != nil {
		http.Error(w, "User already exists", http.StatusConflict)
		return
	}

	w.WriteHeader(http.StatusCreated)
	w.Write([]byte("User registered"))
}

func waitForDB() {
	for i := 0; i < 10; i++ {
		err := db.Ping()
		if err == nil {
			log.Println("Connected to DB")
			return
		}
		log.Println("Waiting for DB...")
		time.Sleep(2 * time.Second)
	}
	log.Fatal("DB never became ready")
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	var req loginRequest

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	var storedHash string
	err := db.QueryRow("SELECT password_hash FROM users WHERE email=$1", req.Email).Scan(&storedHash)
	if err != nil {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	err = bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(req.Password))
	if err != nil {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	// Create session ID
	sessionID := uuid.New().String()

	// Store session in Redis (user email tied to session)
	err = rdb.Set(ctx, "session:"+sessionID, req.Email, time.Hour*24).Err()
	if err != nil {
		http.Error(w, "Session error", http.StatusInternalServerError)
		return
	}

	// Send secure cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "session_id",
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		Secure:   false, // true in production (HTTPS)
		SameSite: http.SameSiteLaxMode,
	})

	w.Write([]byte("Logged in"))
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		cookie, err := r.Cookie("session_id")
		if err != nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		email, err := rdb.Get(ctx, "session:"+cookie.Value).Result()
		if err != nil {
			http.Error(w, "Session expired or invalid", http.StatusUnauthorized)
			return
		}

		// Add user email to request context
		ctxWithUser := context.WithValue(r.Context(), "userEmail", email)
		next.ServeHTTP(w, r.WithContext(ctxWithUser))
	})
}

func meHandler(w http.ResponseWriter, r *http.Request) {
	email := r.Context().Value("userEmail").(string)
	w.Write([]byte("Hello " + email))
}

func main() {
	// Postgres connection
	connStr := "postgres://authuser:authpass@postgres:5432/authdb?sslmode=disable"
	var err error
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal("DB connection error:", err)
	}

	waitForDB()
	initDB()

	// Redis connection
	rdb = redis.NewClient(&redis.Options{
		Addr: "redis:6379",
	})

	// Routes
	http.Handle("/health",
		rateLimitMiddleware(loggingMiddleware(http.HandlerFunc(healthHandler))),
	)

	http.Handle("/register",
		rateLimitMiddleware(loggingMiddleware(http.HandlerFunc(registerHandler))),
	)

	http.Handle("/login",
		rateLimitMiddleware(loggingMiddleware(http.HandlerFunc(loginHandler))),
	)

	http.Handle("/me",
		authMiddleware(
			rateLimitMiddleware(
				loggingMiddleware(http.HandlerFunc(meHandler)),
			),
		),
	)

	log.Println("Auth service running on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}


