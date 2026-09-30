package server

import "testing"

func TestUIBindRequiresExplicitOverride(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8081", "[::1]:8081", "localhost:8081"} {
		if err := ValidateUIBind(addr, false); err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{":8081", "0.0.0.0:8081", "192.0.2.1:8081"} {
		if err := ValidateUIBind(addr, false); err == nil {
			t.Fatalf("%s accepted", addr)
		}
		if err := ValidateUIBind(addr, true); err != nil {
			t.Fatalf("%s override: %v", addr, err)
		}
	}
}
