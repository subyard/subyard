package githubbroker

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultBaseURL = "https://api.github.com"
	maxConfigBytes = 16 << 10
	maxKeyBytes    = 128 << 10
	maxResponse    = 64 << 10
	requestLimit   = 256
)

var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9._~+\-/=]+$`)

// Config identifies the one GitHub App installation an owner-host broker serves.
// PrivateKeyFile is read only by the owner-side issuer and is never returned.
type Config struct {
	AppID          string `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	PrivateKeyFile string `json:"private_key_file"`
}

type Token struct {
	Value     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Issuer struct {
	cfg    Config
	key    *rsa.PrivateKey
	client *http.Client
	base   string
	now    func() time.Time
}

func LoadConfig(path, defaultKeyFile string) (Config, error) {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n") {
		return Config{}, errors.New("github broker config path must be absolute")
	}
	data, err := readProtected(path, maxConfigBytes)
	if err != nil {
		return Config{}, fmt.Errorf("read github broker config: %w", err)
	}
	var cfg Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, errors.New("invalid github broker config")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return Config{}, errors.New("invalid github broker config")
	}
	if cfg.PrivateKeyFile == "" {
		cfg.PrivateKeyFile = defaultKeyFile
		if cfg.PrivateKeyFile == "" {
			cfg.PrivateKeyFile = filepath.Join(filepath.Dir(path), "generated", "github", "github-app.pem")
		}
	}
	return cfg, validateConfig(cfg)
}

func NewIssuer(cfg Config) (*Issuer, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	key, err := loadKey(cfg.PrivateKeyFile)
	if err != nil {
		return nil, err
	}
	return &Issuer{cfg: cfg, key: key, client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: defaultBaseURL, now: time.Now}, nil
}

func (issuer *Issuer) Issue(ctx context.Context) (Token, error) {
	if issuer == nil || issuer.key == nil || issuer.client == nil || issuer.base == "" {
		return Token{}, errors.New("github broker is not configured")
	}
	now := issuer.now().UTC()
	claims := map[string]any{"iat": now.Add(-60 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": issuer.cfg.AppID}
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	encodedHeader, err := jsonPart(header)
	if err != nil {
		return Token{}, errors.New("create github app assertion")
	}
	encodedClaims, err := jsonPart(claims)
	if err != nil {
		return Token{}, errors.New("create github app assertion")
	}
	unsigned := encodedHeader + "." + encodedClaims
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, issuer.key, crypto.SHA256, digest[:])
	if err != nil {
		return Token{}, errors.New("sign github app assertion")
	}
	assertion := unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
	body := bytes.NewReader([]byte(`{}`))
	url := issuer.base + "/app/installations/" + strconv.FormatInt(issuer.cfg.InstallationID, 10) + "/access_tokens"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return Token{}, errors.New("create github token request")
	}
	request.Header.Set("Authorization", "Bearer "+assertion)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "subyard-github-broker")
	response, err := issuer.client.Do(request)
	if err != nil {
		return Token{}, errors.New("github token request failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse {
		return Token{}, errors.New("github token response is too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Token{}, errors.New("github token request was rejected")
	}
	var result struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &result); err != nil || len(result.Token) > 4096 || !tokenPattern.MatchString(result.Token) {
		return Token{}, errors.New("invalid github token response")
	}
	if result.ExpiresAt.IsZero() || !result.ExpiresAt.After(now) || result.ExpiresAt.After(now.Add(75*time.Minute)) {
		return Token{}, errors.New("invalid github token expiry")
	}
	return Token{Value: result.Token, ExpiresAt: result.ExpiresAt}, nil
}

// Handler exposes only the empty POST /token operation and secret-free GET /status.
type Handler struct{ Issuer *Issuer }

func NewHandler(issuer *Issuer) http.Handler { return Handler{Issuer: issuer} }

func (handler Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.RawQuery != "" {
		http.Error(writer, "query is not supported", http.StatusBadRequest)
		return
	}
	switch request.URL.Path {
	case "/status":
		if request.Method != http.MethodGet {
			writer.Header().Set("Allow", http.MethodGet)
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if handler.Issuer == nil {
			http.Error(writer, "github broker is not configured", http.StatusServiceUnavailable)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"configured": true})
	case "/token":
		if request.Method != http.MethodPost {
			writer.Header().Set("Allow", http.MethodPost)
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if request.ContentLength > requestLimit {
			http.Error(writer, "request body is too large", http.StatusRequestEntityTooLarge)
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, requestLimit+1))
		if err != nil || len(body) != 0 {
			http.Error(writer, "request body must be empty", http.StatusBadRequest)
			return
		}
		token, err := handler.Issuer.Issue(request.Context())
		if err != nil {
			http.Error(writer, "github token issuance failed", http.StatusBadGateway)
			return
		}
		writeJSON(writer, http.StatusOK, token)
	default:
		http.NotFound(writer, request)
	}
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func jsonPart(value any) (string, error) {
	data, err := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(data), err
}

func loadKey(path string) (*rsa.PrivateKey, error) {
	data, err := readProtected(path, maxKeyBytes)
	if err != nil {
		return nil, errors.New("read github private key")
	}
	defer clear(data)
	block, rest := pem.Decode(data)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("invalid github private key")
	}
	var key *rsa.PrivateKey
	if parsed, parseErr := x509.ParsePKCS1PrivateKey(block.Bytes); parseErr == nil {
		key = parsed
	} else if parsedAny, parseErr := x509.ParsePKCS8PrivateKey(block.Bytes); parseErr == nil {
		var ok bool
		key, ok = parsedAny.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("github private key is not RSA")
		}
	} else {
		return nil, errors.New("invalid github private key")
	}
	if key.N == nil || key.N.BitLen() < 2048 {
		return nil, errors.New("github private key is too small")
	}
	return key, nil
}

func validateConfig(cfg Config) error {
	if cfg.AppID == "" || len(cfg.AppID) > 64 || strings.IndexFunc(cfg.AppID, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return errors.New("invalid github app id")
	}
	if cfg.InstallationID <= 0 {
		return errors.New("invalid github installation id")
	}
	if !filepath.IsAbs(cfg.PrivateKeyFile) || strings.ContainsAny(cfg.PrivateKeyFile, "\r\n") {
		return errors.New("github private key path must be absolute")
	}
	return nil
}

func readProtected(path string, maximum int64) ([]byte, error) {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n") {
		return nil, errors.New("path is invalid")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("cannot open protected file")
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("file is not a regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() || (info.Mode().Perm() != 0o600 && info.Mode().Perm() != 0o400) {
		return nil, errors.New("file permissions are unsafe")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		return nil, errors.New("file is too large")
	}
	return data, nil
}
