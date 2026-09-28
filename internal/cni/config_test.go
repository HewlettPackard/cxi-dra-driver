package cni

import (
	"testing"
	"time"
)

func TestParseNetConf_Defaults(t *testing.T) {
	conf, err := ParseNetConf([]byte(`{"cniVersion":"1.0.0","name":"slingshot-hsn","type":"slingshot-cni"}`))
	if err != nil {
		t.Fatalf("ParseNetConf: %v", err)
	}
	if got := conf.hintsDirOrDefault(); got != DefaultHintsDir {
		t.Errorf("hintsDirOrDefault() = %q, want %q", got, DefaultHintsDir)
	}
	if conf.retryInterval != defaultRetryInterval {
		t.Errorf("retryInterval = %v, want %v", conf.retryInterval, defaultRetryInterval)
	}
	if conf.retryTimeout != defaultRetryTimeout {
		t.Errorf("retryTimeout = %v, want %v", conf.retryTimeout, defaultRetryTimeout)
	}
}

func TestParseNetConf_Overrides(t *testing.T) {
	conf, err := ParseNetConf([]byte(`{"cniVersion":"1.0.0","hintsDir":"/tmp/hints","retryInterval":"20ms","retryTimeout":"1s"}`))
	if err != nil {
		t.Fatalf("ParseNetConf: %v", err)
	}
	if got := conf.hintsDirOrDefault(); got != "/tmp/hints" {
		t.Errorf("hintsDirOrDefault() = %q, want /tmp/hints", got)
	}
	if conf.retryInterval != 20*time.Millisecond {
		t.Errorf("retryInterval = %v, want 20ms", conf.retryInterval)
	}
	if conf.retryTimeout != time.Second {
		t.Errorf("retryTimeout = %v, want 1s", conf.retryTimeout)
	}
}

func TestParseNetConf_InvalidRetryInterval(t *testing.T) {
	if _, err := ParseNetConf([]byte(`{"retryInterval":"not-a-duration"}`)); err == nil {
		t.Fatal("expected error for invalid retryInterval")
	}
}

func TestParseNetConf_InvalidRetryTimeout(t *testing.T) {
	if _, err := ParseNetConf([]byte(`{"retryTimeout":"not-a-duration"}`)); err == nil {
		t.Fatal("expected error for invalid retryTimeout")
	}
}

func TestParseNetConf_InvalidJSON(t *testing.T) {
	if _, err := ParseNetConf([]byte(`not json`)); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}
