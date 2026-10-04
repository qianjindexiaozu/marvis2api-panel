package prepare

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prepare.json")
	const qimei = "0123456789ABCDEF0123456789ABCDEF0123"
	if err := os.WriteFile(path, []byte(`{"access_key":" test-only-key ","qimei36":"`+qimei+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := Load(path)
	if err != nil || data.AccessKey != "test-only-key" || data.QIMEI36 != strings.ToLower(qimei) {
		t.Fatalf("cannot load valid prepare data: %v", err)
	}
}

func TestLoadRejectsInvalidDataWithoutLeakingValues(t *testing.T) {
	const secret = "test-secret-never-in-errors"
	valid := strings.Repeat("a", 36)
	cases := map[string][]byte{
		"invalid JSON": []byte(`{"access_key":"` + secret),
		"null":         []byte(`null`),
		"array":        []byte(`[]`),
		"missing key":  []byte(`{"qimei36":"` + valid + `"}`),
		"UUID":         []byte(`{"access_key":"` + secret + `","qimei36":"12345678-1234-1234-1234-123456789abc"}`),
		"long qimei":   []byte(`{"access_key":"` + secret + `","qimei36":"` + valid + `aa"}`),
		"wrong type":   []byte(`{"access_key":123,"qimei36":"` + valid + `"}`),
		"large file":   []byte(strings.Repeat(" ", maxFileSize+1)),
	}
	control, _ := json.Marshal(Data{AccessKey: secret + "\ninvalid", QIMEI36: valid})
	cases["control character"] = control
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "prepare.json")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if data, err := Load(path); err == nil || data != (Data{}) {
				t.Fatal("invalid prepare data must be rejected")
			} else if strings.Contains(err.Error(), secret) {
				t.Fatal("error must not include the supplied secret")
			}
		})
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file must fail")
	}
}
