package bd

import (
	"strings"
	"testing"
)

// A tracker's config.yaml is data a snapshot carries; only a setting that
// changes where the database lives is refused, and named (fanout-sweep
// code-3: sync.remote refused every --snapshots run). It is read without
// bd, and YAML the reader cannot resolve where a storage or remote setting
// could be is refused (bd config review round 2).
func TestConfigStorageSetting(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"empty", "", ""},
		{"comments", "# all defaults\n\n", ""},
		{"sync remote", "sync:\n    remote: \"git+https://github.com/x/y.git\"\n", ""},
		{"dotted sync remote", "sync.remote: /var/runs/p2b\n", ""},
		{"export and lint", "export:\n  auto: true\n  path: issues.jsonl\nlint:\n  task: strict\n", ""},
		{"dolt debug", "dolt:\n  debug: true\n  local-only: true\n", ""},
		{"global off", "global: false\n", ""},
		{"embedded mode", "dolt.mode: embedded\n", ""},
		{"flow list elsewhere", "labels: [a, b]\nexport: {auto: true}\n", ""},
		{"db path", "db: /elsewhere/beads.db\n", "db"},
		{"database name", "database: shared\n", "database"},
		{"global on", "global: true\n", "global"},
		{"no-db", "no-db: true\n", "no-db"},
		{"nested dolt host", "dolt:\n  debug: false\n  host: 10.0.0.2\n", "dolt.host"},
		{"dotted dolt port", "dolt.port: 3307\n", "dolt.port"},
		{"server mode", "dolt:\n  mode: server\n", "dolt.mode"},
		{"upper-case key", "Dolt:\n  Host: 10.0.0.2\n", "dolt.host"},
		{"shared server", "dolt.shared-server: true\n", "dolt.shared-server"},
		{"socket under a comment", "# server\ndolt:\n  # local socket\n  socket: /run/dolt.sock\n", "dolt.socket"},
		{"quoted key", "\"dolt\":\n  'user': root\n", "dolt.user"},
		{"flow dolt", "dolt: {host: 10.0.0.2}\n", "!"},
		{"flow root", "{dolt: {host: 10.0.0.2}}\n", "!"},
		{"alias", "base: &b\n  host: 10.0.0.2\ndolt: *b\n", "!"},
		{"merge key", "base: &b\n  host: 10.0.0.2\ndolt:\n  <<: *b\n", "!"},
		{"tag", "dolt:\n  host: !!str 10.0.0.2\n", "!"},
		{"block scalar remote", "sync:\n  remote: |\n    x\n", "!"},
		{"two documents", "sync:\n  remote: x\n---\ndolt:\n  host: y\n", "!"},
		{"list under dolt", "dolt:\n  - host: x\n", "!"},
		// Fail closed on keys the reader does not decode (review round 3).
		{"byte order mark", "\ufeffdolt:\n  host: 10.0.0.2\n", "dolt.host"},
		{"escaped double-quoted key", "\"dol\\u0074\":\n  host: 10.0.0.2\n", "!"},
		{"escaped key elsewhere", "\"ex\\x70ort\": true\n", "!"},
		{"doubled single quote in a key", "'dolt''': x\n", "!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := configStorageSetting([]byte(tc.yaml))
			if tc.want == "!" {
				if err == nil {
					t.Errorf("configStorageSetting = %q, want a refusal", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("configStorageSetting = %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}

// A fork's restored config.yaml has every remote blanked, and nothing else
// changed (bd config review round 2).
func TestConfigWithoutRemotes(t *testing.T) {
	in := "# keep\nsync:\n  remote: \"git+https://x/y.git\" # the remote\n  mode: auto\nexport:\n  auto: true\nfederation.remote-url: https://z\nlabels: [a]\n"
	out, blanked, err := configWithoutRemotes([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	want := "# keep\nsync:\n  remote: \"\"\n  mode: auto\nexport:\n  auto: true\nfederation.remote-url: \"\"\nlabels: [a]\n"
	if string(out) != want || strings.Join(blanked, ",") != "sync.remote,federation.remote-url" {
		t.Errorf("got %q %v, want %q", out, blanked, want)
	}
}

// A .env line is read as the dotenv grammar reads it: any whitespace after
// export, and a line that cannot be parsed unambiguously refused when it
// could name a bd key (review round 3).
func TestEnvStorageOverride(t *testing.T) {
	for _, tc := range []struct{ name, env, want string }{
		{"plain", "BEADS_DB=/x\n", "BEADS_DB"},
		{"export space", "export BEADS_DB=/x\n", "BEADS_DB"},
		{"export tab", "export\tBEADS_DB=/x\n", "BEADS_DB"},
		{"export spaces", "export   BD_DOLT_HOST: h\n", "BD_DOLT_HOST"},
		{"byte order mark", "\ufeffBEADS_DB=/x\n", "BEADS_DB"},
		{"quoted key", "\"BEADS_DB\"=/x\n", "!"},
		{"no separator", "DOLT_HOST\n", "!"},
		{"odd key", "BD DB=/x\n", "!"},
		{"actor", "BD_ACTOR=me\nexport\tBEADS_ACTOR=me\n", ""},
		{"unrelated", "EDITOR=vi\nexport\tLANG=C\n# BEADS_DB=/x\n", ""},
		{"odd unrelated", "some junk line\n", ""},
	} {
		got := envStorageOverride([]byte(tc.env))
		if tc.want == "!" {
			if got == "" {
				t.Errorf("%s: admitted, want a refusal", tc.name)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
