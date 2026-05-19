package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"eviden/k8s-hsm-kmsv2/pkg/hsm"
	kmsPlugin "eviden/k8s-hsm-kmsv2/pkg/kms"

	kmsapi "k8s.io/kms/apis/v2"
	kmsservice "k8s.io/kms/pkg/service"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// findSoftHSM2Lib returns the path to libsofthsm2 or "" if not found.
// The SOFTHSM2_LIB environment variable takes precedence over the default
// candidate list, which allows CI environments (e.g. Nix) to override the path.
func findSoftHSM2Lib() string {
	if p := os.Getenv("SOFTHSM2_LIB"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	candidates := []string{
		// macOS Homebrew (Apple Silicon / Intel)
		"/opt/homebrew/lib/softhsm/libsofthsm2.so",
		"/usr/local/lib/softhsm/libsofthsm2.so",
		// Linux
		"/usr/lib/softhsm/libsofthsm2.so",
		"/usr/lib/x86_64-linux-gnu/softhsm/libsofthsm2.so",
		"/usr/lib/aarch64-linux-gnu/softhsm/libsofthsm2.so",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// initToken creates a temporary SoftHSM2 token directory and initialises a
// token with the given label and PIN.
func initToken(t *testing.T, label, pin string) {
	t.Helper()

	tmpDir := t.TempDir()
	tokenDir := filepath.Join(tmpDir, "tokens")
	if err := os.MkdirAll(tokenDir, 0o755); err != nil {
		t.Fatal(err)
	}

	confPath := filepath.Join(tmpDir, "softhsm2.conf")
	conf := fmt.Sprintf("directories.tokendir = %s\nobjectstore.backend = file\n", tokenDir)
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOFTHSM2_CONF", confPath)

	cmd := exec.Command("softhsm2-util",
		"--init-token", "--slot", "0",
		"--label", label,
		"--so-pin", "12345678",
		"--pin", pin,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("softhsm2-util --init-token: %v\n%s", err, out)
	}
}

func newProvider(t *testing.T) *hsm.Provider {
	t.Helper()

	lib := findSoftHSM2Lib()
	if lib == "" {
		t.Skip("SoftHSM2 not found – skipping integration test")
	}
	if _, err := exec.LookPath("softhsm2-util"); err != nil {
		t.Skip("softhsm2-util not in PATH – skipping")
	}
	t.Logf("Using PKCS#11 library: %s", lib)

	const label = "test-token"
	const pin = "1234"
	initToken(t, label, pin)

	p, err := hsm.NewProvider(hsm.Config{
		LibPath:       lib,
		TokenLabel:    label,
		Pin:           pin,
		KeyLabel:      "test-kek",
		AutoCreateKey: true,
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// -----------------------------------------------------------------------
// Unit-level tests (direct provider + service)
// -----------------------------------------------------------------------

func TestProviderEncryptDecrypt(t *testing.T) {
	p := newProvider(t)

	plain := []byte("hello from the KMS v2 integration test")
	ct, err := p.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if len(ct) == 0 {
		t.Fatal("ciphertext is empty")
	}

	got, err := p.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != string(plain) {
		t.Fatalf("Decrypt mismatch: got %q, want %q", got, plain)
	}
}

func TestProviderDecryptWrongData(t *testing.T) {
	p := newProvider(t)

	_, err := p.Decrypt([]byte("too-short"))
	if err == nil {
		t.Fatal("expected error on short ciphertext")
	}
}

func TestServiceStatus(t *testing.T) {
	p := newProvider(t)
	svc := kmsPlugin.New(p)

	resp, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if resp.Version != "v2" {
		t.Errorf("Version = %q, want v2", resp.Version)
	}
	if resp.Healthz != "ok" {
		t.Errorf("Healthz = %q, want ok", resp.Healthz)
	}
	if resp.KeyID == "" {
		t.Error("KeyID is empty")
	}
}

func TestServiceEncryptDecrypt(t *testing.T) {
	p := newProvider(t)
	svc := kmsPlugin.New(p)
	ctx := context.Background()

	plain := []byte("kubernetes DEK seed material for testing")

	encResp, err := svc.Encrypt(ctx, "uid-001", plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	decrypted, err := svc.Decrypt(ctx, "uid-002", &kmsservice.DecryptRequest{
		Ciphertext:  encResp.Ciphertext,
		KeyID:       encResp.KeyID,
		Annotations: encResp.Annotations,
	})
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(decrypted) != string(plain) {
		t.Fatalf("round-trip mismatch: got %q", decrypted)
	}
}

func TestServiceDecryptWrongKeyID(t *testing.T) {
	p := newProvider(t)
	svc := kmsPlugin.New(p)

	_, err := svc.Decrypt(context.Background(), "uid-x", &kmsservice.DecryptRequest{
		Ciphertext: []byte("aaaaaaaaaaaa" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		KeyID:      "wrong-key-id",
	})
	if err == nil {
		t.Fatal("expected error for wrong key_id")
	}
}

// -----------------------------------------------------------------------
// gRPC integration test
// -----------------------------------------------------------------------

func TestGRPCRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("UNIX socket test not applicable on Windows")
	}

	p := newProvider(t)
	svc := kmsPlugin.New(p)

	socketPath := filepath.Join(t.TempDir(), "kms.sock")
	grpcSvc := kmsservice.NewGRPCService(socketPath, 5*time.Second, svc)

	go func() { _ = grpcSvc.ListenAndServe() }()
	t.Cleanup(func() { grpcSvc.Shutdown() })

	// Give the listener time to bind.
	time.Sleep(200 * time.Millisecond)

	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc dial: %v", err)
	}
	defer conn.Close()

	client := kmsapi.NewKeyManagementServiceClient(conn)
	ctx := context.Background()

	// Status
	st, err := client.Status(ctx, &kmsapi.StatusRequest{})
	if err != nil {
		t.Fatalf("gRPC Status: %v", err)
	}
	if st.Version != "v2" {
		t.Errorf("gRPC Version = %q", st.Version)
	}

	// Encrypt
	plain := []byte("grpc integration test data")
	enc, err := client.Encrypt(ctx, &kmsapi.EncryptRequest{
		Plaintext: plain,
		Uid:       "grpc-uid-1",
	})
	if err != nil {
		t.Fatalf("gRPC Encrypt: %v", err)
	}

	// Decrypt
	dec, err := client.Decrypt(ctx, &kmsapi.DecryptRequest{
		Ciphertext:  enc.Ciphertext,
		Uid:         "grpc-uid-2",
		KeyId:       enc.KeyId,
		Annotations: enc.Annotations,
	})
	if err != nil {
		t.Fatalf("gRPC Decrypt: %v", err)
	}
	if string(dec.Plaintext) != string(plain) {
		t.Fatalf("gRPC round-trip mismatch: %q", dec.Plaintext)
	}
}
