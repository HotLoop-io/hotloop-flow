package nodes

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// nrMongoTLS builds the TLS settings from the config node's files. The files
// are read from the paths the flow names, as the community node does.
func nrMongoTLS(c *nrMongoClient) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.tlsInsecure} //nolint:gosec // tlsInsecure is the flow author's switch
	if c.tlsCAFile != "" {
		pem, err := os.ReadFile(c.tlsCAFile)
		if err != nil {
			return nil, fmt.Errorf("reading tlsCAFile: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tlsCAFile %s holds no certificate", c.tlsCAFile)
		}
		cfg.RootCAs = pool
	}
	if c.tlsKeyFile != "" {
		pem, err := os.ReadFile(c.tlsKeyFile)
		if err != nil {
			return nil, fmt.Errorf("reading tlsCertificateKeyFile: %w", err)
		}
		cert, err := tls.X509KeyPair(pem, pem)
		if err != nil {
			return nil, fmt.Errorf("tlsCertificateKeyFile has to hold the certificate and its key: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}
