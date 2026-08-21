package ologgers

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alberanid/pve2otelcol/config"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/embedded"
)

type recordingLogger struct {
	embedded.Logger
	records []otellog.Record
}

func (l *recordingLogger) Emit(_ context.Context, record otellog.Record) {
	l.records = append(l.records, record)
}

func (l *recordingLogger) Enabled(context.Context, otellog.EnabledParameters) bool {
	return true
}

func TestStr2time(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    time.Time
		wantErr bool
	}{
		{
			name:  "epoch",
			input: "0",
			want:  time.Unix(0, 0),
		},
		{
			name:  "microsecond before second boundary",
			input: "999999",
			want:  time.Unix(0, 999999000),
		},
		{
			name:  "exact second boundary",
			input: "1000000",
			want:  time.Unix(1, 0),
		},
		{
			name:  "typical journal timestamp",
			input: "1700000000123456",
			want:  time.Unix(1700000000, 123456000),
		},
		{
			name:  "one microsecond before epoch",
			input: "-1",
			want:  time.Unix(-1, 999999000),
		},
		{
			name:  "pre-epoch second and microsecond",
			input: "-1000001",
			want:  time.Unix(-2, 999999000),
		},
		{
			name:  "maximum int64 microseconds",
			input: "9223372036854775807",
			want:  time.UnixMicro(math.MaxInt64),
		},
		{
			name:  "minimum int64 microseconds",
			input: "-9223372036854775808",
			want:  time.UnixMicro(math.MinInt64),
		},
		{
			name:    "empty",
			input:   "",
			wantErr: true,
		},
		{
			name:    "malformed",
			input:   "not-a-timestamp",
			wantErr: true,
		},
		{
			name:    "positive overflow",
			input:   "9223372036854775808",
			wantErr: true,
		},
		{
			name:    "negative overflow",
			input:   "-9223372036854775809",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := str2time(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("str2time(%q) error = nil, want error", tt.input)
				}
				if !got.IsZero() {
					t.Fatalf("str2time(%q) time = %v, want zero time", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("str2time(%q) error = %v", tt.input, err)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("str2time(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestLogSetsOnlySuccessfullyParsedTimestamps(t *testing.T) {
	tests := []struct {
		name         string
		source       interface{}
		observed     interface{}
		wantSource   time.Time
		wantObserved time.Time
	}{
		{
			name:         "valid timestamps",
			source:       "1700000000123456",
			observed:     "1700000000654321",
			wantSource:   time.Unix(1700000000, 123456000),
			wantObserved: time.Unix(1700000000, 654321000),
		},
		{
			name:     "invalid timestamps remain unset",
			source:   "invalid",
			observed: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &recordingLogger{}
			logger := &OLogger{Logger: recorder, Ctx: context.Background()}
			logger.Log(map[string]interface{}{
				"_SOURCE_REALTIME_TIMESTAMP": tt.source,
				"__REALTIME_TIMESTAMP":       tt.observed,
			})

			if len(recorder.records) != 1 {
				t.Fatalf("emitted records = %d, want 1", len(recorder.records))
			}
			record := recorder.records[0]
			if got := record.Timestamp(); !got.Equal(tt.wantSource) {
				t.Errorf("record timestamp = %v, want %v", got, tt.wantSource)
			}
			if got := record.ObservedTimestamp(); !got.Equal(tt.wantObserved) {
				t.Errorf("record observed timestamp = %v, want %v", got, tt.wantObserved)
			}
		})
	}
}

func TestNewTLSConfigWithoutTLSFilesUsesExporterDefaults(t *testing.T) {
	tlsConfig, err := newTLSConfig(&config.Config{})
	if err != nil {
		t.Fatalf("newTLSConfig() error = %v, want nil", err)
	}
	if tlsConfig != nil {
		t.Fatalf("newTLSConfig() = %#v, want nil", tlsConfig)
	}
}

func TestNewTLSConfigAppendsCAtoSystemRoots(t *testing.T) {
	additionalPEM, additionalCert, _ := testCertificate(t, true)
	systemPEM, systemCert, _ := testCertificate(t, true)
	caPath := writeTLSFile(t, "additional-ca.pem", additionalPEM)

	systemRoots := x509.NewCertPool()
	if !systemRoots.AppendCertsFromPEM(systemPEM) {
		t.Fatal("failed to prepare system root pool")
	}
	tlsConfig, err := newTLSConfigWithSystemRoots(&config.Config{OtlpTLSCAFile: caPath}, func() (*x509.CertPool, error) {
		return systemRoots, nil
	})
	if err != nil {
		t.Fatalf("newTLSConfigWithSystemRoots() error = %v, want nil", err)
	}
	if tlsConfig.RootCAs == nil {
		t.Fatal("TLS RootCAs = nil, want system and additional roots")
	}
	assertPoolContainsSubject(t, tlsConfig.RootCAs, systemCert.RawSubject)
	assertPoolContainsSubject(t, tlsConfig.RootCAs, additionalCert.RawSubject)
}

func TestNewTLSConfigUsesClientCertificateOnlyAsIdentity(t *testing.T) {
	certPEM, _, keyPEM := testCertificate(t, false)
	certPath := writeTLSFile(t, "client.pem", certPEM)
	keyPath := writeTLSFile(t, "client-key.pem", keyPEM)

	tlsConfig, err := newTLSConfigWithSystemRoots(&config.Config{
		OtlpTLSCertFile: certPath,
		OtlpTLSKeyFile:  keyPath,
	}, func() (*x509.CertPool, error) {
		t.Fatal("system roots should not be materialized when no additional CA is configured")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("newTLSConfigWithSystemRoots() error = %v, want nil", err)
	}
	if len(tlsConfig.Certificates) != 1 {
		t.Fatalf("TLS client certificates = %d, want 1", len(tlsConfig.Certificates))
	}
	if tlsConfig.RootCAs != nil {
		t.Fatal("TLS RootCAs is non-nil, want the standard system-root behavior")
	}
}

func TestNewTLSConfigDoesNotExposeCredentialPaths(t *testing.T) {
	secretDir := filepath.Join(t.TempDir(), "private-collector-credentials")
	tests := []struct {
		name string
		cfg  config.Config
	}{
		{
			name: "missing CA",
			cfg:  config.Config{OtlpTLSCAFile: filepath.Join(secretDir, "internal-ca.pem")},
		},
		{
			name: "missing client certificate",
			cfg: config.Config{
				OtlpTLSCertFile: filepath.Join(secretDir, "client.pem"),
				OtlpTLSKeyFile:  filepath.Join(secretDir, "client-key.pem"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newTLSConfigWithSystemRoots(&tt.cfg, func() (*x509.CertPool, error) {
				return x509.NewCertPool(), nil
			})
			if err == nil {
				t.Fatal("newTLSConfigWithSystemRoots() error = nil, want non-nil")
			}
			if strings.Contains(err.Error(), secretDir) {
				t.Fatalf("error exposes credential directory: %q", err)
			}
		})
	}
}

func TestNewTLSConfigReportsSystemRootFailure(t *testing.T) {
	wantErr := errors.New("system root failure")
	_, err := newTLSConfigWithSystemRoots(&config.Config{OtlpTLSCAFile: "unused"}, func() (*x509.CertPool, error) {
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("newTLSConfigWithSystemRoots() error = %v, want wrapped %v", err, wantErr)
	}
}

func testCertificate(t *testing.T, isCA bool) ([]byte, *x509.Certificate, []byte) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: t.Name()},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  isCA,
		KeyUsage:              x509.KeyUsageDigitalSignature,
	}
	if isCA {
		template.KeyUsage |= x509.KeyUsageCertSign
	} else {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatalf("create test certificate: %v", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse test certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal test key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), certificate,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func writeTLSFile(t *testing.T, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write TLS fixture: %v", err)
	}
	return path
}

func assertPoolContainsSubject(t *testing.T, pool *x509.CertPool, want []byte) {
	t.Helper()
	for _, subject := range pool.Subjects() {
		if bytes.Equal(subject, want) {
			return
		}
	}
	t.Fatalf("certificate pool does not contain subject %x", want)
}
