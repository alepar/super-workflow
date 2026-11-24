package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgyScanSecretsFailsClosed holds the scanner to spec section 4.8: a case per
// token pattern, per separator shape and per file class. A file the scanner cannot
// read as text is never reported clean, and no value is ever printed.
func TestAgyScanSecretsFailsClosed(t *testing.T) {
	agyIsolate(t)
	// Planted values are built at run time so no credential-shaped literal sits in the source.
	val := strings.Repeat("ab12", 8)
	csrf := "ANTIGRAVITY_CSRF_TOKEN"
	ya29 := "ya29." + strings.Repeat("x", 30)
	scan := func(t *testing.T, files map[string][]byte) (int, string) {
		t.Helper()
		dir := t.TempDir()
		for name, body := range files {
			agyWrite(t, filepath.Join(dir, name), body)
		}
		return runScan(t, dir)
	}
	want := func(t *testing.T, files map[string][]byte, code int, named ...string) {
		t.Helper()
		got, out := scan(t, files)
		if got != code {
			t.Errorf("exit %d, out %q: want %d", got, out, code)
		}
		for _, n := range named {
			if !strings.Contains(out, n) {
				t.Errorf("out %q does not name %q", out, n)
			}
		}
		for _, v := range []string{val, ya29, strings.Repeat("y", 30)} {
			if strings.Contains(out, v) {
				t.Errorf("out %q prints a value", out)
			}
		}
	}
	patterns := map[string]string{
		"csrf":            csrf + "=" + val,
		"csrf spaced":     csrf + " = " + val,
		"csrf json":       `{"` + csrf + `" : "` + val + `"}`,
		"gocspx":          "GOCSPX-" + strings.Repeat("ab12", 6),
		"access_token":    `{"access_token": "` + val + `"}`,
		"id_token":        `{"id_token": "` + val + `"}`,
		"client_secret":   `{"client_secret": "` + val + `"}`,
		"escaped json":    `{"content":"{\"access_token\":\"` + val + `\"}"}`,
		"spaced sep":      `{"client_secret" : "` + val + `"}`,
		"escaped csrf":    `{"content":"{\"` + csrf + `\" : \"` + val + `\"}"}`,
		"csrf after text": "export " + csrf + "=" + val + "\n",
	}
	for name, body := range patterns {
		t.Run(name, func(t *testing.T) {
			want(t, map[string][]byte{"f.txt": []byte(body)}, 1, "f.txt")
		})
	}
	t.Run("binary csrf", func(t *testing.T) {
		want(t, map[string][]byte{"b.bin": []byte("\x00\x01" + csrf + "=" + val + "\x00")}, 1, "b.bin")
	})
	t.Run("binary ya29", func(t *testing.T) {
		want(t, map[string][]byte{"b.bin": []byte("\x00" + ya29 + "\x00")}, 1, "b.bin")
	})
	t.Run("binary clean", func(t *testing.T) {
		want(t, map[string][]byte{"b.bin": []byte("\x00\x01plain\x02\x00")}, 0)
	})
	t.Run("unreadable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads every file")
		}
		dir := t.TempDir()
		p := filepath.Join(dir, "locked.txt")
		agyWrite(t, p, []byte("harmless"))
		if err := os.Chmod(p, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
		code, out := runScan(t, dir)
		if code != 3 || !strings.Contains(out, "locked.txt") {
			t.Errorf("exit %d, out %q: want 3, the file named", code, out)
		}
	})
	t.Run("found beats unreadable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads every file")
		}
		dir := t.TempDir()
		agyWrite(t, filepath.Join(dir, "leak.txt"), []byte(csrf+"="+val))
		p := filepath.Join(dir, "locked.txt")
		agyWrite(t, p, []byte("harmless"))
		if err := os.Chmod(p, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
		code, out := runScan(t, dir)
		if code != 1 || !strings.Contains(out, "leak.txt") || !strings.Contains(out, "locked.txt") || strings.Contains(out, val) {
			t.Errorf("exit %d, out %q: want 1, both files named, no value", code, out)
		}
	})
	for name, magic := range map[string]string{
		"gzip": "\x1f\x8b\x08\x00", "zip": "PK\x03\x04", "zstd": "\x28\xb5\x2f\xfd",
	} {
		t.Run("compressed "+name, func(t *testing.T) {
			want(t, map[string][]byte{name + ".dat": []byte(magic + "harmless")}, 3, name+".dat", "compressed")
		})
	}
	t.Run("redacted", func(t *testing.T) {
		want(t, map[string][]byte{"f.txt": []byte(csrf + "=<redacted>")}, 0)
		want(t, map[string][]byte{"f.txt": []byte(csrf + " = <redacted>")}, 0)
	})
	t.Run("fixtures", func(t *testing.T) {
		if code, out := runScan(t, filepath.Join("testdata", "agy")); code != 0 {
			t.Errorf("the agy fixtures must scan clean: %d %s", code, out)
		}
	})
}
