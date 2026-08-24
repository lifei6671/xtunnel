package bootstrap

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestValidateWebAssets(t *testing.T) {
	tests := []struct {
		name      string
		assets    fstest.MapFS
		wantError string
	}{
		{
			name: "valid index",
			assets: fstest.MapFS{
				"dist/index.html": {Data: []byte("<!doctype html>")},
			},
		},
		{
			name:      "missing index",
			assets:    fstest.MapFS{},
			wantError: "read dist/index.html",
		},
		{
			name: "empty index",
			assets: fstest.MapFS{
				"dist/index.html": {Data: []byte(" \n\t")},
			},
			wantError: "dist/index.html is empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateWebAssets(test.assets)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("validate assets: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("validate assets error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}
