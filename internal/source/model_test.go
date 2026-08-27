package source

import "testing"

func TestReferenceValid(t *testing.T) {
	tests := []struct {
		name      string
		reference Reference
		want      bool
	}{
		{name: "valid range", reference: Reference{Path: "src/app.py", StartLine: 2, EndLine: 5}, want: true},
		{name: "missing path", reference: Reference{StartLine: 2, EndLine: 5}, want: false},
		{name: "zero start", reference: Reference{Path: "src/app.py", StartLine: 0, EndLine: 5}, want: false},
		{name: "reversed range", reference: Reference{Path: "src/app.py", StartLine: 5, EndLine: 2}, want: false},
		{name: "absolute path", reference: Reference{Path: "/etc/passwd", StartLine: 1, EndLine: 1}, want: false},
		{name: "parent traversal", reference: Reference{Path: "../secret", StartLine: 1, EndLine: 1}, want: false},
		{name: "windows parent traversal", reference: Reference{Path: `..\secret`, StartLine: 1, EndLine: 1}, want: false},
		{name: "non canonical path", reference: Reference{Path: "src/../secret", StartLine: 1, EndLine: 1}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.reference.Valid(); got != tt.want {
				t.Fatalf("Reference.Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}
