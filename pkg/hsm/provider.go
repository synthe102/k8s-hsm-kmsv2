// Package hsm provides a PKCS#11-based HSM provider for AES-256-GCM
// encrypt/decrypt operations used by the Kubernetes KMS v2 plugin.
package hsm

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/miekg/pkcs11"
)

const (
	gcmIVSize      = 12  // AES-GCM IV size in bytes
	gcmTagBits     = 128 // AES-GCM tag size in bits
	aes256KeyBytes = 32  // AES-256 key size in bytes
)

// Provider wraps a PKCS#11 session and KEK handle for envelope encryption.
type Provider struct {
	ctx       *pkcs11.Ctx
	session   pkcs11.SessionHandle
	kekHandle pkcs11.ObjectHandle
	keyID     string
	mu        sync.Mutex
}

// Config holds PKCS#11 provider configuration.
type Config struct {
	LibPath       string // Path to PKCS#11 shared library (.so / .dylib)
	TokenLabel    string // PKCS#11 token label to find the slot
	Pin           string // PKCS#11 user PIN
	KeyLabel      string // Label of the AES-256 KEK in the HSM
	SlotID        uint   // Explicit slot ID (used when UseSlotID is true)
	UseSlotID     bool   // Use SlotID instead of TokenLabel to find the slot
	AutoCreateKey bool   // Create the KEK automatically if not found
}

// NewProvider initializes the PKCS#11 library, opens a session, logs in,
// and locates (or creates) the AES-256 KEK.
func NewProvider(cfg Config) (*Provider, error) {
	ctx := pkcs11.New(cfg.LibPath)
	if ctx == nil {
		return nil, fmt.Errorf("failed to load PKCS#11 library: %s", cfg.LibPath)
	}

	if err := ctx.Initialize(); err != nil {
		ctx.Destroy()
		return nil, fmt.Errorf("PKCS#11 initialize: %w", err)
	}

	slot, err := findSlot(ctx, cfg.TokenLabel, cfg.SlotID, cfg.UseSlotID)
	if err != nil {
		ctx.Finalize()
		ctx.Destroy()
		return nil, err
	}

	session, err := ctx.OpenSession(slot, pkcs11.CKF_SERIAL_SESSION|pkcs11.CKF_RW_SESSION)
	if err != nil {
		ctx.Finalize()
		ctx.Destroy()
		return nil, fmt.Errorf("open session on slot %d: %w", slot, err)
	}

	if err := ctx.Login(session, pkcs11.CKU_USER, cfg.Pin); err != nil {
		ctx.CloseSession(session)
		ctx.Finalize()
		ctx.Destroy()
		return nil, fmt.Errorf("login: %w", err)
	}

	kekHandle, err := findOrCreateKEK(ctx, session, cfg.KeyLabel, cfg.AutoCreateKey)
	if err != nil {
		ctx.Logout(session)
		ctx.CloseSession(session)
		ctx.Finalize()
		ctx.Destroy()
		return nil, err
	}

	h := sha256.Sum256([]byte(cfg.KeyLabel))
	keyID := hex.EncodeToString(h[:8])

	return &Provider{
		ctx:       ctx,
		session:   session,
		kekHandle: kekHandle,
		keyID:     keyID,
	}, nil
}

// Encrypt encrypts plaintext using AES-256-GCM with the HSM KEK.
// Returns iv || ciphertext || tag.
func (p *Provider) Encrypt(plaintext []byte) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	iv := make([]byte, gcmIVSize)
	if _, err := rand.Read(iv); err != nil {
		return nil, fmt.Errorf("generate IV: %w", err)
	}

	gcmParams := pkcs11.NewGCMParams(iv, nil, gcmTagBits)
	defer gcmParams.Free()

	mechanism := []*pkcs11.Mechanism{
		pkcs11.NewMechanism(pkcs11.CKM_AES_GCM, gcmParams),
	}

	if err := p.ctx.EncryptInit(p.session, mechanism, p.kekHandle); err != nil {
		return nil, fmt.Errorf("encrypt init: %w", err)
	}

	ciphertext, err := p.ctx.Encrypt(p.session, plaintext)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}

	// Prepend IV so the caller can store a single blob.
	result := make([]byte, gcmIVSize+len(ciphertext))
	copy(result, iv)
	copy(result[gcmIVSize:], ciphertext)
	return result, nil
}

// Decrypt decrypts data produced by Encrypt (iv || ciphertext || tag).
func (p *Provider) Decrypt(data []byte) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(data) < gcmIVSize+1 {
		return nil, fmt.Errorf("ciphertext too short (%d bytes)", len(data))
	}

	iv := data[:gcmIVSize]
	ciphertext := data[gcmIVSize:]

	gcmParams := pkcs11.NewGCMParams(iv, nil, gcmTagBits)
	defer gcmParams.Free()

	mechanism := []*pkcs11.Mechanism{
		pkcs11.NewMechanism(pkcs11.CKM_AES_GCM, gcmParams),
	}

	if err := p.ctx.DecryptInit(p.session, mechanism, p.kekHandle); err != nil {
		return nil, fmt.Errorf("decrypt init: %w", err)
	}

	plaintext, err := p.ctx.Decrypt(p.session, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}

	return plaintext, nil
}

// KeyID returns the stable, non-secret identifier of the current KEK.
func (p *Provider) KeyID() string {
	return p.keyID
}

// Healthy returns true if the PKCS#11 session is still usable.
func (p *Provider) Healthy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.ctx.GetSessionInfo(p.session)
	return err == nil
}

// Close logs out, closes the session, and finalizes the PKCS#11 library.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx != nil {
		p.ctx.Logout(p.session)
		p.ctx.CloseSession(p.session)
		p.ctx.Finalize()
		p.ctx.Destroy()
		p.ctx = nil
	}
	return nil
}

// ---------------------------------------------------------------------------
// internal helpers
// ---------------------------------------------------------------------------

func findSlot(ctx *pkcs11.Ctx, tokenLabel string, slotID uint, useSlotID bool) (uint, error) {
	slots, err := ctx.GetSlotList(true)
	if err != nil {
		return 0, fmt.Errorf("get slot list: %w", err)
	}
	if len(slots) == 0 {
		return 0, fmt.Errorf("no PKCS#11 slots with tokens found")
	}

	if useSlotID {
		for _, s := range slots {
			if s == slotID {
				return s, nil
			}
		}
		return 0, fmt.Errorf("slot ID %d not found", slotID)
	}

	for _, s := range slots {
		info, err := ctx.GetTokenInfo(s)
		if err != nil {
			continue
		}
		if strings.TrimSpace(info.Label) == strings.TrimSpace(tokenLabel) {
			return s, nil
		}
	}
	return 0, fmt.Errorf("token with label %q not found", tokenLabel)
}

func findOrCreateKEK(ctx *pkcs11.Ctx, session pkcs11.SessionHandle, keyLabel string, autoCreate bool) (pkcs11.ObjectHandle, error) {
	template := []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_CLASS, pkcs11.CKO_SECRET_KEY),
		pkcs11.NewAttribute(pkcs11.CKA_KEY_TYPE, pkcs11.CKK_AES),
		pkcs11.NewAttribute(pkcs11.CKA_LABEL, keyLabel),
	}

	if err := ctx.FindObjectsInit(session, template); err != nil {
		return 0, fmt.Errorf("find objects init: %w", err)
	}
	handles, _, err := ctx.FindObjects(session, 1)
	if err != nil {
		ctx.FindObjectsFinal(session)
		return 0, fmt.Errorf("find objects: %w", err)
	}
	if err := ctx.FindObjectsFinal(session); err != nil {
		return 0, fmt.Errorf("find objects final: %w", err)
	}

	if len(handles) > 0 {
		return handles[0], nil
	}

	if !autoCreate {
		return 0, fmt.Errorf("KEK with label %q not found and auto-create is disabled", keyLabel)
	}

	return generateAES256Key(ctx, session, keyLabel)
}

func generateAES256Key(ctx *pkcs11.Ctx, session pkcs11.SessionHandle, label string) (pkcs11.ObjectHandle, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return 0, fmt.Errorf("generate key ID: %w", err)
	}

	template := []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_CLASS, pkcs11.CKO_SECRET_KEY),
		pkcs11.NewAttribute(pkcs11.CKA_KEY_TYPE, pkcs11.CKK_AES),
		pkcs11.NewAttribute(pkcs11.CKA_LABEL, label),
		pkcs11.NewAttribute(pkcs11.CKA_ID, id),
		pkcs11.NewAttribute(pkcs11.CKA_VALUE_LEN, aes256KeyBytes),
		pkcs11.NewAttribute(pkcs11.CKA_TOKEN, true),
		pkcs11.NewAttribute(pkcs11.CKA_PRIVATE, true),
		pkcs11.NewAttribute(pkcs11.CKA_SENSITIVE, true),
		pkcs11.NewAttribute(pkcs11.CKA_EXTRACTABLE, false),
		pkcs11.NewAttribute(pkcs11.CKA_ENCRYPT, true),
		pkcs11.NewAttribute(pkcs11.CKA_DECRYPT, true),
		pkcs11.NewAttribute(pkcs11.CKA_WRAP, true),
		pkcs11.NewAttribute(pkcs11.CKA_UNWRAP, true),
	}

	mechanism := []*pkcs11.Mechanism{
		pkcs11.NewMechanism(pkcs11.CKM_AES_KEY_GEN, nil),
	}

	handle, err := ctx.GenerateKey(session, mechanism, template)
	if err != nil {
		return 0, fmt.Errorf("generate AES-256 key: %w", err)
	}

	return handle, nil
}
