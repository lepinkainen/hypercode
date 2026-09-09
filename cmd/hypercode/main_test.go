package main

import "testing"

func TestListenerBoundary(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8090", "localhost:8090", "[::1]:8090", "100.101.102.103:8090", "[fd7a:115c:a1e0::1]:8090"} {
		if err := validateAddress(addr); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{":8090", "0.0.0.0:8090", "[::]:8090", "192.168.1.5:8090", "example.com:8090", "8.8.8.8:8090"} {
		if validateAddress(addr) == nil {
			t.Errorf("accepted %s", addr)
		}
	}
}
