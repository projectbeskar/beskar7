/*
Copyright 2024 The Beskar7 Authors.

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

// mock-redfish is a standalone Redfish BMC emulator for in-cluster smoke
// testing of the Beskar7 operator. It serves the same multi-vendor handler
// as the unit-test fake (internal/redfishmock) on a real net.Listener.
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/projectbeskar/beskar7/internal/redfishmock"
)

// knownVendors maps lower-case CLI input to the canonical VendorType constant.
var knownVendors = map[string]redfishmock.VendorType{
	"dell":       redfishmock.VendorDell,
	"hpe":        redfishmock.VendorHPE,
	"lenovo":     redfishmock.VendorLenovo,
	"supermicro": redfishmock.VendorSupermicro,
	"generic":    redfishmock.VendorGeneric,
}

func main() {
	var (
		listenAddr  string
		vendorFlag  string
		useTLS      bool
		tlsCert     string
		tlsKey      string
		username    string
		password    string
		disableAuth bool
	)

	// --cert-san may be repeated; collect into a slice via a custom flag.Value.
	var certSANs sanList

	flag.StringVar(&listenAddr, "listen-addr", ":8443", "Address to listen on (host:port).")
	flag.StringVar(&vendorFlag, "vendor", "Generic",
		"Vendor to emulate. One of: Dell, HPE, Lenovo, Supermicro, Generic (case-insensitive).")
	flag.BoolVar(&useTLS, "tls", true,
		"Serve over TLS. When true and --tls-cert/--tls-key are empty, a self-signed cert is generated in memory.")
	flag.StringVar(&tlsCert, "tls-cert", "", "Path to TLS certificate file (PEM). Used only when --tls=true.")
	flag.StringVar(&tlsKey, "tls-key", "", "Path to TLS private-key file (PEM). Used only when --tls=true.")
	flag.StringVar(&username, "username", "admin", "Basic-auth username accepted by the mock server.")
	flag.StringVar(&password, "password", "password123", "Basic-auth password accepted by the mock server.")
	flag.BoolVar(&disableAuth, "disable-auth", false, "Disable HTTP Basic Auth enforcement.")
	flag.Var(&certSANs, "cert-san",
		"Additional SAN (DNS name or IP) for the self-signed cert. May be repeated.")
	flag.Parse()

	// Resolve vendor.
	vendor, ok := knownVendors[strings.ToLower(vendorFlag)]
	if !ok {
		log.Printf("unknown vendor %q; valid choices: Dell, HPE, Lenovo, Supermicro, Generic", vendorFlag)
		os.Exit(1)
	}

	// Build the mock handler.
	srv := redfishmock.NewMockRedfishServer(vendor)
	srv.SetCredentials(username, password)
	if disableAuth {
		srv.DisableAuth()
	}

	httpSrv := &http.Server{
		Addr:         listenAddr,
		Handler:      srv.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	// Resolve listener.
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Printf("failed to listen on %s: %v", listenAddr, err)
		os.Exit(1)
	}

	if useTLS {
		tlsConfig, err := buildTLSConfig(tlsCert, tlsKey, certSANs)
		if err != nil {
			log.Printf("failed to build TLS config: %v", err)
			os.Exit(1)
		}
		ln = tls.NewListener(ln, tlsConfig)
	}

	log.Printf("mock-redfish listening on %s vendor=%s tls=%t", listenAddr, vendorFlag, useTLS)

	// Start server in background.
	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// Wait for a signal or a fatal server error.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		log.Printf("received signal %s, shutting down", sig)
	case err := <-errCh:
		log.Printf("server error: %v", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown error: %v", err)
		os.Exit(1)
	}
}

// buildTLSConfig returns a *tls.Config. When certFile and keyFile are both
// empty a self-signed RSA-2048 certificate is generated in memory; otherwise
// the provided files are loaded. The certificate covers "localhost",
// "127.0.0.1", and any entries in extraSANs.
func buildTLSConfig(certFile, keyFile string, extraSANs []string) (*tls.Config, error) {
	var cert tls.Certificate
	var err error

	if certFile != "" || keyFile != "" {
		if certFile == "" || keyFile == "" {
			return nil, fmt.Errorf("--tls-cert and --tls-key must both be set or both be empty")
		}
		cert, err = tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("loading TLS key pair: %w", err)
		}
	} else {
		cert, err = generateSelfSigned(extraSANs)
		if err != nil {
			return nil, fmt.Errorf("generating self-signed cert: %w", err)
		}
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// generateSelfSigned creates an in-memory self-signed RSA-2048 certificate
// covering localhost, 127.0.0.1, and any extra SANs provided.
func generateSelfSigned(extraSANs []string) (tls.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generating RSA key: %w", err)
	}

	dnsNames := []string{"localhost"}
	ipAddrs := []net.IP{net.ParseIP("127.0.0.1")}

	for _, san := range extraSANs {
		if ip := net.ParseIP(san); ip != nil {
			ipAddrs = append(ipAddrs, ip)
		} else {
			dnsNames = append(dnsNames, san)
		}
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generating serial: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "mock-redfish"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * 365 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ipAddrs,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("creating certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	return tls.X509KeyPair(certPEM, keyPEM)
}

// sanList implements flag.Value for a repeatable --cert-san flag.
type sanList []string

func (s *sanList) String() string {
	return strings.Join(*s, ",")
}

func (s *sanList) Set(v string) error {
	*s = append(*s, v)
	return nil
}
