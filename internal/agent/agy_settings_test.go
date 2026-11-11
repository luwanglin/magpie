package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
)

func TestAgyWithoutSettingsFlag(t *testing.T) {
	prev := AgySettingsFlag
	AgySettingsFlag = func(string) bool { return false }
	t.Cleanup(func() { AgySettingsFlag = prev })

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	if err := provider.Save(provider.Provider{ID: "deepseek", Name: "DeepSeek", Chat: "https://api.deepseek.com/v1", Key: "k", Models: []string{"pro"}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := "{\n  \"model\": \"mine\",\n  \"modelProvider\": \"vertex\",\n  \"trustedWorkspaces\": [\"/w\"]\n}\n"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Find("agy")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply("model", "magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	if got := agyRead(t, path); got != orig {
		t.Fatalf("settings.json written while agy isn't running:\n%s", got)
	}
	if strings.Contains(a.Launch(), "--settings") || !strings.Contains(a.Launch(), "magpie agy run --model 'magpie/deepseek/pro'") {
		t.Fatalf("launch: %q", a.Launch())
	}
	if !strings.Contains(a.Launch(), "GEMINI_API_KEY="+gateway.TokenFor("agy")) {
		t.Fatalf("launch key: %q", a.Launch())
	}
	s := a.Start()
	if s.Model != "magpie/deepseek/pro" || len(s.Args) != 0 || s.Hold == nil {
		t.Fatalf("start: %+v", s)
	}
	over := filepath.Join(filepath.Dir(path), "magpie-settings.json")
	if _, err := os.Stat(agyAppliedPath(over)); err != nil {
		t.Fatal("applied file:", err)
	}

	release, err := s.Hold()
	if err != nil {
		t.Fatal(err)
	}
	held := agyRead(t, path)
	var m map[string]any
	if err := json.Unmarshal([]byte(held), &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "magpie/deepseek/pro" || m["modelProvider"] != "gemini" || !strings.Contains(held, "/w") {
		t.Fatalf("held:\n%s", held)
	}
	release()
	if got := agyRead(t, path); got != orig {
		t.Fatalf("not restored:\n%s", got)
	}
	if s.Plained("--remote-control") != true {
		t.Fatal("remote control")
	}
}

func TestAgyHoldPutsBackADeadHolder(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "settings.json")
	over := filepath.Join(dir, "magpie-settings.json")
	orig := []byte("{\n  \"model\": \"mine\",\n  \"modelProvider\": \"vertex\",\n  \"trustedWorkspaces\": [\"/w\"]\n}\n")
	if err := os.WriteFile(user, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(over, []byte(`{"model":"magpie/deepseek/pro","modelProvider":"gemini"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	release, err := agyHold(user, over)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(user+".magpie-holders", strconv.Itoa(os.Getpid()))); err != nil {
		t.Fatal(err)
	}
	again, err := agyHold(user, over)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(agyRead(t, user)), &m); err != nil {
		t.Fatal(err)
	}
	if m["modelProvider"] != "gemini" || !strings.Contains(agyRead(t, user), "/w") {
		t.Fatalf("after a dead holder:\n%s", agyRead(t, user))
	}
	again()
	if got := agyRead(t, user); got != string(orig) {
		t.Fatalf("restored:\n%s", got)
	}
	release()
	if got := agyRead(t, user); got != string(orig) {
		t.Fatalf("second release:\n%s", got)
	}
}

// A distro has no magpie. When agy --help is a usage line without
// --settings, the function copies magpie's applied settings over for the
// process and puts the user's file back. A stand-in that only echoes keeps
// --settings, which TestAgyFunctionInEveryPOSIXShell covers.
func TestAgyDistroHoldsSettingsWhenHelpHasNoFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the distro's shells")
	}
	sh, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	home := t.TempDir()
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(dir, "settings.json")
	orig := "{\n  \"model\": \"mine\",\n  \"modelProvider\": \"vertex\",\n  \"trustedWorkspaces\": [\"/w\"]\n}\n"
	if err := os.WriteFile(user, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "magpie-settings.json"), []byte("{\n  \"model\": \"magpie/relay/glm-4.6\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	applied := "{\n  \"model\": \"magpie/relay/glm-4.6\",\n  \"modelProvider\": \"gemini\",\n  \"trustedWorkspaces\": [\"/w\"]\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "magpie-settings.applied.json"), []byte(applied), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/sh
if [ "$1" = "--help" ]; then
  echo "Usage of agy:"
  exit 0
fi
prov=$(sed -n 's/.*"modelProvider"[^"]*"\([^"]*\)".*/\1/p' "$HOME/.gemini/antigravity-cli/settings.json" | head -n 1)
echo "prov=$prov args=$*"
`
	if err := os.WriteFile(filepath.Join(bin, "agy"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(t.TempDir(), "profile")
	body := agyDistroFunction("posix", "k'1", "http://172.20.0.1:3425")
	if err := os.WriteFile(profile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, "-c", `. "$1" && agy -p hi`, "bash", profile)
	cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s\nfunction:\n%s", err, out, body)
	}
	got := strings.TrimSpace(string(out))
	if got != "prov=gemini args=--model magpie/relay/glm-4.6 -p hi" {
		t.Fatalf("got %q\nfunction:\n%s", got, body)
	}
	if b, _ := os.ReadFile(user); string(b) != orig {
		t.Fatalf("not restored:\n%s", b)
	}
}
