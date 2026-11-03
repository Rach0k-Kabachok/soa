package marketplace

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"marketplace/catalog-service/internal/api"
)

type Server struct {
	db       *sql.DB
	secret   []byte
	interval time.Duration
}

type principal struct {
	ID   int64
	Role string
}

type principalKey struct{}

func current(r *http.Request) *principal {
	p, _ := r.Context().Value(principalKey{}).(*principal)
	return p
}

type problem struct {
	status int
	body   api.ErrorResponse
}

func (p *problem) Error() string { return p.body.Message }

func fail(status int, code, message string, details ...map[string]interface{}) error {
	p := &problem{status: status, body: api.ErrorResponse{
		ErrorCode: api.ErrorResponseErrorCode(code), Message: message,
	}}
	if len(details) > 0 {
		p.body.Details = &details[0]
	}
	return p
}

func reply(w http.ResponseWriter, status int, value any, err error) {
	if err != nil {
		var p *problem
		if !errors.As(err, &p) {
			slog.Error("request failed", "error", err.Error())
			p = &problem{status: 500, body: api.ErrorResponse{ErrorCode: "INTERNAL_ERROR", Message: "Ошибка сервера"}}
		}
		status, value = p.status, p.body
	}
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func decode[T any](r *http.Request) (T, error) {
	var body T
	err := json.NewDecoder(r.Body).Decode(&body)
	if err != nil {
		return body, fail(400, "VALIDATION_ERROR", "Неверное тело запроса", map[string]interface{}{"violations": []string{err.Error()}})
	}
	return body, nil
}

func uniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func requireRole(r *http.Request, roles ...string) error {
	for _, role := range roles {
		if current(r).Role == role {
			return nil
		}
	}
	return fail(403, "ACCESS_DENIED", "Недостаточно прав")
}

func (s *Server) transaction(ctx context.Context, fn func(*sql.Tx) (any, error)) (any, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	value, err := fn(tx)
	if err != nil {
		return nil, err
	}
	return value, tx.Commit()
}

func NewHandler(db *sql.DB, secret string, interval time.Duration) (http.Handler, error) {
	s := &Server{db: db, secret: []byte(secret), interval: interval}
	spec, err := api.GetSwagger()
	if err != nil {
		return nil, err
	}
	router, err := legacy.NewRouter(spec)
	if err != nil {
		return nil, err
	}
	validationError := func(w http.ResponseWriter, r *http.Request, err error) {
		var violations []string
		var collect func(error)
		collect = func(e error) {
			switch v := e.(type) {
			case openapi3.MultiError:
				for _, child := range v {
					collect(child)
				}
			case *openapi3filter.RequestError:
				collect(v.Err)
			case *openapi3.SchemaError:
				violations = append(violations, "/"+strings.Join(v.JSONPointer(), "/")+": "+v.Reason)
			default:
				violations = append(violations, e.Error())
			}
		}
		collect(err)
		reply(w, 0, nil, fail(400, "VALIDATION_ERROR", "Запрос не соответствует OpenAPI", map[string]interface{}{"violations": violations}))
	}
	generated := api.HandlerWithOptions(s, api.StdHTTPServerOptions{ErrorHandlerFunc: validationError})
	validated := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, params, err := router.FindRoute(r)
		if err == nil {
			err = openapi3filter.ValidateRequest(r.Context(), &openapi3filter.RequestValidationInput{
				Request: r, PathParams: params, Route: route,
				Options: &openapi3filter.Options{MultiError: true, AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
			})
		}
		if err != nil {
			validationError(w, r, err)
			return
		}
		generated.ServeHTTP(w, r)
	})
	return s.logging(s.authenticate(validated)), nil
}

func (s *Server) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "ok\n")
}

type loggedResponse struct {
	http.ResponseWriter
	status int
}

func (w *loggedResponse) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func redact(value any) any {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if strings.Contains(strings.ToLower(key), "password") || strings.Contains(strings.ToLower(key), "token") {
				v[key] = "***"
			} else {
				v[key] = redact(child)
			}
		}
	case []any:
		for i := range v {
			v[i] = redact(v[i])
		}
	}
	return value
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now().UTC()
		requestID := uuid.NewString()
		w.Header().Set("X-Request-Id", requestID)
		var body any
		if r.Method == "POST" || r.Method == "PUT" || r.Method == "DELETE" {
			data, err := io.ReadAll(r.Body)
			if err != nil || (len(data) > 0 && json.Unmarshal(data, &body) != nil) {
				body = "<invalid JSON>"
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
			body = redact(body)
		}
		p := &principal{}
		r = r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
		response := &loggedResponse{ResponseWriter: w, status: 200}
		next.ServeHTTP(response, r)
		var userID any
		if p.ID != 0 {
			userID = p.ID
		}
		fields := []any{"request_id", requestID, "method", r.Method, "endpoint", r.URL.Path,
			"status_code", response.status, "duration_ms", float64(time.Since(start).Microseconds()) / 1000,
			"user_id", userID, "timestamp", start.Format(time.RFC3339Nano)}
		if r.Method == "POST" || r.Method == "PUT" || r.Method == "DELETE" {
			fields = append(fields, "request_body", body)
		}
		slog.Info("api request", fields...)
	})
}
