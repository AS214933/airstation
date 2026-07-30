package config

import (
	"testing"
	"time"
)

func TestLoadUsesDefaultTmpRetention(t *testing.T) {
	t.Setenv("AIRSTATION_SECRET_KEY", "1234567890")
	t.Setenv("AIRSTATION_JWT_SIGN", "1234567890")
	t.Setenv("AIRSTATION_TMP_RETENTION", "")

	conf := Load()
	if conf.TmpRetention != 24*time.Hour {
		t.Fatalf("tmp retention = %s, want 24h", conf.TmpRetention)
	}
}

func TestLoadUsesConfiguredTmpRetention(t *testing.T) {
	t.Setenv("AIRSTATION_SECRET_KEY", "1234567890")
	t.Setenv("AIRSTATION_JWT_SIGN", "1234567890")
	t.Setenv("AIRSTATION_TMP_RETENTION", "12h")

	conf := Load()
	if conf.TmpRetention != 12*time.Hour {
		t.Fatalf("tmp retention = %s, want 12h", conf.TmpRetention)
	}
}
