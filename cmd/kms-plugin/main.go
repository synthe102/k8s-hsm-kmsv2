package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"eviden/k8s-hsm-kmsv2/pkg/hsm"
	kmsPlugin "eviden/k8s-hsm-kmsv2/pkg/kms"

	kmsservice "k8s.io/kms/pkg/service"
)

func main() {
	// PKCS#11 flags
	pkcs11Lib := flag.String("pkcs11-lib", envOrDefault("PKCS11_LIB", ""), "Path to PKCS#11 shared library")
	tokenLabel := flag.String("token-label", envOrDefault("TOKEN_LABEL", ""), "PKCS#11 token label")
	pin := flag.String("pin", envOrDefault("PKCS11_PIN", ""), "PKCS#11 user PIN")
	slotID := flag.Uint("slot-id", 0, "PKCS#11 slot ID (requires --use-slot-id)")
	useSlotID := flag.Bool("use-slot-id", false, "Select slot by ID instead of token label")

	// Key flags
	keyLabel := flag.String("key-label", envOrDefault("KEY_LABEL", "k8s-kms-kek"), "Label of the AES-256 KEK in the HSM")
	autoCreate := flag.Bool("auto-create-key", true, "Create the KEK if it does not exist")

	// Server flags
	socketPath := flag.String("socket", envOrDefault("SOCKET_PATH", "/tmp/kms-plugin.sock"), "UNIX socket path for the gRPC server")
	timeout := flag.Duration("timeout", 5*time.Second, "gRPC connection timeout")

	flag.Parse()

	if err := validateFlags(*pkcs11Lib, *tokenLabel, *pin, *useSlotID); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		flag.Usage()
		os.Exit(1)
	}

	// Remove stale socket file.
	_ = os.Remove(*socketPath)

	log.Printf("initializing HSM provider (lib=%s token=%q)", *pkcs11Lib, *tokenLabel)

	provider, err := hsm.NewProvider(hsm.Config{
		LibPath:       *pkcs11Lib,
		TokenLabel:    *tokenLabel,
		Pin:           *pin,
		KeyLabel:      *keyLabel,
		SlotID:        *slotID,
		UseSlotID:     *useSlotID,
		AutoCreateKey: *autoCreate,
	})
	if err != nil {
		log.Fatalf("HSM init failed: %v", err)
	}
	defer provider.Close()

	log.Printf("HSM ready  key_id=%s", provider.KeyID())

	svc := kmsPlugin.New(provider)
	grpcSvc := kmsservice.NewGRPCService(*socketPath, *timeout, svc)

	// Graceful shutdown on SIGINT / SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("received %v – shutting down", sig)
		grpcSvc.Shutdown()
	}()

	log.Printf("listening on unix://%s", *socketPath)
	if err := grpcSvc.ListenAndServe(); err != nil {
		log.Fatalf("gRPC server: %v", err)
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func validateFlags(lib, token, pin string, useSlotID bool) error {
	if lib == "" {
		return fmt.Errorf("--pkcs11-lib (or PKCS11_LIB) is required")
	}
	if token == "" && !useSlotID {
		return fmt.Errorf("--token-label (or TOKEN_LABEL) is required unless --use-slot-id is set")
	}
	if pin == "" {
		return fmt.Errorf("--pin (or PKCS11_PIN) is required")
	}
	return nil
}
