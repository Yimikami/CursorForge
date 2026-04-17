package certs

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

type CA struct {
	Cert    *x509.Certificate
	Key     *rsa.PrivateKey
	CertDER []byte
	certPEM []byte
	keyPEM  []byte
	dir     string
}

func LoadOrCreate(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	cb, certErr := os.ReadFile(certPath)
	kb, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		if ca, err := parsePEM(cb, kb); err == nil {
			ca.dir = dir
			return ca, nil
		}
	}

	ca, err := generate()
	if err != nil {
		return nil, err
	}
	ca.dir = dir
	if err := os.WriteFile(certPath, ca.certPEM, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, ca.keyPEM, 0o600); err != nil {
		return nil, err
	}
	return ca, nil
}

func generate() (*CA, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "CursorForge Local CA",
			Organization: []string{"CursorForge"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return &CA{Cert: cert, Key: key, CertDER: der, certPEM: certPEM, keyPEM: keyPEM}, nil
}

func parsePEM(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return nil, errors.New("invalid certificate PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, errors.New("invalid key PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, CertDER: cb.Bytes, certPEM: certPEM, keyPEM: keyPEM}, nil
}

func (c *CA) CertPEM() []byte { return c.certPEM }
func (c *CA) KeyPEM() []byte  { return c.keyPEM }
func (c *CA) Dir() string     { return c.dir }

func (c *CA) Fingerprint() string {
	sum := sha256.Sum256(c.CertDER)
	return hex.EncodeToString(sum[:])
}
