// Copyright (C) 2026 The uwuAOSP Project
// SPDX-License-Identifier: Apache-2.0

package uni

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const managedSigningKeysMarker = ".uni-managed-keys"

var releaseKeyNames = []string{
	"bluetooth", "cyngn-app", "media", "networkstack", "nfc", "platform",
	"releasekey", "sdk_sandbox", "shared", "testcert", "verity",
}

func createSigningKeyPair(directory, name string, bits int, apexPayload bool) error {
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return fmt.Errorf("generate %s private key: %w", name, err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate %s certificate serial: %w", name, err)
	}
	now := time.Now()
	certificate := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization:       []string{"uwuAOSP"},
			OrganizationalUnit: []string{name},
			CommonName:         "uwuAOSP release",
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(10000 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create %s certificate: %w", name, err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("encode %s private key: %w", name, err)
	}
	base := filepath.Join(directory, name)
	if err := os.WriteFile(base+".pk8", privateDER, 0600); err != nil {
		return err
	}
	if err := os.WriteFile(base+".x509.pem", pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: certificateDER,
	}), 0600); err != nil {
		return err
	}
	if apexPayload {
		if err := os.WriteFile(base+".pem", pem.EncodeToMemory(&pem.Block{
			Type: "PRIVATE KEY", Bytes: privateDER,
		}), 0600); err != nil {
			return err
		}
	}
	return nil
}

func initializeSigningKeys(ctx context.Context, top, destination string) (string, error) {
	keysDir, err := filepath.Abs(destination)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(keysDir))
	if err != nil {
		return "", fmt.Errorf("signing key parent directory must already exist: %w", err)
	}
	top, err = filepath.EvalSymlinks(top)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(top, parent)
	if err != nil {
		return "", err
	}
	if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("signing keys must be stored outside the Android source tree: %s", keysDir)
	}
	keysDir = filepath.Join(parent, filepath.Base(keysDir))
	if _, err := os.Lstat(keysDir); err == nil {
		return "", fmt.Errorf("signing key directory already exists; existing keys were not changed: %s", keysDir)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	staging, err := os.MkdirTemp(parent, ".uni-signing-keys-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	for _, name := range releaseKeyNames {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := createSigningKeyPair(staging, name, 2048, false); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(staging, managedSigningKeysMarker), []byte("v1\n"), 0600); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.Mkdir(keysDir, 0700); err != nil {
		return "", fmt.Errorf("signing key directory already exists or cannot be reserved: %w", err)
	}
	installed := make([]string, 0, len(releaseKeyNames)*2+2)
	cleanIncomplete := func() {
		for _, path := range installed {
			_ = os.Remove(path)
		}
		_ = os.Remove(keysDir)
	}
	for _, name := range releaseKeyNames {
		for _, suffix := range []string{".pk8", ".x509.pem"} {
			file := name + suffix
			path := filepath.Join(keysDir, file)
			if err := os.Link(filepath.Join(staging, file), path); err != nil {
				cleanIncomplete()
				return "", fmt.Errorf("install signing key %s: %w", file, err)
			}
			installed = append(installed, path)
		}
	}
	marker := filepath.Join(keysDir, managedSigningKeysMarker)
	if err := os.Link(filepath.Join(staging, managedSigningKeysMarker), marker); err != nil {
		cleanIncomplete()
		return "", err
	}
	installed = append(installed, marker)
	for _, suffix := range []string{".pk8", ".x509.pem"} {
		path := filepath.Join(keysDir, "testkey"+suffix)
		if err := os.Symlink("releasekey"+suffix, path); err != nil {
			cleanIncomplete()
			return "", err
		}
		installed = append(installed, path)
	}
	return keysDir, nil
}
