package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

type AuthType string

const (
	AuthTypePublic AuthType = "public"
	AuthTypeKeys   AuthType = "keys"
	AuthTypeGitHub AuthType = "github"
	AuthTypeInvite AuthType = "invite"
)

const (
	DirPerm        = 0700
	FilePerm       = 0600
	PublicFilePerm = 0644
	NonceLength    = 32
	InviteTokenLen = 8
	RawKeyLength   = 64
)

type Authorizer interface {
	Authorize(pubKey ed25519.PublicKey) bool
}

type TokenAuthorizer interface {
	Authorizer
	AuthorizeWithToken(pubKey ed25519.PublicKey, token string) bool
}

type Authenticator interface {
	Sign(data []byte) ([]byte, error)
	PublicKey() ed25519.PublicKey
	Token() string
}

type KeyStore struct {
	keys map[string]bool
}

func NewKeyStore() *KeyStore {
	return &KeyStore{keys: make(map[string]bool)}
}

func (s *KeyStore) Add(pubKey ed25519.PublicKey) {
	s.keys[string(pubKey)] = true
}

func (s *KeyStore) Authorize(pubKey ed25519.PublicKey) bool {
	return s.keys[string(pubKey)]
}

func (s *KeyStore) LoadAuthorizedKeys(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return s.ParseAuthorizedKeys(data)
}

func (s *KeyStore) ParseAuthorizedKeys(data []byte) error {
	for len(data) > 0 {
		pubKey, _, _, rest, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			data = rest
			continue
		}
		if cryptoPubKey, ok := pubKey.(ssh.CryptoPublicKey); ok {
			if edKey, ok := cryptoPubKey.CryptoPublicKey().(ed25519.PublicKey); ok {
				s.Add(edKey)
			}
		}
		data = rest
	}
	return nil
}

func saveAuthorizedKey(pubKey ed25519.PublicKey) error {
	path := GetDefaultAuthorizedKeysPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, DirPerm); err != nil {
		return err
	}

	sshPubKey, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return err
	}
	keyLine := string(ssh.MarshalAuthorizedKey(sshPubKey))

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, FilePerm)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.WriteString(keyLine); err != nil {
		return err
	}
	return nil
}

type GitHubAuthorizer struct {
	users []string
	orgs  []string
	keys  *KeyStore
}

func NewGitHubAuthorizer(users []string, orgs []string) *GitHubAuthorizer {
	return &GitHubAuthorizer{
		users: users,
		orgs:  orgs,
		keys:  NewKeyStore(),
	}
}

func (g *GitHubAuthorizer) FetchKeys(ctx context.Context) error {
	for _, user := range g.users {
		req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("https://github.com/%s.keys", user), nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			continue
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		g.keys.ParseAuthorizedKeys(data)
	}
	return nil
}

func (g *GitHubAuthorizer) Authorize(pubKey ed25519.PublicKey) bool {
	return g.keys.Authorize(pubKey)
}

type PrivateKeyAuthenticator struct {
	privKey ed25519.PrivateKey
	token   string
}

func NewPrivateKeyAuthenticator(privKey ed25519.PrivateKey, token string) *PrivateKeyAuthenticator {
	return &PrivateKeyAuthenticator{privKey: privKey, token: token}
}

func (a *PrivateKeyAuthenticator) Sign(data []byte) ([]byte, error) {
	return ed25519.Sign(a.privKey, data), nil
}

func (a *PrivateKeyAuthenticator) PublicKey() ed25519.PublicKey {
	return a.privKey.Public().(ed25519.PublicKey)
}

func (a *PrivateKeyAuthenticator) Token() string {
	return a.token
}

func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := ssh.ParseRawPrivateKey(data)
	if err == nil {
		if edKey, ok := key.(*ed25519.PrivateKey); ok {
			return *edKey, nil
		}
	}
	if len(data) == RawKeyLength {
		return ed25519.PrivateKey(data), nil
	}
	return nil, errors.New("not an ed25519 private key")
}

func savePrivateKey(path string, privKey ed25519.PrivateKey) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, DirPerm); err != nil {
		return err
	}
	return os.WriteFile(path, privKey, FilePerm)
}

type Message struct {
	Type      string `json:"type"`
	Nonce     string `json:"nonce,omitempty"`
	Token     string `json:"token,omitempty"`
	Identity  string `json:"identity,omitempty"`
	Signature string `json:"signature,omitempty"`
	Error     string `json:"error,omitempty"`
}

func NewChallenge() (string, error) {
	nonce := make([]byte, NonceLength)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(nonce), nil
}

func Verify(pubKey ed25519.PublicKey, nonce string, signature string) bool {
	nonceBytes, err := base64.StdEncoding.DecodeString(nonce)
	if err != nil {
		return false
	}
	sigBytes, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return false
	}
	return ed25519.Verify(pubKey, nonceBytes, sigBytes)
}

func GetDefaultAuthorizedKeysPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".p2p", "authorized_keys")
}

func GetDefaultIDKeyPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".p2p", "id_ed25519")
}

func CreateAuthorizer(s string) (Authorizer, error) {
	if s == "" {
		return nil, nil
	}
	if s == "invite" {
		token := make([]byte, InviteTokenLen)
		rand.Read(token)
		tokenStr := hex.EncodeToString(token)
		fmt.Printf("\n--- INVITE MODE ---\n")
		fmt.Printf("Invite Token: p2p_invite_%s\n", tokenStr)
		fmt.Printf("Run this on client:\n  p2p connect <room> --auth p2p_invite_%s\n", tokenStr)
		fmt.Printf("-------------------\n\n")
		return &InviteAuthorizer{inviteToken: "p2p_invite_" + tokenStr}, nil
	}
	if strings.HasPrefix(s, "github:") {
		user := strings.TrimPrefix(s, "github:")
		a := NewGitHubAuthorizer([]string{user}, nil)
		if err := a.FetchKeys(context.Background()); err != nil {
			return nil, err
		}
		return a, nil
	}
	if strings.HasPrefix(s, "ssh-ed25519") {
		ks := NewKeyStore()
		if err := ks.ParseAuthorizedKeys([]byte(s)); err != nil {
			return nil, err
		}
		return ks, nil
	}
	ks := NewKeyStore()
	if err := ks.LoadAuthorizedKeys(s); err != nil {
		return nil, err
	}
	return ks, nil
}

func CreateAuthenticator(s string) (Authenticator, error) {
	var token string
	if strings.HasPrefix(s, "p2p_invite_") {
		token = s
		s = ""
	}

	if s == "" {
		defaultPath := GetDefaultIDKeyPath()
		if _, err := os.Stat(defaultPath); os.IsNotExist(err) {
			fmt.Println("No identity found. Generating new ed25519 key...")
			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				return nil, err
			}
			if err := savePrivateKey(defaultPath, priv); err != nil {
				return nil, err
			}
			pubSSH, _ := ssh.NewPublicKey(pub)
			os.WriteFile(defaultPath+".pub", ssh.MarshalAuthorizedKey(pubSSH), PublicFilePerm)
			s = defaultPath
		} else {
			s = defaultPath
		}
	}

	privKey, err := LoadPrivateKey(s)
	if err != nil {
		return nil, err
	}
	return NewPrivateKeyAuthenticator(privKey, token), nil
}

type InviteAuthorizer struct {
	mu          sync.Mutex
	pubKey      ed25519.PublicKey
	inviteToken string
}

func (a *InviteAuthorizer) Authorize(pubKey ed25519.PublicKey) bool {
	if a.inviteToken != "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pubKey == nil {
		a.pubKey = pubKey
		saveAuthorizedKey(pubKey)
		fmt.Printf("Invite Mode: Trust established and saved to %s\n", GetDefaultAuthorizedKeysPath())
		return true
	}
	return string(a.pubKey) == string(pubKey)
}

func (a *InviteAuthorizer) AuthorizeWithToken(pubKey ed25519.PublicKey, token string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inviteToken != "" && token == a.inviteToken {
		if a.pubKey == nil {
			a.pubKey = pubKey
			saveAuthorizedKey(pubKey)
			fmt.Printf("Invite Mode: Token verified. Trust established and saved to %s\n", GetDefaultAuthorizedKeysPath())
		}
		return string(a.pubKey) == string(pubKey)
	}
	return false
}

type MultiAuthorizer struct {
	Authorizers []Authorizer
}

func (m *MultiAuthorizer) Authorize(pubKey ed25519.PublicKey) bool {
	for _, a := range m.Authorizers {
		if a.Authorize(pubKey) {
			return true
		}
	}
	return false
}

func (m *MultiAuthorizer) AuthorizeWithToken(pubKey ed25519.PublicKey, token string) bool {
	for _, a := range m.Authorizers {
		if ta, ok := a.(TokenAuthorizer); ok {
			if ta.AuthorizeWithToken(pubKey, token) {
				return true
			}
		} else if token == "" {
			if a.Authorize(pubKey) {
				return true
			}
		}
	}
	return false
}

func (m *MultiAuthorizer) Add(a Authorizer) {
	if a != nil {
		m.Authorizers = append(m.Authorizers, a)
	}
}
