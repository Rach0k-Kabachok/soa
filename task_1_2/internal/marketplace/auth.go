package marketplace

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
	"marketplace/catalog-service/internal/api"
)

func (s *Server) tokens(id int64, role string) (api.TokenResponse, error) {
	now := time.Now()
	sign := func(kind string, ttl time.Duration) (string, error) {
		return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"sub": strconv.FormatInt(id, 10), "role": role, "type": kind,
			"iat": now.Unix(), "exp": now.Add(ttl).Unix(),
		}).SignedString(s.secret)
	}
	access, err := sign("access", 15*time.Minute)
	if err != nil {
		return api.TokenResponse{}, err
	}
	refresh, err := sign("refresh", 7*24*time.Hour)
	return api.TokenResponse{AccessToken: access, RefreshToken: refresh, TokenType: "Bearer", ExpiresIn: 900}, err
}

func (s *Server) parse(raw, kind string) (principal, error) {
	token, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) { return s.secret, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil {
		return principal{}, err
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || claims["type"] != kind {
		return principal{}, errors.New("invalid token type")
	}
	subject, err := claims.GetSubject()
	if err != nil {
		return principal{}, err
	}
	id, err := strconv.ParseInt(subject, 10, 64)
	role, ok := claims["role"].(string)
	if err != nil || id <= 0 || !ok || (role != "USER" && role != "SELLER" && role != "ADMIN") {
		return principal{}, errors.New("invalid claims")
	}
	return principal{ID: id, Role: role}, nil
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || strings.HasPrefix(r.URL.Path, "/auth/") {
			next.ServeHTTP(w, r)
			return
		}
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			reply(w, 0, nil, fail(401, "TOKEN_INVALID", "Требуется Bearer access token"))
			return
		}
		p, err := s.parse(strings.TrimPrefix(header, "Bearer "), "access")
		if err != nil {
			code := "TOKEN_INVALID"
			if errors.Is(err, jwt.ErrTokenExpired) {
				code = "TOKEN_EXPIRED"
			}
			reply(w, 0, nil, fail(401, code, "Невалидный или просроченный access token"))
			return
		}
		*current(r) = p
		next.ServeHTTP(w, r)
	})
}

func (s *Server) Register(w http.ResponseWriter, r *http.Request) {
	body, err := decode[api.RegisterRequest](r)
	if err != nil {
		reply(w, 0, nil, err)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		reply(w, 0, nil, err)
		return
	}
	role := "USER"
	if body.Role != nil {
		role = string(*body.Role)
	}
	var user api.UserResponse
	err = s.db.QueryRowContext(r.Context(), `INSERT INTO users(username, password_hash, role) VALUES ($1,$2,$3) RETURNING id, username, role`,
		body.Username, string(hash), role).Scan(&user.Id, &user.Username, &user.Role)
	if uniqueViolation(err) {
		err = fail(409, "USERNAME_TAKEN", "Имя пользователя занято")
	}
	reply(w, 201, user, err)
}

func (s *Server) Login(w http.ResponseWriter, r *http.Request) {
	body, err := decode[api.LoginRequest](r)
	if err != nil {
		reply(w, 0, nil, err)
		return
	}
	var id int64
	var hash, role string
	err = s.db.QueryRowContext(r.Context(), `SELECT id, password_hash, role FROM users WHERE username=$1`, body.Username).Scan(&id, &hash, &role)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Password)) != nil) {
		err = fail(401, "INVALID_CREDENTIALS", "Неверное имя пользователя или пароль")
	}
	if err != nil {
		reply(w, 0, nil, err)
		return
	}
	tokens, err := s.tokens(id, role)
	reply(w, 200, tokens, err)
}

func (s *Server) Refresh(w http.ResponseWriter, r *http.Request) {
	body, err := decode[api.RefreshRequest](r)
	if err != nil {
		reply(w, 0, nil, err)
		return
	}
	p, err := s.parse(body.RefreshToken, "refresh")
	if err != nil {
		reply(w, 0, nil, fail(401, "REFRESH_TOKEN_INVALID", "Невалидный refresh token"))
		return
	}
	tokens, err := s.tokens(p.ID, p.Role)
	reply(w, 200, tokens, err)
}
