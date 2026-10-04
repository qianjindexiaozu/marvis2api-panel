package upstream

import (
	"strings"
	"testing"
)

func TestMarvisExtUsesDeviceQIMEI(t *testing.T) {
	c := New()
	c.DeviceQIMEI = "0123456789abcdef0123456789abcdef0123"
	raw := c.marvisExtHeader("dce13552-8bd3-4759-b6e1-22a2c9815c38", "openid")
	if !strings.Contains(raw, `"qimei36":"0123456789abcdef0123456789abcdef0123"`) {
		t.Fatal(raw)
	}
	if !strings.Contains(raw, `"guid":"dce13552-8bd3-4759-b6e1-22a2c9815c38"`) {
		t.Fatal(raw)
	}
}
