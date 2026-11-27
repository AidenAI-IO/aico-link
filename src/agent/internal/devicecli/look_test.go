package devicecli

import "testing"

func TestLookOptionsRejectInvalidRegions(t *testing.T) {
	for _, args := range [][]string{
		{"--region", "0", "0", "1001", "1000"},
		{"--region", "0", "0", "0", "1000"},
		{"--region", "500", "0", "100", "1000"},
		{"--region", "0", "0", "1.5", "1000"},
		{"--region", "0", "0", "500"},
	} {
		if _, err := Calls(append([]string{"look"}, args...)); err == nil {
			t.Errorf("accepted invalid options: %v", args)
		}
	}
}
