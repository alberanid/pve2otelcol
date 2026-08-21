package ologgers

import (
	"context"
	"math"
	"testing"
	"time"

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
