package filesystem

import (
	"bytes"
	"testing"
)

func TestReadLimitedAcceptsBoundaryAndRejectsLargerInput(t *testing.T) {
	tests := []struct {
		name     string
		size     int
		wantSize int
		wantOver bool
	}{
		{name: "at limit", size: int(DefaultMaxFileSize), wantSize: int(DefaultMaxFileSize), wantOver: false},
		{name: "over limit", size: int(DefaultMaxFileSize) + 1, wantSize: 0, wantOver: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, overLimit, err := readLimited(bytes.NewReader(bytes.Repeat([]byte{'x'}, tt.size)))
			if err != nil {
				t.Fatalf("readLimited() error = %v", err)
			}
			if overLimit != tt.wantOver {
				t.Errorf("readLimited() overLimit = %v, want %v", overLimit, tt.wantOver)
			}
			if len(content) != tt.wantSize {
				t.Errorf("readLimited() content size = %d, want %d", len(content), tt.wantSize)
			}
		})
	}
}
