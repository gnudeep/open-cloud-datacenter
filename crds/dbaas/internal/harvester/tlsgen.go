/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package harvester

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"time"
)

// TLSBundle holds the four PEM-encoded values generated for one DBInstance.
// CACertPEM and CAKeyPEM are stored in the K8s Secret so the CA can be
// referenced for future cert rotation. ServerCertPEM and ServerKeyPEM are
// written into the VM via cloud-init.
type TLSBundle struct {
	CACertPEM     string
	CAKeyPEM      string
	ServerCertPEM string
	ServerKeyPEM  string
}

// generateTLS creates an ephemeral self-signed CA and a server certificate
// signed by that CA. commonName is used as the Subject CN for both certs
// (typically the VM name). All certificates are valid for 10 years; rotation
// is out of scope for Phase 1.
func generateTLS(commonName string) (*TLSBundle, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "dbaas-ca-" + commonName},
		NotBefore:             time.Now().Add(-time.Minute), // clock-skew buffer
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, err
	}

	serverCertPEM, serverKeyPEM, err := issueServerCert(caCert, caKey, commonName, nil)
	if err != nil {
		return nil, err
	}

	return &TLSBundle{
		CACertPEM:     tlsPemEncode("CERTIFICATE", caCertDER),
		CAKeyPEM:      tlsPemEncodeKey(caKey),
		ServerCertPEM: serverCertPEM,
		ServerKeyPEM:  serverKeyPEM,
	}, nil
}

// RenewServerCert re-issues the server certificate using the existing CA,
// adding the supplied IPs as Subject Alternative Names. Call this once the
// VM's vpc-net IP is known and push the result back into the VM.
func RenewServerCert(caCertPEM, caKeyPEM, commonName string, ips []net.IP) (serverCertPEM, serverKeyPEM string, err error) {
	caKeyBlock, _ := pem.Decode([]byte(caKeyPEM))
	caKey, err := x509.ParsePKCS1PrivateKey(caKeyBlock.Bytes)
	if err != nil {
		return
	}
	caCertBlock, _ := pem.Decode([]byte(caCertPEM))
	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		return
	}
	return issueServerCert(caCert, caKey, commonName, ips)
}

// issueServerCert creates a server certificate signed by caCert/caKey.
// ips may be nil for the initial issuance (before the VM IP is known).
func issueServerCert(caCert *x509.Certificate, caKey *rsa.PrivateKey, commonName string, ips []net.IP) (certPEM, keyPEM string, err error) {
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return
	}

	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// DNS SAN lets clients use sslmode=verify-full by VM hostname.
		DNSNames:    []string{commonName},
		IPAddresses: ips,
	}
	serverCertDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return
	}
	certPEM = tlsPemEncode("CERTIFICATE", serverCertDER)
	keyPEM = tlsPemEncodeKey(serverKey)
	return
}

func tlsPemEncode(blockType string, data []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: data}))
}

func tlsPemEncodeKey(key *rsa.PrivateKey) string {
	return tlsPemEncode("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key))
}
