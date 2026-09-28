package modelrt

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weights.gguf")
	payload := []byte("not really weights")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	good := hex.EncodeToString(sum[:])

	if err := verifyDigest(path, ""); err != nil {
		t.Fatalf("an unpublished digest must skip the check: %v", err)
	}
	if err := verifyDigest(path, good); err != nil {
		t.Fatalf("matching digest rejected: %v", err)
	}
	if err := verifyDigest(path, strings.Repeat("0", 64)); err == nil {
		t.Fatal("want an error for a mismatched digest")
	}
}

func TestLFSDigest(t *testing.T) {
	good := strings.Repeat("a", 64)
	cases := map[string]string{
		`"` + good + `"`:                    good,
		good:                                good,
		strings.ToUpper(good):               strings.ToLower(good),
		`"short"`:                           "",
		`"` + strings.Repeat("z", 64) + `"`: "",
		"":                                  "",
	}
	for in, want := range cases {
		if got := lfsDigest(in); got != want {
			t.Errorf("lfsDigest(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestOCIDigest(t *testing.T) {
	good := strings.Repeat("b", 64)
	if got := ociDigest("sha256:" + good); got != good {
		t.Errorf("ociDigest=%q, want %q", got, good)
	}
	if got := ociDigest("sha512:" + good); got != "" {
		t.Errorf("non-sha256 digest = %q, want empty", got)
	}
	if got := ociDigest("sha256:abc"); got != "" {
		t.Errorf("short digest = %q, want empty", got)
	}
}
