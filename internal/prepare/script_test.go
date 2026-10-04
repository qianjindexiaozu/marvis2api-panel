package prepare

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const scriptTestKey = "fixture-signing-key"
const scriptTestQIMEI = "0123456789abcdef0123456789abcdef0123"

func runPrepare(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	for _, name := range []string{"bash", "python3"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("prepare script tests require %s", name)
		}
	}
	script, err := filepath.Abs("../../scripts/prepare.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if strings.Contains(string(out), scriptTestKey) || strings.Contains(string(out), scriptTestQIMEI) {
		t.Fatal("prepare must not print key or device values")
	}
	return out, err
}

func clientFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	assets := filepath.Join(dir, "OfflinePack", "main", "current", "assets")
	if err := os.MkdirAll(assets, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assets, "sdk-changed-hash.js"), []byte(`const sdk={marvis_client:"`+scriptTestKey+`"};`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "device-info.cache"), []byte("qimei36="+scriptTestQIMEI+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPrepareScriptExtractsAndImports(t *testing.T) {
	client := clientFixture(t)
	output := filepath.Join(t.TempDir(), "private", "marvis.json")
	out, err := runPrepare(t, "--client-dir", client, "--app-dir", filepath.Join(t.TempDir(), "missing-app"), "--output", output)
	if err != nil {
		t.Fatalf("extract: %v: %s", err, out)
	}
	data, err := Load(output)
	if err != nil || data.AccessKey != scriptTestKey || data.QIMEI36 != scriptTestQIMEI {
		t.Fatalf("invalid extracted data: %v", err)
	}
	for path, mode := range map[string]os.FileMode{filepath.Dir(output): 0o700, output: 0o644} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("unexpected permissions for %s", filepath.Base(path))
		}
	}
	imported := filepath.Join(t.TempDir(), "private", "imported.json")
	out, err = runPrepare(t, "--import", output, "--output", imported)
	if err != nil {
		t.Fatalf("import without App: %v: %s", err, out)
	}
	data2, err := Load(imported)
	if err != nil || data2 != data {
		t.Fatal("import must preserve required data")
	}
}

func TestPrepareScriptFailsClosed(t *testing.T) {
	client := clientFixture(t)
	output := filepath.Join(t.TempDir(), "private", "marvis.json")
	args := []string{"--client-dir", client, "--app-dir", filepath.Join(t.TempDir(), "missing-app"), "--output", output}
	if out, err := runPrepare(t, args...); err != nil {
		t.Fatalf("initial extraction: %v: %s", err, out)
	}
	before, _ := os.ReadFile(output)
	if err := os.Remove(filepath.Join(client, "device-info.cache")); err != nil {
		t.Fatal(err)
	}
	if _, err := runPrepare(t, args...); err == nil {
		t.Fatal("missing registered device data must fail")
	}
	after, _ := os.ReadFile(output)
	if string(after) != string(before) {
		t.Fatal("failed extraction must leave the existing file unchanged")
	}
	assets := filepath.Join(client, "OfflinePack", "main", "current", "assets")
	if err := os.WriteFile(filepath.Join(assets, "another-hash.js"), []byte(`const sdk={"marvis_client":"different-fixture-key"};`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runPrepare(t, args...); err == nil || !strings.Contains(string(out), "多个不同") {
		t.Fatal("ambiguous SDK keys must fail without choosing one")
	}
}

func TestPrepareImportRejectsInvalidDataAndDropsUnrelatedFields(t *testing.T) {
	input := filepath.Join(t.TempDir(), "input.json")
	output := filepath.Join(t.TempDir(), "private", "output.json")
	if err := os.WriteFile(input, []byte(`{"access_key":"`+scriptTestKey+`","qimei36":"uuid"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runPrepare(t, "--import", input, "--output", output); err == nil {
		t.Fatal("invalid imported data must fail")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("failed import must not create a file")
	}
	if err := os.WriteFile(input, []byte(`{"access_key":"`+scriptTestKey+`","qimei36":"`+scriptTestQIMEI+`","access_token":"not-exported"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runPrepare(t, "--import", input, "--output", output); err != nil {
		t.Fatalf("import: %v: %s", err, out)
	}
	raw, _ := os.ReadFile(output)
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil || len(data) != 2 {
		t.Fatal("prepare must contain only the signing key and device id")
	}
}

func TestPrepareRefusesSharedOutputDirectory(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(parent, "output.json")
	if _, err := runPrepare(t, "--client-dir", clientFixture(t), "--output", output); err == nil {
		t.Fatal("world-accessible output directory must be rejected")
	}
}
