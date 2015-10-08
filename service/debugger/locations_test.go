package debugger

import (
	"testing"
)

func TestComplexLocationParse(t *testing.T) {
	tests := []string{
		`Load:/\tif ok/+/\tif rule/`,
	}
	for i := range tests {
		_, err := parseLocationSpec(tests[i])
		if err != nil {
			t.Fatalf("Unexpected error parsing locspec '%s': %v", tests[i], err)
		}
	}
}
