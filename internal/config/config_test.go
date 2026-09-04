package config

import "testing"

func TestValidateTransports(t *testing.T) {
	c := loadDefaultConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}
	c.Transports[0].Protocol = "udp"
	if err := c.Validate(); err == nil {
		t.Fatal("UDP transport must be rejected")
	}
}

func TestValidateRejectsTLS(t *testing.T) {
	c := loadDefaultConfig()
	c.TLS.Enabled = true
	if err := c.Validate(); err == nil {
		t.Fatal("TLS must be rejected until the listener supports it")
	}
}
