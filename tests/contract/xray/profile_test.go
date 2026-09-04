package xray_test

import "testing"

func TestKnownSS2022ConfigurationFailures(t *testing.T) {
	bin := contractBinary(t)
	apiAddress, inboundAddress := freeAddress(t), freeAddress(t)
	tests := []struct {
		name    string
		clients []map[string]string
		key     string
	}{
		{name: "empty clients enters unsupported single-user mode", clients: []map[string]string{}, key: testKey('s')},
		{name: "invalid service key", clients: []map[string]string{{"email": "bootstrap", "password": testKey('b')}}, key: "invalid"},
		{name: "invalid user key", clients: []map[string]string{{"email": "bootstrap", "password": "invalid"}}, key: testKey('s')},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := runtimeConfig(apiAddress, inboundAddress, "2022-blake3-aes-256-gcm", test.key, test.clients)
			if err := runConfigCheck(t, bin, config); err == nil {
				t.Fatal("known-invalid SS2022 configuration was accepted")
			}
		})
	}
}
