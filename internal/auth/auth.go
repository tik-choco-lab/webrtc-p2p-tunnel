package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tik-choco-lab/webrtc-p2p-tunnel/internal/logger"
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
	AuthMsgTypeChallenge = "challenge"
	AuthMsgTypeResponse  = "response"
	AuthMsgTypeSuccess   = "success"
	AuthMsgTypeError     = "error"

	InviteTokenPrefix  = "p2p_invite_"
	DefaultConfigDir   = ".p2p"
	DefaultAuthKeyFile = "authorized_keys"
	DefaultIDKeyFile   = "id_ed25519"

	GitHubUserKeysURL  = "https://github.com/%s.keys"
	GitHubOrgKeysURL   = "https://api.github.com/orgs/%s/public_members"
	GitHubAcceptHeader = "application/vnd.github.v3+json"
	GitHubOrgPrefix    = "org/"
	AuthPrefixGitHub   = "github:"
	SSHPubKeyPrefixED  = "ssh-ed25519"

	InviteHeader    = "\n--- INVITE MODE ---\n"
	InviteFooter    = "-------------------\n\n"
	InviteClientMsg = "Run this on client:\n  p2p connect %s --auth %s\n"
	DefaultRoomID   = "<room>"
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
	ok := s.keys[string(pubKey)]
	if !ok {
		logger.Debug("KeyStore: Key not found in authorized list")
	} else {
		logger.Debug("KeyStore: Key authorized")
	}
	return ok
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
	targets []string
	keys    *KeyStore
}

func NewGitHubAuthorizer(targets []string) *GitHubAuthorizer {
	return &GitHubAuthorizer{
		targets: targets,
		keys:    NewKeyStore(),
	}
}

func (g *GitHubAuthorizer) FetchKeys(ctx context.Context) error {
	for _, target := range g.targets {
		if strings.HasPrefix(target, GitHubOrgPrefix) {
			if err := g.fetchOrgKeys(ctx, strings.TrimPrefix(target, GitHubOrgPrefix)); err != nil {
				return err
			}
			continue
		}
		if err := g.fetchUserKeys(ctx, target); err != nil {
			return err
		}
	}
	return nil
}

func (g *GitHubAuthorizer) fetchUserKeys(ctx context.Context, user string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf(GitHubUserKeysURL, user), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return g.keys.ParseAuthorizedKeys(data)
}

func (g *GitHubAuthorizer) fetchOrgKeys(ctx context.Context, org string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf(GitHubOrgKeysURL, org), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", GitHubAcceptHeader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var members []struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&members); err != nil {
		return err
	}
	for _, m := range members {
		_ = g.fetchUserKeys(ctx, m.Login)
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
	sigBytes, err2 := base64.StdEncoding.DecodeString(signature)
	if err != nil || err2 != nil {
		return false
	}
	return ed25519.Verify(pubKey, nonceBytes, sigBytes)
}

func Authorize(a Authorizer, pubKey ed25519.PublicKey, token string) bool {
	if a == nil {
		return true
	}
	if ta, ok := a.(TokenAuthorizer); ok {
		return ta.AuthorizeWithToken(pubKey, token)
	}
	return a.Authorize(pubKey)
}

func GetDefaultAuthorizedKeysPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, DefaultConfigDir, DefaultAuthKeyFile)
}

func GetDefaultIDKeyPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, DefaultConfigDir, DefaultIDKeyFile)
}

func CreateAuthorizer(s, roomID string) (Authorizer, error) {
	switch {
	case s == "":
		return nil, nil
	case s == string(AuthTypeInvite):
		return createInviteAuth(roomID)
	case strings.HasPrefix(s, AuthPrefixGitHub):
		return createGitHubAuth(strings.TrimPrefix(s, AuthPrefixGitHub))
	case strings.HasPrefix(s, SSHPubKeyPrefixED):
		return createSSHAuth(s)
	default:
		return createKeyStoreAuth(s)
	}
}

func createInviteAuth(roomID string) (Authorizer, error) {
	token := make([]byte, InviteTokenLen)
	rand.Read(token)
	tokenStr := hex.EncodeToString(token)
	invite := InviteTokenPrefix + tokenStr
	fmt.Printf(InviteHeader)
	logger.Info(fmt.Sprintf("Invite Token: %s", invite))
	if roomID == "" {
		roomID = DefaultRoomID
	}
	fmt.Printf(InviteClientMsg, roomID, invite)
	fmt.Printf(InviteFooter)
	return &InviteAuthorizer{inviteToken: invite}, nil
}

func createGitHubAuth(target string) (Authorizer, error) {
	a := NewGitHubAuthorizer([]string{target})
	if err := a.FetchKeys(context.Background()); err != nil {
		return nil, err
	}
	return a, nil
}

func createSSHAuth(s string) (Authorizer, error) {
	ks := NewKeyStore()
	if err := ks.ParseAuthorizedKeys([]byte(s)); err != nil {
		return nil, err
	}
	return ks, nil
}

func createKeyStoreAuth(s string) (Authorizer, error) {
	ks := NewKeyStore()
	if err := ks.LoadAuthorizedKeys(s); err != nil {
		return nil, err
	}
	return ks, nil
}

func CreateAuthenticator(s string) (Authenticator, error) {
	var token string
	if strings.HasPrefix(s, InviteTokenPrefix) {
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
		logger.Info("Invite Mode: Trust established and saved")
		return true
	}
	ok := string(a.pubKey) == string(pubKey)
	logger.Debug(fmt.Sprintf("Invite Mode: Authorize result: %v", ok))
	return ok
}

func (a *InviteAuthorizer) AuthorizeWithToken(pubKey ed25519.PublicKey, token string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inviteToken != "" && token == a.inviteToken {
		if a.pubKey == nil {
			a.pubKey = pubKey
			saveAuthorizedKey(pubKey)
			logger.Info("Invite Mode: Token verified. Trust established.")
		}
		ok := string(a.pubKey) == string(pubKey)
		logger.Debug(fmt.Sprintf("Invite Mode: AuthorizeWithToken result: %v", ok))
		return ok
	}
	logger.Debug("Invite Mode: Token mismatch or empty")
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
