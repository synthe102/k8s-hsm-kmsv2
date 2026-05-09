// Package kms implements the Kubernetes KMS v2 Service interface,
// delegating encrypt/decrypt operations to the HSM provider.
package kms

import (
	"context"
	"fmt"
	"log"

	"eviden/k8s-hsm-kmsv2/pkg/hsm"

	kmsservice "k8s.io/kms/pkg/service"
)

const kmsAPIVersion = "v2"

// Service implements kmsservice.Service backed by a PKCS#11 HSM.
type Service struct {
	provider *hsm.Provider
}

// Compile-time check.
var _ kmsservice.Service = (*Service)(nil)

// New creates a KMS v2 service backed by the given HSM provider.
func New(provider *hsm.Provider) *Service {
	return &Service{provider: provider}
}

// Status reports the plugin health and current key ID.
func (s *Service) Status(_ context.Context) (*kmsservice.StatusResponse, error) {
	healthz := "ok"
	if !s.provider.Healthy() {
		healthz = "unhealthy: HSM session error"
	}
	return &kmsservice.StatusResponse{
		Version: kmsAPIVersion,
		Healthz: healthz,
		KeyID:   s.provider.KeyID(),
	}, nil
}

// Encrypt wraps the DEK seed with the HSM KEK.
func (s *Service) Encrypt(_ context.Context, uid string, plaintext []byte) (*kmsservice.EncryptResponse, error) {
	if len(plaintext) == 0 {
		return nil, fmt.Errorf("plaintext is empty")
	}

	ciphertext, err := s.provider.Encrypt(plaintext)
	if err != nil {
		log.Printf("encrypt error uid=%s: %v", uid, err)
		return nil, fmt.Errorf("encryption failed: %w", err)
	}

	return &kmsservice.EncryptResponse{
		Ciphertext:  ciphertext,
		KeyID:       s.provider.KeyID(),
		Annotations: nil,
	}, nil
}

// Decrypt unwraps the DEK seed with the HSM KEK.
func (s *Service) Decrypt(_ context.Context, uid string, req *kmsservice.DecryptRequest) ([]byte, error) {
	if len(req.Ciphertext) == 0 {
		return nil, fmt.Errorf("ciphertext is empty")
	}

	if req.KeyID != s.provider.KeyID() {
		return nil, fmt.Errorf("unknown key_id %q (current: %q)", req.KeyID, s.provider.KeyID())
	}

	plaintext, err := s.provider.Decrypt(req.Ciphertext)
	if err != nil {
		log.Printf("decrypt error uid=%s: %v", uid, err)
		return nil, fmt.Errorf("decryption failed: %w", err)
	}

	return plaintext, nil
}
